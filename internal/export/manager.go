package export

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/vfs"

	"github.com/google/uuid"
)

// Backoff for a source that cannot be reached. The transfer is deferred, not
// failed, and the deferral does not spend the retry budget: a pool whose
// members are offline for an afternoon must not fail every file planned that
// day (the same rule internal/upload applies to a queued write).
const (
	unavailableRetry    = 30 * time.Second
	unavailableRetryMax = time.Hour
	// riskControlRetry is the hard back-off a ban signal earns.
	riskControlRetry = 30 * time.Minute
	// maxAttempts is how many times content that arrived wrong, or a local
	// failure that is not the disk, is tried again before the file is failed
	// and the job moves on.
	maxAttempts = 3
)

// Options configures a Manager.
type Options struct {
	Store *Store
	// FS is the filesystem core the sources live in. The export reads its
	// metadata store and its mounts' providers directly; it never calls
	// FS.Read, which would count as foreground IO and would fill the block
	// cache with bytes nobody will ask for again.
	FS     *vfs.FS
	Config config.Export
	Now    func() time.Time
}

// Manager owns exports.db and the goroutine that drains it. It is shaped like
// vfs.FS.StartCopies: one loop, a ticker, and a wake channel, so a newly
// created job starts without waiting for the next tick.
type Manager struct {
	store *Store
	fs    *vfs.FS
	cfg   config.Export
	now   func() time.Time

	busy atomic.Pointer[func() bool]
	// writeFault is a test seam at the destination write, where an external
	// drive fails in ways no unit test can arrange for real.
	writeFault func(off int64) error

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	closed  bool
	wake    chan struct{}
	probes  map[string]time.Time
	rates   map[string]*rateMeter
	running map[string]context.CancelFunc
}

