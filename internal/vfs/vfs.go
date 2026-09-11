// Package vfs is the filesystem core: it owns the inode tree, resolves paths
// across the mounted remotes, serves reads from the block cache, and applies
// the per-directory consistency mode. FUSE and MCP are both thin adapters over
// it (docs/DESIGN.md §3).
package vfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
)

// Errors returned to adapters, which map them to errno or MCP errors.
var (
	ErrNotFound        = errors.New("vfs: no such file or directory")
	ErrExists          = errors.New("vfs: file exists")
	ErrIsDir           = errors.New("vfs: is a directory")
	ErrNotDir          = errors.New("vfs: not a directory")
	ErrNotEmpty        = errors.New("vfs: directory not empty")
	ErrReadOnly        = errors.New("vfs: read-only mount point")
	ErrNoSpace         = errors.New("vfs: no space left on device")
	ErrCrossMount      = errors.New("vfs: cannot move across remotes")
	ErrUploadCancelled = journal.ErrCancelled
	ErrUploadPurging   = journal.ErrUploadPurging
)

// Mount binds a subtree of the mount point to one remote.
type Mount struct {
	// Prefix is the path inside the mount, e.g. "/work". "/" mounts a remote
	// at the root.
	Prefix string
	// Remote is the name used in metadata and cache keys.
	Remote string
	// Root is the provider-side directory id the prefix maps to.
	RootID string
	// AccountBinding is an opaque local credential generation supplied by
	// daemon assembly. Uploads persist it so a same-named remote cannot receive
	// an older account's queued bytes after reconfiguration.
	AccountBinding string
	Provider       provider.Provider
	Mode           config.Mode
	DirTTL         time.Duration
	Pin            bool
}

// Options configures a VFS.
type Options struct {
	Meta   *meta.Store
	Cache  *cache.Cache
	Mounts []Mount
	// DefaultDirTTL applies when a mount does not set one.
	DefaultDirTTL time.Duration
	// AttrTTL is how long file attributes stay fresh.
	AttrTTL time.Duration
	// NegativeTTL is how long a missing name is remembered.
	NegativeTTL time.Duration
	// ReadAheadBlocks caps the sequential prefetch window (0 disables it).
	ReadAheadBlocks int
	// ReadaheadRequest caps how many contiguous missing blocks a readahead
	// run merges into one range request, in bytes. Zero means "derive it
	// per mount": 16 MiB when the mount's provider allows range reads and
	// recommends 16 QPS or fewer for downloads (a request budget worth
	// spending on fewer, larger requests), otherwise one block. A non-zero
	// value must be a positive multiple of the cache's block size.
	ReadaheadRequest int64
	// PrefetchDepth is how deep readdir warms subdirectories (0 disables it).
	PrefetchDepth int
	// WriteSettle holds a committed write back from the upload queue for this
	// long, so a burst of flushes on one file results in a single upload. The
	// data is already durable locally; this only delays the network send.
	// Zero disables the window.
	WriteSettle time.Duration
	// Now is injectable for tests.
	Now func() time.Time
	// OnInvalidate, when set, is called after the tree changes so the FUSE
	// adapter can notify the kernel.
	OnInvalidate func(ino uint64)
}

