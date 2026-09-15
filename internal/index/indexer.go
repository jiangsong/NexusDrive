package index

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/embed"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/textract"
	"cloudfs/internal/vfs"
)

// The indexer keeps index.db in step with the tree (docs/agent-roadmap.md
// §3, TODO.md T-37). It has two sources of work: the VFS change feed, which
// names files as they are written, renamed or removed, and reconcile passes
// over the metadata store, which catch whatever the feed missed (a restart,
// a queue overflow, a rule added later). Both only queue; a single worker
// extracts, so the index never competes with itself for the remote.
//
// Two selection modes can be on at once. Pinned mode indexes files a pin
// covers once the cache holds every block of them, reading from the cache
// alone, so it costs the remote nothing. Rules mode fetches the files the
// rules select through the ordinary read path, metered by the hourly fetch
// budget, and sleeps out risk control instead of retrying into it.

const (
	defaultStartDelay     = 30 * time.Second
	defaultReconcileEvery = 10 * time.Minute
	defaultYieldMax       = 5 * time.Second
	defaultRiskSleep      = 15 * time.Minute
	// busyPoll is how often a yielding worker looks at FS.Busy again.
	busyPoll = 50 * time.Millisecond
	// pendingBatch is how many queued files one turn of the worker takes
	// before it lets a synchronous ReconcileNow interleave.
	pendingBatch = 8
	// readPiece bounds one ReadFileRange call while a file is fetched.
	readPiece = 4 << 20
	// headBytes is what KindOf sees of a file.
	headBytes = 8 << 10
)

// Pause reasons reported in Progress.Paused.
const (
	PausedBusy        = "busy"
	PausedRiskControl = "risk_control"
	PausedBudget      = "budget"
	PausedTextBudget  = "text_budget"
)

// Options configures an Indexer. FS, Store and Config are required; the
// rest have defaults.
type Options struct {
	FS     *vfs.FS
	Store  *Store
	Config config.Index
	// Unofficial reports whether a remote is reached through an unofficial
	// API (provider.Caps.Tier == "unofficial"), which halves its fetch
	// budget. Nil derives it from the mounts' providers.
	Unofficial func(remote string) bool
	Now        func() time.Time
	// StartDelay is how long Start waits before the first reconcile pass.
	// Default 30s.
	StartDelay time.Duration
	// ReconcileEvery is the period of the local reconcile pass. Default 10m.
	ReconcileEvery time.Duration
	// YieldMax bounds how long the worker stands aside for foreground IO
	// before extracting one file anyway. Default 5s.
	YieldMax time.Duration
	// RiskSleep is how long the worker sleeps after provider.ErrRiskControl.
	// Default 15m.
	RiskSleep time.Duration
	// Embedder turns chunk text into vectors for semantic search
	// (embed_worker.go, hybrid.go). Nil keeps the index keyword-only:
	// nothing is queued for embedding and hybrid/vector searches degrade.
	Embedder embed.Embedder
}

// Progress is what index_status and the console show. The counters are
// cumulative for the life of the process.
type Progress struct {
	Running bool `json:"running"`
	// Extracting is how many files are being extracted right now (0 or 1).
	Extracting int `json:"extracting"`
	// Pending is the queue length after the last change to it.
	Pending   int   `json:"pending"`
	Extracted int64 `json:"extracted"`
	// Skipped counts files whose text at a new version hashed the same, or
	// that were queued but no longer selected.
	Skipped int64 `json:"skipped"`
	Failed  int64 `json:"failed"`
	Yields  int64 `json:"yields"`
	// Paused is "" | busy | risk_control | budget | text_budget.
	Paused   string    `json:"paused,omitempty"`
	ResumeAt time.Time `json:"resume_at,omitzero"`
	// LastReconcile is the end of the last full pass in this process.
	LastReconcile time.Time `json:"last_reconcile,omitzero"`
}

// ReconcileReport counts what one ReconcileNow did.
type ReconcileReport struct {
	Walked, Queued, Extracted, Skipped, Failed, Deleted, Renamed int
}

// errPaused ends a drain because the worker must wait (risk control, a
// spent budget, a full index). It never reaches a caller.
var errPaused = errors.New("index: paused")

// errNotCached says a pinned file lost blocks between the walk and the
// worker; the next reconcile pass will queue it again.
var errNotCached = errors.New("index: pinned file is no longer complete in the cache")