// New builds a manager over an open store.
func New(opt Options) (*Manager, error) {
	if opt.Store == nil {
		return nil, errors.New("export: a Store is required")
	}
	if opt.FS == nil {
		return nil, errors.New("export: an FS is required")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Config == (config.Export{}) {
		// A caller that did not go through config.Default() gets the
		// built-in settings rather than a manager that yields to nothing
		// and transfers one file at a time.
		opt.Config = config.DefaultExport()
	}
	if err := opt.Config.Validate(); err != nil {
		return nil, err
	}
	m := &Manager{
		store: opt.Store, fs: opt.FS, cfg: opt.Config, now: opt.Now,
		wake:    make(chan struct{}, 1),
		probes:  map[string]time.Time{},
		rates:   map[string]*rateMeter{},
		running: map[string]context.CancelFunc{},
	}
	if opt.Store.Owner() {
		// Whatever the last run had in flight goes back on the queue. Its
		// bytes and its bitmap stay: they are on the destination disk.
		if err := opt.Store.ResetActive(context.Background()); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// SetBusy installs the foreground-IO probe, the way cache.SetBusy is wired.
func (m *Manager) SetBusy(f func() bool) { m.busy.Store(&f) }

func (m *Manager) busyFn() func() bool {
	if p := m.busy.Load(); p != nil {
		return *p
	}
	return nil
}

// Store exposes the durable plan for the control plane's read-only views.
func (m *Manager) Store() *Store { return m.store }

// Start runs the drain loop until ctx ends or Stop is called.
func (m *Manager) Start(ctx context.Context, interval time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.cancel != nil || !m.store.Owner() {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ctx, m.cancel = context.WithCancel(ctx)
	wake := m.wake
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			_ = m.RunOnce(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-wake:
			}
		}
	}()
}

// Stop ends the loop and every transfer it has in flight. Their last durable
// checkpoint is kept, so a restart resumes from it.
func (m *Manager) Stop() {
	m.mu.Lock()
	m.closed = true
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	for _, c := range m.running {
		c()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// Close stops the manager and releases the store.
func (m *Manager) Close() error {
	m.Stop()
	return m.store.Close()
}

// Wake asks the loop to look at the queue now.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Create validates a request, records the job, and wakes the loop to plan it.
// The refusals that a person has to see immediately — a bad path, a
// destination that cannot be written, a mirror without a marker — happen
// here, synchronously, rather than inside the planner.
func (m *Manager) Create(ctx context.Context, req Request) (Job, error) {
	if !m.store.Owner() {
		return Job{}, ErrNotOwner
	}
	sources, err := canonicalSources(req.Sources)
	if err != nil {
		return Job{}, err
	}
	dest, err := filepath.Abs(config.ExpandHome(req.Dest))
	if err != nil {
		return Job{}, fmt.Errorf("%w: destination: %v", ErrInvalid, err)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return Job{}, fmt.Errorf("%w: destination: %v", ErrInvalid, err)
	}
	st, err := os.Stat(dest)
	if err != nil {
		return Job{}, fmt.Errorf("%w: destination: %v", ErrInvalid, err)
	}
	dev, _ := deviceOf(st)

	identity, err := m.fs.Meta().Identity(ctx)
	if err != nil {
		return Job{}, err
	}
	bindings, err := m.bindingsFor(sources)
	if err != nil {
		return Job{}, err
	}
	if req.Mirror {
		mk, err := readMarker(dest)
		if err != nil || !sameSources(mk.Sources, sources) {
			return Job{}, ErrMirrorMarkerMissing
		}
	}
	now := m.now()
	job := Job{
		ID: uuid.NewString(), State: StatePlanning, Sources: sources, Dest: dest, DestDev: dev,
		MetaIdentity: identity, Bindings: bindings, CreatedAt: now, UpdatedAt: now,
		Options: JobOptions{
			Mirror: req.Mirror, Verify: req.Verify, PreserveMTime: true,
			Transfers:     pick(req.Transfers, m.cfg.Transfers),
			Streams:       pick(req.Streams, m.cfg.Streams),
			RangeSize:     pick64(req.RangeSize, int64(m.cfg.RangeSize)),
			MultiRangeMin: int64(m.cfg.MultiRangeMin),
		},
	}
	if err := m.store.CreateJob(ctx, job); err != nil {
		return Job{}, err
	}
	m.Wake()
	return job, nil
}

func pick(v, dflt int) int {
	if v > 0 {
		return v
	}
	return dflt
}

func pick64(v, dflt int64) int64 {
	if v > 0 {
		return v
	}
	return dflt
}

// canonicalSources rejects anything that is not a canonical absolute virtual
// path, the way every other path-taking entry point in the tree does.
func canonicalSources(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("%w: no sources", ErrInvalid)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, p := range in {
		if !strings.HasPrefix(p, "/") || path.Clean(p) != p || len(p) > 4096 || strings.ContainsAny(p, "\x00\\") {
			return nil, fmt.Errorf("%w: %q is not a canonical absolute virtual path", ErrInvalid, p)
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	// Two sources with the same base name would land on the same relative
	// path and overwrite each other.
	roots := map[string]string{}
	for _, p := range out {
		r := relRoot(p)
		if prev, ok := roots[r]; ok {
			return nil, fmt.Errorf("%w: %s and %s would both export to %q", ErrInvalid, prev, p, r)
		}
		roots[r] = p
	}
	return out, nil
}

// bindingsFor records which mount each source came from.
func (m *Manager) bindingsFor(sources []string) ([]Binding, error) {
	seen := map[string]bool{}
	var out []Binding
	for _, s := range sources {
		mt, ok := m.mountFor(s)
		if !ok {
			return nil, fmt.Errorf("%w: %s is not under any mount", ErrInvalid, s)
		}
		if seen[mt.Prefix] {
			continue
		}
		seen[mt.Prefix] = true
		out = append(out, Binding{Prefix: mt.Prefix, Remote: mt.Remote, RootID: mt.RootID, AccountBinding: mt.AccountBinding})
	}
	return out, nil
}