// FS is the filesystem core.
type FS struct {
	changeMu           sync.Mutex
	changeWatchers     map[chan Change]struct{}
	hasChangeWatchers  atomic.Bool
	changesClosed      bool
	copyPublishMu      sync.RWMutex       // read-open admission also excludes destructive cleanup
	uploadCleanupFault func(string) error // durable intent / metadata / cache boundaries
	// publishFault is the same kind of seam at the boundaries of publishing
	// an upload's result. Production leaves it nil.
	publishFault        func(string) error
	readStartedFault    func() // test boundary after retaining an active read
	uploadResumeFault   func() // test boundary after checksum verification
	copyCheckpointFault func(journal.CopyJob) error
	copyBindFault       func() error
	copyCleanupFault    func() error // after durable cleanup intent, before unlink
	copyWorkerMu        sync.Mutex
	copyCancel          context.CancelFunc
	copyWG              sync.WaitGroup
	copyClosed          bool
	copyError           string
	// serverCopyError reports a server-side copy whose result is still
	// unknown. It is kept apart from copyError so a preparation failure and an
	// unresolved copy cannot overwrite each other's explanation.
	serverCopyError string
	copyWake        chan struct{}
	copyRunsMu      sync.Mutex
	copyRuns        map[string]context.CancelFunc
	// commitFault, when set by a test, fails a commit after its blob and
	// journal row are in, the way a busy metadata store does.
	commitFault func() error
	meta        *meta.Store
	cache       *cache.Cache
	opt         Options
	now         func() time.Time

	mounts []Mount // longest prefix first
	// mountDirs are the inodes of the mount-prefix directories, which no
	// provider's listing may remove.
	mountDirs map[uint64]bool
	// space caches what the backends report for df.
	space spaceCache

	mu      sync.Mutex
	handles map[uint64]*Handle
	nextFH  uint64
	// Remote publication cannot race a writer's initial node lookup and
	// registration. Never hold this gate during provider directory IO.
	remotePublishMu sync.RWMutex
	writers         map[uint64]int // protected by mu; retained through close commit

	prefetch *prefetcher
	// fgIO counts the reads and writes the kernel is waiting on, so
	// background work can stand aside.
	fgIO     atomic.Int64
	journal  *journal.Journal
	uploader *upload.Uploader

	// singleflight for directory listings and block fetches
	dirFlight flight[uint64, *directoryRefresh]
	// prefetching tracks blocks a read-ahead goroutine already owns.
	prefetching inflight
	blockFlight flight[blockKey, []byte]
	subFlight   flight[subKey, []byte]
	// readaheadDisabled remembers, per mount remote name, that a coalesced
	// multi-block readahead run once came back short on that remote (a
	// server that caps request size below what was asked). Once set,
	// readahead on that remote never coalesces again for the life of the
	// process. Presence is the signal; the value is unused.
	readaheadDisabled sync.Map
	// invalidateFn is the kernel-notification hook. It is installed after
	// the FS is already serving (the mount comes later), so it is read and
	// written atomically rather than through opt.
	invalidateFn      atomic.Pointer[func(ino uint64)]
	invalidateEntryFn atomic.Pointer[func(parent uint64, name string)]
	invalidateAllFn   atomic.Pointer[func()]
	// paths caches ino -> path for MountForIno; see pathOf. dirIDs caches
	// ino -> provider id for directories.
	paths  atomic.Pointer[sync.Map]
	dirIDs atomic.Pointer[sync.Map]

	// pinMu serialises policy changes and their application to cached keys.
	pinMu     sync.Mutex
	pins      []meta.Pin
	hasPins   atomic.Bool
	pinWake   chan struct{}
	pinCancel context.CancelFunc
	pinClosed bool
	pinWG     sync.WaitGroup
	pinError  string
}

// subKey identifies one aligned range of a block for request collapsing.
type subKey struct {
	ino    uint64
	idx    int64
	off, n int64
}

type blockKey struct {
	ino uint64
	idx int64
}

// New builds a VFS. Mount prefixes are normalised and sorted longest first so
// the deepest match wins.
func New(opt Options) (*FS, error) {
	return newFS(opt, false)
}