// Indexer is the background worker over one Store and one FS.
type Indexer struct {
	opt        Options
	fs         *vfs.FS
	store      *Store
	budget     *Budget
	unofficial func(remote string) bool
	now        func() time.Time
	extract    textract.Options

	// matcher is the current rule set; pinMatcher applies the built-in
	// include patterns and size limit to pinned files, so a pinned video
	// is not queued only to fail as unsupported.
	matcher    atomic.Pointer[Matcher]
	pinMatcher *Matcher

	// runMu serialises passes over the tree and turns of the queue, so a
	// synchronous ReconcileNow and the worker never extract the same file
	// at once and a full pass's DeleteMissing never races an upsert.
	runMu sync.Mutex

	progMu   sync.Mutex
	prog     Progress
	watchers map[chan Progress]struct{}
	yields   atomic.Int64
	// fetched counts the bytes readFile took from remotes in this process,
	// what cloudfs_index_fetch_bytes_total reports.
	fetched atomic.Int64

	lifeMu sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	// goroutines is how many Start began and Close waits for.
	goroutines int
	wake       chan struct{}
	full       atomic.Bool
	stopWatch  func()

	// embedder and emb are the embedding side (embed_worker.go); embedder
	// is nil for a keyword-only index.
	embedder embed.Embedder
	emb      embedState
}