func newFS(opt Options, cleanupOnly bool) (*FS, error) {
	if opt.Meta == nil || opt.Cache == nil {
		return nil, errors.New("vfs: Meta and Cache are required")
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.DefaultDirTTL <= 0 {
		opt.DefaultDirTTL = 5 * time.Minute
	}
	if opt.AttrTTL <= 0 {
		opt.AttrTTL = 30 * time.Second
	}
	if opt.NegativeTTL <= 0 {
		opt.NegativeTTL = 5 * time.Second
	}
	if opt.WriteSettle < 0 {
		opt.WriteSettle = 0
	}
	if opt.ReadaheadRequest < 0 || (opt.ReadaheadRequest > 0 && opt.ReadaheadRequest%opt.Cache.BlockSize() != 0) {
		return nil, fmt.Errorf("vfs: ReadaheadRequest (%d) must be a positive multiple of the cache block size (%d)",
			opt.ReadaheadRequest, opt.Cache.BlockSize())
	}
	f := &FS{
		meta:    opt.Meta,
		cache:   opt.Cache,
		opt:     opt,
		now:     opt.Now,
		handles: map[uint64]*Handle{},
		nextFH:  1,
		pinWake: make(chan struct{}, 1),
	}
	f.paths.Store(&sync.Map{})
	f.dirIDs.Store(&sync.Map{})
	if opt.OnInvalidate != nil {
		f.SetInvalidate(opt.OnInvalidate)
	}
	for _, m := range opt.Mounts {
		m.Prefix = normalisePrefix(m.Prefix)
		if m.Provider == nil && !cleanupOnly {
			return nil, fmt.Errorf("vfs: mount %s has no provider", m.Prefix)
		}
		if m.Mode == "" {
			m.Mode = config.ModeWriteback
		}
		if m.AccountBinding == "" {
			// Direct library users and tests predating account generations get a
			// deterministic mount-local fallback. Production daemon assembly
			// always supplies the persisted config generation.
			sum := sha256.Sum256([]byte(m.Remote + "\x00" + m.RootID))
			m.AccountBinding = "unmanaged-sha256:" + hex.EncodeToString(sum[:])
		}
		if m.DirTTL <= 0 {
			m.DirTTL = opt.DefaultDirTTL
		}
		f.mounts = append(f.mounts, m)
	}
	sort.Slice(f.mounts, func(i, j int) bool { return len(f.mounts[i].Prefix) > len(f.mounts[j].Prefix) })
	f.prefetch = newPrefetcher(f, opt.PrefetchDepth)
	if cleanupOnly {
		return f, nil // never create mount nodes or restore unrelated pin state
	}
	if err := f.ensureMountDirs(context.Background()); err != nil {
		return nil, err
	}
	if err := f.loadPins(context.Background()); err != nil {
		return nil, err
	}
	// Journal-backed cache references are not ordinary evictable reads. The
	// uploader/discard path releases them after their durable obligation ends.
	for _, key := range f.cache.Keys() {
		if IsLocalOnly(key.RemoteID) {
			f.cache.Pin(key, true)
		}
	}
	return f, nil
}

func normalisePrefix(p string) string {
	p = path.Clean("/" + strings.Trim(p, "/"))
	return p
}

// Close stops background work.
func (f *FS) Close() error {
	f.closeChanges()
	f.StopCopies()
	f.pinMu.Lock()
	f.pinClosed = true
	if f.pinCancel != nil {
		f.pinCancel()
	}
	f.pinMu.Unlock()
	f.pinWG.Wait()
	f.prefetch.stop()
	return nil
}

// Mounts returns the configured mounts, deepest prefix first.
func (f *FS) Mounts() []Mount { return f.mounts }

// Cache exposes the block cache (metrics, pin commands).
func (f *FS) Cache() *cache.Cache { return f.cache }

// Meta exposes the metadata store (search, status).
func (f *FS) Meta() *meta.Store { return f.meta }

// ensureMountDirs creates the intermediate directories that hold the mount
// prefixes, so "/work" and "/gd" exist even before any listing.
func (f *FS) ensureMountDirs(ctx context.Context) error {
	f.mountDirs = map[uint64]bool{}
	for _, m := range f.mounts {
		if m.Prefix == "/" {
			continue
		}
		parent := meta.RootIno
		segs := strings.Split(strings.Trim(m.Prefix, "/"), "/")
		for i, seg := range segs {
			n, err := f.meta.Lookup(ctx, parent, seg)
			if errors.Is(err, meta.ErrNotFound) {
				node := meta.Node{
					ParentIno: parent, Name: seg, Kind: provider.KindDir,
					Mode: 0o755, MTime: f.now(), TTL: 365 * 24 * time.Hour,
				}
				if i == len(segs)-1 {
					node.Remote = m.Remote
					node.RemoteID = m.RootID
				}
				n, err = f.meta.Upsert(ctx, node)
			}
			if err != nil {
				return fmt.Errorf("vfs: create mount dir %s: %w", m.Prefix, err)
			}
			f.mountDirs[n.Ino] = true
			parent = n.Ino
		}
	}
	return nil
}

// isMountDir reports whether ino is a segment of a mount prefix. Such a node
// belongs to the layout, not to any provider's listing: a remote mounted at
// "/" lists the root completely and does not know about /raw, so without this
// the reconciliation removed the prefix directories of the other mounts.
func (f *FS) isMountDir(ino uint64) bool { return f.mountDirs[ino] }

// mountFor returns the mount serving p (the deepest matching prefix).
func (f *FS) mountFor(p string) (Mount, bool) {
	p = path.Clean("/" + strings.Trim(p, "/"))
	for _, m := range f.mounts {
		if m.Prefix == "/" {
			return m, true
		}
		if p == m.Prefix || strings.HasPrefix(p, m.Prefix+"/") {
			return m, true
		}
	}
	return Mount{}, false
}

// MountForIno resolves the mount serving an inode.
func (f *FS) MountForIno(ctx context.Context, ino uint64) (Mount, string, error) {
	p, err := f.pathOf(ctx, ino)
	if err != nil {
		// Only a missing inode is "not found". A cancelled context or a
		// database error reported as ENOENT would be cached by the kernel as
		// a negative entry and turn a transient failure into a missing file.
		if errors.Is(err, meta.ErrNotFound) {
			return Mount{}, "", ErrNotFound
		}
		return Mount{}, "", err
	}
	m, ok := f.mountFor(p)
	if !ok {
		return Mount{}, p, ErrNotFound
	}
	return m, p, nil
}

// isMountRoot reports whether ino is exactly a mount prefix directory.
func (f *FS) isMountRoot(p string) bool {
	for _, m := range f.mounts {
		if m.Prefix == p {
			return true
		}
	}
	return false
}

// Attr is the stat result handed to adapters.
type Attr struct {
	Ino     uint64
	Name    string
	IsDir   bool
	Size    int64
	MTime   time.Time
	Mode    uint32
	Remote  string
	Version string
	// Cached is the fraction of the file present in the block cache, 0..1.
	Cached float64
	// Pinned reports whether the path is covered by a pin.
	Pinned bool
	// LocalOnly is true while the file exists only as a committed local blob
	// waiting in the upload queue.
	LocalOnly bool
}

// attrOf builds the attributes of a node whose virtual path the caller does
// not have. Callers that do — a listing knows the directory it is listing —
// use attrAt, because Pinned is a property of the path and looking each
// child's path up again would cost a query per entry.
func (f *FS) attrOf(ctx context.Context, n meta.Node) Attr {
	return f.attrAt(ctx, n, "")
}

// attrAt is attrOf with the node's virtual path already known; pass "" to
// have it resolved, which only happens when a pin rule exists at all.
func (f *FS) attrAt(ctx context.Context, n meta.Node, p string) Attr {
	a := Attr{
		Ino: n.Ino, Name: n.Name, IsDir: n.IsDir(), Size: n.Size,
		MTime: n.MTime, Mode: n.Mode, Remote: n.Remote, Version: n.Version,
		LocalOnly: IsLocalOnly(n.RemoteID),
	}
	if !n.IsDir() && n.RemoteID != "" {
		have, total := f.cache.Present(cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version})
		if total > 0 {
			a.Cached = float64(have) / float64(total)
		} else if n.Size == 0 {
			a.Cached = 1
		}
	}
	// The field existed before anything set it: every listing reported every
	// file unpinned, which made a "pinned" column in any adapter a lie.
	if f.hasPins.Load() {
		if p == "" {
			p, _ = f.meta.Path(ctx, n.Ino)
		}
		if p != "" {
			a.Pinned = f.pathPinned(p)
		}
	}
	return a
}

// HandleAttr returns the attributes of the node behind an open handle, as
// the handle last saw them. Create and Flush already hold the node, so the
// kernel's reply is built without another query.
func (f *FS) HandleAttr(ctx context.Context, h *Handle) Attr {
	h.mu.Lock()
	n := h.Node
	h.mu.Unlock()
	return f.attrOf(ctx, n)
}

// Stat returns attributes for an inode, refreshing from the provider when the
// cached copy is stale.
func (f *FS) Stat(ctx context.Context, ino uint64) (Attr, error) {
	n, err := f.meta.Get(ctx, ino)
	if errors.Is(err, meta.ErrNotFound) {
		return Attr{}, ErrNotFound
	}
	if err != nil {
		return Attr{}, err
	}
	a := f.attrOf(ctx, n)
	// A file being written has a size the metadata does not know yet. The
	// caller, and through it the kernel, must see what has actually been
	// written, otherwise a program that writes and then reads back through the
	// same handle reads past a stale end of file.
	if size, ok := f.pendingSize(ino); ok && size != a.Size {
		a.Size = size
	}
	return a, nil
}

// pendingSize returns the largest staged size among open write handles for an
// inode, and whether any exist.
func (f *FS) pendingSize(ino uint64) (int64, bool) {
	f.mu.Lock()
	handles := make([]*Handle, 0, len(f.handles))
	for _, h := range f.handles {
		if h.Ino == ino {
			handles = append(handles, h)
		}
	}
	f.mu.Unlock()

	var size int64
	var found bool
	for _, h := range handles {
		h.mu.Lock()
		w := h.writer
		closed := h.closed
		h.mu.Unlock()
		if w == nil || closed || w.staging == nil {
			continue
		}
		if s := w.staging.Size(); s > size || !found {
			size = s
			found = true
		}
	}
	return size, found
}

// StatPath resolves a path and returns its attributes, listing parents as
// needed so an uncached path still resolves.
func (f *FS) StatPath(ctx context.Context, p string) (Attr, error) {
	n, err := f.resolve(ctx, p)
	if err != nil {
		return Attr{}, err
	}
	return f.attrAt(ctx, n, path.Clean("/"+p)), nil
}

// resolve walks a path, filling directories from the provider on the way.
func (f *FS) resolve(ctx context.Context, p string) (meta.Node, error) {
	cur, err := f.meta.Get(ctx, meta.RootIno)
	if err != nil {
		return meta.Node{}, err
	}
	for _, seg := range strings.Split(strings.Trim(path.Clean("/"+p), "/"), "/") {
		if seg == "" {
			continue
		}
		child, err := f.lookupNode(ctx, cur.Ino, seg)
		if err != nil {
			return meta.Node{}, err
		}
		cur = child
	}
	return cur, nil
}

// Lookup returns one child by name.
func (f *FS) Lookup(ctx context.Context, parent uint64, name string) (Attr, error) {
	n, err := f.lookupNode(ctx, parent, name)
	if err != nil {
		return Attr{}, err
	}
	return f.attrOf(ctx, n), nil
}