// New binds the index to the FS. It records the meta store identity (a
// rebuilt meta store empties the index), mirrors the configured rules into
// the store and builds the matcher. It starts nothing.
func New(opt Options) (*Indexer, error) {
	if opt.FS == nil || opt.Store == nil {
		return nil, errors.New("index: an Indexer needs an FS and a Store")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.StartDelay <= 0 {
		opt.StartDelay = defaultStartDelay
	}
	if opt.ReconcileEvery <= 0 {
		opt.ReconcileEvery = defaultReconcileEvery
	}
	if opt.YieldMax <= 0 {
		opt.YieldMax = defaultYieldMax
	}
	if opt.RiskSleep <= 0 {
		opt.RiskSleep = defaultRiskSleep
	}
	x := &Indexer{
		opt: opt, fs: opt.FS, store: opt.Store, now: opt.Now,
		budget:   NewBudget(opt.Config.FetchBudgetPerHour, opt.Now),
		extract:  textract.DefaultOptions(),
		watchers: map[chan Progress]struct{}{},
		wake:     make(chan struct{}, 1),
		embedder: opt.Embedder,
		emb:      embedState{wake: make(chan struct{}, 1)},
	}
	if opt.Embedder != nil {
		// Chunks are queued for embedding only while something will
		// drain the queue, and never past max_chunks.
		opt.Store.SetEmbedCap(opt.Config.MaxChunks)
	}
	if opt.Config.MaxTextBytes > 0 {
		x.extract.MaxTextBytes = int64(opt.Config.MaxTextBytes)
	}
	x.unofficial = opt.Unofficial
	if x.unofficial == nil {
		tiers := map[string]bool{}
		for _, m := range opt.FS.Mounts() {
			if m.Provider != nil {
				tiers[m.Remote] = m.Provider.Capabilities().Tier == provider.TierUnofficial
			}
		}
		x.unofficial = func(remote string) bool { return tiers[remote] }
	}
	x.pinMatcher = NewMatcher([]Rule{{Path: "/"}}, opt.Config.Exclude)
	ctx := context.Background()
	id, err := opt.FS.Meta().Identity(ctx)
	if err != nil {
		return nil, fmt.Errorf("index: meta identity: %w", err)
	}
	if _, err := opt.Store.EnsureIdentity(ctx, id); err != nil {
		return nil, err
	}
	if err := opt.Store.SyncConfigRules(ctx, RulesFromConfig(opt.Config.Rules)); err != nil {
		return nil, err
	}
	if err := x.ReloadRules(ctx); err != nil {
		return nil, err
	}
	return x, nil
}

// Store returns the index store.
func (x *Indexer) Store() *Store { return x.store }

// Embedder returns the configured embedder, nil for a keyword-only index.
func (x *Indexer) Embedder() embed.Embedder { return x.embedder }

// Yields reports how often the worker stood aside for foreground IO.
func (x *Indexer) Yields() int64 { return x.yields.Load() }

// ReloadRules rebuilds the matcher from the store's rule table (after a rule
// was added or removed at run time) and asks for a full pass.
func (x *Indexer) ReloadRules(ctx context.Context) error {
	rules, err := x.store.Rules(ctx)
	if err != nil {
		return err
	}
	x.matcher.Store(NewMatcher(rules, x.opt.Config.Exclude))
	x.full.Store(true)
	x.kick()
	return nil
}

// Start runs the change consumer, the worker and the periodic reconcile
// until ctx ends or Close is called. Calling it twice is a no-op.
func (x *Indexer) Start(ctx context.Context) {
	x.lifeMu.Lock()
	defer x.lifeMu.Unlock()
	if x.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	x.cancel = cancel
	x.done = make(chan struct{}, 3)
	ch, stop := x.fs.WatchChanges()
	x.stopWatch = stop
	x.update(func(p *Progress) { p.Running = true })
	go x.consume(ctx, ch)
	go x.work(ctx)
	x.goroutines = 2
	if x.embedder != nil {
		go x.embedWork(ctx)
		x.goroutines++
		x.kickEmbed()
	}
}

// Close stops the goroutines Start began and waits for them. The store is
// left open for its owner to close.
func (x *Indexer) Close() error {
	x.lifeMu.Lock()
	defer x.lifeMu.Unlock()
	if x.cancel == nil {
		return nil
	}
	x.cancel()
	x.stopWatch()
	for range x.goroutines {
		<-x.done
	}
	x.cancel, x.stopWatch = nil, nil
	x.update(func(p *Progress) { p.Running = false })
	return nil
}

// Kick asks the worker for a full pass now, instead of at the next tick.
func (x *Indexer) Kick() {
	x.full.Store(true)
	x.kick()
}

// kick wakes the worker without blocking.
func (x *Indexer) kick() {
	select {
	case x.wake <- struct{}{}:
	default:
	}
}

// Progress returns a snapshot of the counters.
func (x *Indexer) Progress() Progress {
	x.progMu.Lock()
	defer x.progMu.Unlock()
	return x.prog
}

// Watch returns a channel that receives a snapshot after every change to
// the progress, and a function that ends the subscription. A subscriber
// that falls behind misses snapshots rather than slowing the worker.
func (x *Indexer) Watch() (<-chan Progress, func()) {
	ch := make(chan Progress, 16)
	x.progMu.Lock()
	x.watchers[ch] = struct{}{}
	x.progMu.Unlock()
	return ch, func() {
		x.progMu.Lock()
		defer x.progMu.Unlock()
		if _, ok := x.watchers[ch]; ok {
			delete(x.watchers, ch)
			close(ch)
		}
	}
}

func (x *Indexer) update(fn func(p *Progress)) {
	x.progMu.Lock()
	defer x.progMu.Unlock()
	fn(&x.prog)
	for ch := range x.watchers {
		select {
		case ch <- x.prog:
		default:
		}
	}
}

// pause records why the worker stops and, when known, when it may go on.
func (x *Indexer) pause(why string, resumeAt time.Time) {
	x.update(func(p *Progress) { p.Paused, p.ResumeAt = why, resumeAt })
}

// pausedUntil reports the current pause; a timed pause whose ResumeAt has
// passed is cleared here.
func (x *Indexer) pausedUntil() (why string, resumeAt time.Time) {
	x.progMu.Lock()
	defer x.progMu.Unlock()
	if x.prog.Paused == "" || x.prog.Paused == PausedBusy {
		return "", time.Time{}
	}
	if !x.prog.ResumeAt.IsZero() && !x.now().Before(x.prog.ResumeAt) {
		x.prog.Paused, x.prog.ResumeAt = "", time.Time{}
		return "", time.Time{}
	}
	return x.prog.Paused, x.prog.ResumeAt
}

// consume turns change events into queue entries. It never extracts and
// never calls the provider: a path it cannot resolve in meta is treated as
// gone, and anything larger than a file (a rescan hint, a vanished
// directory that held documents) becomes a request for a full pass.
func (x *Indexer) consume(ctx context.Context, ch <-chan vfs.Change) {
	defer func() { x.done <- struct{}{} }()
	for {
		select {
		case <-ctx.Done():
			return
		case c, ok := <-ch:
			if !ok {
				return
			}
			x.handleChange(ctx, c)
			x.kick()
		}
	}
}

func (x *Indexer) handleChange(ctx context.Context, c vfs.Change) {
	if c.Rescan {
		x.full.Store(true)
		return
	}
	if c.Subtree && len(c.Paths) == 2 {
		// A rename is reported as its two ends. Moving the paths in one
		// UPDATE keeps every document's indexed_at, which a walk would do
		// too, one row at a time.
		from, to := c.Paths[0], c.Paths[1]
		if _, err := x.fs.Meta().Resolve(ctx, from); errors.Is(err, meta.ErrNotFound) {
			if _, err := x.fs.Meta().Resolve(ctx, to); err == nil && from != "/" && to != "/" {
				_, _ = x.store.RenamePrefix(ctx, from, to)
			}
		}
	}
	for _, p := range c.Paths {
		x.handlePath(ctx, p, c.Subtree)
	}
}

func (x *Indexer) handlePath(ctx context.Context, p string, subtree bool) {
	n, err := x.fs.Meta().Resolve(ctx, p)
	if err != nil {
		if !errors.Is(err, meta.ErrNotFound) {
			return
		}
		if d, ok, _ := x.store.DocumentByPath(ctx, p); ok {
			_ = x.store.DeleteDocument(ctx, d.ID)
			return
		}
		if under, _ := x.store.DocumentsUnder(ctx, p); under > 0 {
			x.full.Store(true)
		}
		return
	}
	if n.IsDir() {
		if subtree {
			_, _ = x.reconcile(ctx, []string{p}, false)
		}
		return
	}
	if x.selects(n, p) {
		_ = x.store.Enqueue(ctx, n.Ino, p, PendingChange)
		return
	}
	if d, ok, _ := x.store.DocumentByPath(ctx, p); ok && d.RemoteID == n.RemoteID {
		// Still there, no longer selected (it grew past the rule's size).
		_ = x.store.DeleteDocument(ctx, d.ID)
	}
}

// selects reports whether the file at p is to be indexed under either mode.
func (x *Indexer) selects(n meta.Node, p string) bool {
	if x.pinnedSelects(n, p) {
		return true
	}
	_, ok := x.matcher.Load().Match(p, n.Size)
	return ok
}

// pinnedSelects is pinned mode's test: covered by a pin, of an indexable
// kind and size, and complete in the cache.
func (x *Indexer) pinnedSelects(n meta.Node, p string) bool {
	if !x.opt.Config.Pinned || !x.pinCovers(p) {
		return false
	}
	if _, ok := x.pinMatcher.Match(p, n.Size); !ok {
		return false
	}
	return x.fs.Cache().Complete(fileKey(n))
}

// pinCovers mirrors the pin rules' coverage: a pin names a path, and a
// recursive one covers everything below it.
func (x *Indexer) pinCovers(p string) bool {
	for _, pin := range x.fs.PinPolicies() {
		if p == pin.Path || pin.Recursive && (pin.Path == "/" || strings.HasPrefix(p, strings.TrimSuffix(pin.Path, "/")+"/")) {
			return true
		}
	}
	return false
}

func fileKey(n meta.Node) cache.FileKey {
	return cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
}

// work is the single extraction goroutine. It runs a full pass at
// StartDelay and every ReconcileEvery, drains the queue whenever woken, and
// sleeps out a timed pause instead of polling through it.
func (x *Indexer) work(ctx context.Context) {
	defer func() { x.done <- struct{}{} }()
	start := time.NewTimer(x.opt.StartDelay)
	defer start.Stop()
	tick := time.NewTicker(x.opt.ReconcileEvery)
	defer tick.Stop()
	var resume *time.Timer
	var resumeC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			if resume != nil {
				resume.Stop()
			}
			return
		case <-start.C:
			x.full.Store(true)
		case <-tick.C:
			x.full.Store(true)
		case <-resumeC:
		case <-x.wake:
		}
		if resume != nil {
			resume.Stop()
			resume, resumeC = nil, nil
		}
		x.turn(ctx, nil)
		if why, at := x.pausedUntil(); why != "" && !at.IsZero() {
			resume = time.NewTimer(at.Sub(x.now()))
			resumeC = resume.C
		}
	}
}