func (f *FS) lookupNode(ctx context.Context, parent uint64, name string) (meta.Node, error) {
	// The hit is the common case and costs one query; the negative cache is
	// only consulted on a miss.
	n, err := f.meta.Lookup(ctx, parent, name)
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, meta.ErrNotFound) {
		return meta.Node{}, err
	}
	if absent, err := f.meta.IsAbsent(ctx, parent, name); err == nil && absent {
		return meta.Node{}, ErrNotFound
	}
	// Not cached: make sure the parent listing is authoritative, then look
	// again. Only the freshness matters here, not the listing itself.
	if err := f.freshenDir(ctx, parent); err != nil {
		return meta.Node{}, err
	}
	n, err = f.meta.Lookup(ctx, parent, name)
	if errors.Is(err, meta.ErrNotFound) {
		_ = f.meta.MarkAbsent(ctx, parent, name, f.opt.NegativeTTL)
		return meta.Node{}, ErrNotFound
	}
	return n, err
}

// ReadDir lists a directory, refreshing from the provider when the cached
// listing is stale. A fresh listing costs zero provider calls.
func (f *FS) ReadDir(ctx context.Context, ino uint64) ([]Attr, error) {
	nodes, err := f.readDirRefresh(ctx, ino, false)
	if err != nil {
		return nil, err
	}
	// This is the kernel mount's hot path: every ls(1) lands here. Resolve the
	// directory's own path once (it is cached) so each child's Pinned is
	// answered by joining, not by a recursive path query per entry — the same
	// fix ReadDirPath got. pathOf is only worth calling when a pin exists at
	// all, which is the only case attrAt consults the path.
	dir := ""
	if f.hasPins.Load() {
		dir, _ = f.pathOf(ctx, ino)
	}
	out := make([]Attr, 0, len(nodes))
	for _, n := range nodes {
		if dir == "" {
			out = append(out, f.attrOf(ctx, n))
			continue
		}
		out = append(out, f.attrAt(ctx, n, path.Join(dir, n.Name)))
	}
	return out, nil
}

// ReadDirPath lists by path.
func (f *FS) ReadDirPath(ctx context.Context, p string) ([]Attr, error) {
	n, err := f.resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	if !n.IsDir() {
		return nil, ErrNotDir
	}
	nodes, err := f.readDirRefresh(ctx, n.Ino, false)
	if err != nil {
		return nil, err
	}
	dir := path.Clean("/" + p)
	out := make([]Attr, 0, len(nodes))
	for _, child := range nodes {
		out = append(out, f.attrAt(ctx, child, path.Join(dir, child.Name)))
	}
	return out, nil
}

// freshenDir brings ino's listing up to date without loading it. The lookup
// miss path needs to know the listing is authoritative, not what is in it:
// reading N rows to learn that the N+1th name is new made a batch of creates
// in one directory quadratic (2000 files took 39 s).
func (f *FS) freshenDir(ctx context.Context, ino uint64) error {
	_, err := f.dirListing(ctx, ino, false, false)
	return err
}

// readDirRefresh returns the children of ino, fetching from the provider when
// the listing is stale or force is set.
func (f *FS) readDirRefresh(ctx context.Context, ino uint64, force bool) ([]meta.Node, error) {
	return f.dirListing(ctx, ino, force, true)
}

// dirListing is readDirRefresh with the choice of whether a listing that
// needs no fetch is loaded at all (want=false returns nil children).
func (f *FS) dirListing(ctx context.Context, ino uint64, force, want bool) ([]meta.Node, error) {
	// Freshness first: on the common path the listing is fresh and the
	// caller only wanted to know that, so the directory row itself is not
	// read at all.
	p, err := f.pathOf(ctx, ino)
	if errors.Is(err, meta.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m, hasMount := f.mountFor(p)
	ttl := f.opt.DefaultDirTTL
	if hasMount {
		ttl = m.DirTTL
	}
	st, err := f.meta.DirState(ctx, ino)
	if err != nil {
		return nil, err
	}
	if !force && st.Fresh(f.now(), ttl) {
		if !want {
			return nil, nil
		}
		return f.meta.Children(ctx, ino)
	}
	dirNode, err := f.meta.Get(ctx, ino)
	if errors.Is(err, meta.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !dirNode.IsDir() {
		return nil, ErrNotDir
	}
	// A directory above every mount prefix (e.g. the root when remotes are
	// mounted at /work and /gd) has no provider; its children are the mount
	// dirs, which ensureMountDirs created.
	if !hasMount || dirNode.RemoteID == "" && !f.isMountRoot(p) {
		if !want {
			return nil, nil
		}
		return f.meta.Children(ctx, ino)
	}

	refreshed, err := f.dirFlight.Do(ino, func() (*directoryRefresh, error) {
		// A delta arriving mid-listing bumps this directory's refresh
		// generation, and the fence then refuses to publish the listing that
		// started earlier. That refusal is right — the view is stale — but it
		// is not the reader's answer: a caller asked what is in this
		// directory, and "someone else refreshed it while I looked" is a
		// reason to look again, not to fail. Without the retry a scan of a
		// large tree loses whichever directories a delta poll happened to
		// land on: they come back empty or erroring, and correct a moment
		// later, which is the worst shape a wrong answer can take.
		var err error
		for attempt := 0; attempt < listingRetries; attempt++ {
			node := dirNode
			if attempt > 0 {
				// Re-read: the fence may have fired because the directory
				// itself was replaced, and the next attempt must target what
				// is there now.
				fresh, getErr := f.meta.Get(ctx, ino)
				if getErr != nil {
					return nil, getErr
				}
				if !fresh.IsDir() {
					return nil, ErrNotDir
				}
				node = fresh
			}
			err = f.fetchDir(ctx, m, ino, node, st.Complete)
			if err == nil {
				return &directoryRefresh{}, nil
			}
			if !errors.Is(err, meta.ErrListingChanged) || ctx.Err() != nil {
				return nil, err
			}
			// Deliberately not short-circuiting on "the directory is complete
			// now": a complete flag can predate this request, and treating it
			// as this caller's answer hands back a listing nobody checked —
			// which is how an empty directory reads as a real one.
			//
			// Retrying immediately loses again: what supersedes the listing is
			// a batch of changes being applied, and it bumps this directory
			// more than once while it runs. Standing back briefly is what lets
			// the batch finish, and it is bounded so a directory under a
			// constant stream of changes still answers instead of spinning.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(listingBackoff << attempt):
			}
		}
		return nil, err
	})
	if err != nil {
		// Serve a stale listing rather than failing when we have one.
		if st.Complete {
			if !want {
				return nil, nil
			}
			return f.meta.Children(ctx, ino)
		}
		return nil, err
	}
	f.prefetch.schedule(p, ino, 1)
	if !want {
		return nil, nil
	}
	return refreshed.children(ctx, f.meta, ino)
}

// listingRetries bounds how many times a listing is re-run after a concurrent
// refresh superseded it. A steady stream of deltas for one directory would
// otherwise let a reader retry indefinitely; after this many attempts the
// caller gets the stale-but-complete listing, or the error.
const listingRetries = 5

// listingBackoff is the first pause between attempts; it doubles each time,
// so five attempts stand back for at most about a quarter of a second in
// total before the caller is told the directory could not be read.
const listingBackoff = 5 * time.Millisecond

// fetchDir lists ino from the provider and applies the listing. wasComplete
// says whether a listing was already cached, which decides whether the kernel
// can hold anything the new one invalidates.
func (f *FS) fetchDir(ctx context.Context, m Mount, ino uint64, dirNode meta.Node, wasComplete bool) error {
	listing, err := f.meta.BeginDirListing(ctx, dirNode)
	if err != nil {
		return err
	}
	defer listing.Close()
	remoteID := dirNode.RemoteID
	if remoteID == "" {
		remoteID = m.RootID
	}
	children := make([]meta.Node, 0, meta.DirListingBatch)
	// Anything the tree gained after this instant cannot be in the listing
	// we are about to apply, and must not be treated as deleted by it.
	started := f.now()
	visit := func(e provider.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		children = append(children, nodeFromEntry(m.Remote, e, f.opt.AttrTTL))
		if len(children) == meta.DirListingBatch {
			if err := listing.Append(ctx, children); err != nil {
				return err
			}
			clear(children)
			children = children[:0]
		}
		return nil
	}
	if err := collectDirectory(ctx, m.Provider, remoteID, listing, visit); err != nil {
		return err
	}
	if len(children) > 0 {
		if err := listing.Append(ctx, children); err != nil {
			return err
		}
	}
	// A listing cannot see a file whose upload is still queued, nor an entry
	// created after the listing began, so it must not remove or overwrite
	// either. Without the second rule a background prefetch that listed a
	// directory just before a local mkdir would apply its stale view and
	// delete the new directory, and the next create in it failed with ENOENT.
	protect := func(n meta.Node) bool {
		return f.protectRemoteNode(n) || f.isMountDir(n.Ino) || !n.FetchedAt.Before(started.Truncate(time.Second))
	}
	f.remotePublishMu.Lock()
	err = listing.Commit(ctx, f.opt.AttrTTL, protect)
	f.remotePublishMu.Unlock()
	if err != nil {
		// A commit error can have an uncertain outcome (for example a
		// durability failure). Conservatively invalidate even if it rolled
		// back, rather than leave kernel readers using a possibly old view.
		f.listingNotificationFallback(ino)
		return err
	}
	// Only a listing that changed something the kernel may already hold is
	// worth an invalidation. The first listing of a directory is not: the
	// kernel is filling its directory cache from this very call, and an
	// invalidation now would throw that away — which is what made every
	// warm walk re-read the directories the cold walk had fetched.
	// Once metadata is committed, request cancellation must not suppress
	// coherence notifications. A failure falls back to a mount-wide hint.
	notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	count, removed, err := listing.Summary(notifyCtx)
	if err != nil {
		f.listingNotificationFallback(ino)
		return err
	}
	if removed {
		f.dropPaths()
	}
	if count > 0 {
		f.wakePins()
	}
	if count > 128 {
		f.changedNode(notifyCtx, ino, true)
		if !wasComplete {
			return nil
		}
		if fn := f.invalidateAllFn.Load(); fn != nil && *fn != nil {
			(*fn)()
			return nil
		}
	}
	err = listing.Changes(notifyCtx, func(change meta.DirChange) {
		if count <= 128 {
			f.changedListing(notifyCtx, ino, change)
		}
		if wasComplete {
			f.invalidateListing(ino, change)
		}
	})
	if err != nil {
		f.listingNotificationFallback(ino)
		return err
	}
	// Share completion, not a full child slice: paginated/lookup callers
	// need the refresh but must not load every cached node afterwards.
	return nil
}

func (f *FS) listingNotificationFallback(ino uint64) {
	f.dropPaths()
	f.wakePins()
	f.emitChange(Change{Rescan: true})
	f.invalidate(ino)
	if fn := f.invalidateAllFn.Load(); fn != nil && *fn != nil {
		(*fn)()
	}
}

// SetInvalidateAll installs an asynchronous, coalescing kernel-cache fallback
// for large or unreadable change batches. It must not block a FUSE request.
func (f *FS) SetInvalidateAll(fn func()) { f.invalidateAllFn.Store(&fn) }

// localOnlyNode reports whether a node exists only on this machine: its
// upload has not completed, or it was just created and has not even been
// committed yet (no remote id at all, and dirty). A listing cannot know
// about either, so a listing must not be allowed to remove them.
func localOnlyNode(n meta.Node) bool {
	if IsLocalOnly(n.RemoteID) {
		return true
	}
	return n.Kind == provider.KindFile && (n.RemoteID == "" || n.Dirty)
}

func nodeFromEntry(remote string, e provider.Entry, ttl time.Duration) meta.Node {
	n := meta.Node{
		Name: e.Name, Kind: e.Kind, Size: e.Size, MTime: e.ModTime,
		Remote: remote, RemoteID: e.ID, Version: e.Version,
		RemoteVersion: e.Version, TTL: ttl,
	}
	if n.MTime.IsZero() {
		n.MTime = time.Unix(0, 0)
	}
	if e.Kind == provider.KindDir {
		n.Mode = 0o755
	} else {
		n.Mode = 0o644
	}
	for _, ht := range []provider.HashType{provider.HashSHA1, provider.HashMD5, provider.HashSHA256} {
		if v, ok := e.Hashes[ht]; ok && v != "" {
			n.HashType, n.Hash = string(ht), v
			break
		}
	}
	if n.Version == "" && n.Hash != "" {
		n.Version = n.Hash
		n.RemoteVersion = n.Hash
	}
	return n
}

func mapProviderErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, provider.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, provider.ErrExists):
		return ErrExists
	default:
		return err
	}
}