// turn runs a full pass when one is requested and then drains the queue
// until it is empty or the worker must pause.
func (x *Indexer) turn(ctx context.Context, rep *ReconcileReport) {
	if x.full.Swap(false) {
		if r, err := x.reconcile(ctx, nil, true); err == nil {
			if rep != nil {
				rep.Walked, rep.Queued, rep.Deleted, rep.Renamed = r.Walked, r.Queued, r.Deleted, r.Renamed
			}
		} else if ctx.Err() == nil {
			// Try again next time; the queue is still worth draining.
			x.full.Store(true)
		}
	}
	x.drain(ctx, rep)
}

// ReconcileNow runs a full pass and drains the queue on the caller's
// goroutine. It returns when the queue is empty or the worker would pause
// (risk control, budget); Progress says which.
func (x *Indexer) ReconcileNow(ctx context.Context) (ReconcileReport, error) {
	rep, err := x.reconcile(ctx, nil, true)
	if err != nil {
		return rep, err
	}
	x.drain(ctx, &rep)
	return rep, ctx.Err()
}

// reconcile walks roots (nil: every rule root and every pin) through meta
// and queues the files whose indexed version is not the current one. A
// full pass also moves renamed documents and deletes the ones no root
// reaches any more. It never lists a directory from the remote: what meta
// does not know is left to the crawler and the next pass.
func (x *Indexer) reconcile(ctx context.Context, roots []string, full bool) (ReconcileReport, error) {
	x.runMu.Lock()
	defer x.runMu.Unlock()
	var rep ReconcileReport
	m := x.matcher.Load()
	if roots == nil {
		roots = append(roots, m.Roots()...)
		if x.opt.Config.Pinned {
			for _, pin := range x.fs.PinPolicies() {
				roots = append(roots, pin.Path)
			}
		}
	}
	seen := map[int64]bool{}
	visited := map[uint64]bool{}
	visit := func(n meta.Node, p string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n.IsDir() {
			if m.Excluded(p) {
				return meta.SkipDir
			}
			return nil
		}
		if visited[n.Ino] {
			return nil
		}
		visited[n.Ino] = true
		rep.Walked++
		if !x.selects(n, p) {
			return nil
		}
		d, ok, err := x.store.DocumentByRemote(ctx, n.Remote, n.RemoteID)
		if err != nil {
			return err
		}
		if ok {
			seen[d.ID] = true
			if d.Version == n.Version && d.State != DocDirty {
				if d.Path != p {
					if err := x.store.SetPath(ctx, d.ID, p); err != nil {
						return err
					}
					rep.Renamed++
				}
				return nil
			}
		}
		rep.Queued++
		return x.store.Enqueue(ctx, n.Ino, p, PendingReconcile)
	}
	for _, root := range roots {
		n, err := x.rootNode(ctx, root)
		if errors.Is(err, vfs.ErrNotFound) || errors.Is(err, meta.ErrNotFound) {
			continue
		}
		if err != nil {
			return rep, err
		}
		if !n.IsDir() {
			if err := visit(n, root); err != nil && !errors.Is(err, meta.SkipDir) {
				return rep, err
			}
			continue
		}
		if m.Excluded(root) && root != "/" {
			continue
		}
		if err := x.fs.Meta().WalkSubtree(ctx, n.Ino, root, visit); err != nil {
			return rep, err
		}
	}
	if full {
		// Documents queued but not yet extracted keep their row; the
		// walk marked them seen above. Everything else no root reached.
		gone, err := x.store.DeleteMissing(ctx, seen)
		if err != nil {
			return rep, err
		}
		rep.Deleted = int(gone)
		x.update(func(p *Progress) { p.LastReconcile = x.now() })
	}
	x.refreshPending(ctx)
	return rep, nil
}

// rootNode resolves a root. A rule root goes through the FS so a configured
// subtree that was never listed is looked up once; a pin root is in meta
// already or the pin could not have filled.
func (x *Indexer) rootNode(ctx context.Context, root string) (meta.Node, error) {
	if n, err := x.fs.Meta().Resolve(ctx, root); err == nil {
		return n, nil
	}
	a, err := x.fs.StatPath(ctx, root)
	if err != nil {
		return meta.Node{}, err
	}
	return x.fs.Meta().Get(ctx, a.Ino)
}

func (x *Indexer) refreshPending(ctx context.Context) {
	if st, err := x.store.Stats(ctx); err == nil {
		x.update(func(p *Progress) { p.Pending = st.Pending })
	}
}