func (f *FS) invalidate(ino uint64) {
	if fn := f.invalidateFn.Load(); fn != nil && *fn != nil {
		(*fn)(ino)
	}
}

type ctxKey int

const kernelOriginKey ctxKey = 1

// FromKernel marks a context as carrying a request the kernel itself made.
// The kernel keeps its own caches coherent for the changes it makes through
// the mount, so those need no invalidation from us; only changes that
// arrive another way (MCP, the delta feed, an upload landing) do. Sending
// one anyway cost a syscall per operation and threw away the page cache of
// every file the kernel had just written.
func FromKernel(ctx context.Context) context.Context {
	return context.WithValue(ctx, kernelOriginKey, true)
}

func fromKernel(ctx context.Context) bool {
	v, _ := ctx.Value(kernelOriginKey).(bool)
	return v
}

// invalidateFrom is invalidate for a change made by the request in ctx.
func (f *FS) invalidateFrom(ctx context.Context, ino uint64) {
	if fromKernel(ctx) {
		return
	}
	f.invalidate(ino)
}

// invalidateEntryFrom drops one kernel dentry for a name that a request other
// than the kernel's removed or moved. Invalidating the parent inode is not
// enough: the kernel keeps a positive dentry for the name for the whole entry
// timeout, and a file deleted through MCP or the control API kept answering
// stat(2) from the mount for that long. The kernel drops its own dentries for
// the unlinks and renames it performs, so those requests skip this.
func (f *FS) invalidateEntryFrom(ctx context.Context, parent uint64, name string) {
	if fromKernel(ctx) {
		return
	}
	if fn := f.invalidateEntryFn.Load(); fn != nil && *fn != nil {
		(*fn)(parent, name)
	}
}

// pathOf resolves an inode's path, remembering the answer. Every create,
// open and lookup resolves its parent's path to find the mount, and the
// parent of a batch of small files is the same directory every time; a
// recursive query per call was a tenth of a create. The cache is dropped
// whole whenever anything moves or vanishes (dropPaths), which is rare next
// to the lookups it saves.
func (f *FS) pathOf(ctx context.Context, ino uint64) (string, error) {
	m := f.paths.Load()
	if v, ok := m.Load(ino); ok {
		return v.(string), nil
	}
	p, err := f.meta.Path(ctx, ino)
	if err != nil {
		return "", err
	}
	m.Store(ino, p)
	return p, nil
}

// dropPaths forgets every cached path and directory id; called after a
// rename or removal.
func (f *FS) dropPaths() {
	f.paths.Store(&sync.Map{})
	f.dirIDs.Store(&sync.Map{})
	if f.hasPins.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		f.pinMu.Lock()
		if err := f.reconcilePinsLocked(ctx); err != nil {
			f.pinError = "pin retention could not be reconciled after a tree change"
		}
		f.pinMu.Unlock()
		f.wakePins()
	}
}

// dirRemoteID returns the provider id of a directory, cached alongside the
// paths: every write into a directory needs it.
func (f *FS) dirRemoteID(ctx context.Context, ino uint64) (string, error) {
	m := f.dirIDs.Load()
	if v, ok := m.Load(ino); ok {
		return v.(string), nil
	}
	n, err := f.meta.Get(ctx, ino)
	if err != nil {
		return "", err
	}
	if n.RemoteID != "" && !IsLocalOnly(n.RemoteID) {
		m.Store(ino, n.RemoteID)
	}
	return n.RemoteID, nil
}

// invalidateListing tells the kernel what a refreshed listing changed: the
// directory itself (its cached stream), each entry whose content moved, and
// each name that appeared or vanished.
func (f *FS) invalidateListing(dir uint64, c meta.DirChange) {
	f.invalidate(dir)
	for _, ino := range c.Updated {
		f.invalidate(ino)
	}
	if fn := f.invalidateEntryFn.Load(); fn != nil && *fn != nil {
		for _, name := range c.Removed {
			(*fn)(dir, name)
		}
		for _, name := range c.Added {
			(*fn)(dir, name)
		}
	}
}

// SetInvalidateEntry installs the hook that drops one kernel dentry.
func (f *FS) SetInvalidateEntry(fn func(parent uint64, name string)) {
	f.invalidateEntryFn.Store(&fn)
}

// Refresh forces a re-listing of a directory (used by delta events and
// `cloudfs warm`).
func (f *FS) Refresh(ctx context.Context, ino uint64) error {
	_, err := f.readDirRefresh(ctx, ino, true)
	return err
}

// Warm recursively lists a subtree so later lookups are local. depth < 0 means
// unlimited. It respects the provider rate limiter through normal List calls.
func (f *FS) Warm(ctx context.Context, p string, depth int) (dirs int, err error) {
	n, err := f.resolve(ctx, p)
	if err != nil {
		return 0, err
	}
	return f.warmNode(ctx, n, depth)
}

func (f *FS) warmNode(ctx context.Context, n meta.Node, depth int) (int, error) {
	if !n.IsDir() {
		return 0, nil
	}
	kids, err := f.readDirRefresh(ctx, n.Ino, false)
	if err != nil {
		return 0, err
	}
	count := 1
	if depth == 0 {
		return count, nil
	}
	for _, k := range kids {
		if ctx.Err() != nil {
			return count, ctx.Err()
		}
		if !k.IsDir() {
			continue
		}
		c, err := f.warmNode(ctx, k, depth-1)
		count += c
		if err != nil {
			return count, err
		}
	}
	return count, nil
}

// Busy reports whether the kernel is waiting on us. Background work in the
// layers below asks before spending disk or provider bandwidth on itself.
func (f *FS) Busy() bool { return f.fgIO.Load() > 0 }

// DropCaches empties the block cache and marks every directory stale, so the
// next access of anything pays the cold-path cost again. It returns how many
// files were dropped.
//
// It exists for measurement: a benchmark's cold numbers used to require
// killing the daemon and deleting the cache directory, which folded process
// start-up into every cold figure. Two kinds of entry are kept because
// dropping them would lose data rather than speed: files whose only copy is
// the local one (their upload has not finished) and pinned files, which the
// user asked to keep resident.
func (f *FS) DropCaches(ctx context.Context) (int, error) {
	f.dropPaths()
	// A cold start is also a quiet disk: what the previous run wrote behind
	// is flushed now, not under the next measurement.
	defer syncDisks()
	dropped := 0
	for _, k := range f.cache.Keys() {
		if IsLocalOnly(k.RemoteID) || f.cache.IsPinned(k) {
			continue
		}
		f.cache.Forget(k)
		dropped++
	}
	if err := f.meta.InvalidateAll(ctx); err != nil {
		return dropped, err
	}
	f.invalidate(meta.RootIno)
	return dropped, nil
}

// SetInvalidate installs the kernel-notification callback after the fact. The
// VFS is built before the FUSE mount exists, so the mount cannot be passed in
// through Options; without this hook the kernel's cached listings and
// attributes are never told about a change.
func (f *FS) SetInvalidate(fn func(ino uint64)) {
	f.invalidateFn.Store(&fn)
}
