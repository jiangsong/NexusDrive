package index

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

// harness is a real vfs.FS over a fake provider (the same wiring as
// mcpsrv's newEnv), an index store, and an Indexer with tiny delays.
type harness struct {
	fs    *vfs.FS
	fake  *fakeprovider.Fake
	up    *upload.Uploader
	store *Store
	x     *Indexer
	cfg   config.Index
	opts  func(*Options)
	// offset is added to the wall clock every component sees, so a test
	// can move past a listing's one-second protection window without
	// sleeping through it.
	offset atomic.Int64
}

func (h *harness) now() time.Time { return time.Now().Add(time.Duration(h.offset.Load())) }

// advance moves the shared clock forward.
func (h *harness) advance(d time.Duration) { h.offset.Add(int64(d)) }

func newHarness(t *testing.T, cfg config.Index) *harness {
	t.Helper()
	return newHarnessOpt(t, cfg, nil)
}

// newHarnessOpt is newHarness with a hook over the Indexer options, so a
// test can hand in an embedder or shorten a delay.
func newHarnessOpt(t *testing.T, cfg config.Index, opts func(*Options)) *harness {
	t.Helper()
	dir := t.TempDir()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	h := &harness{}
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{Now: h.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })

	// "ali" has Caps.PathIDs false, so a rename keeps the remote id and
	// the index can recognise the moved file.
	fake := fakeprovider.New("ali")
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca, Now: h.now,
		AttrTTL: time.Minute, DefaultDirTTL: time.Minute, NegativeTTL: time.Second,
		Mounts: []vfs.Mount{{
			Prefix: "/", Remote: "ali", RootID: fakeprovider.RootID,
			Provider: fake, Mode: config.ModeWriteback, DirTTL: time.Minute,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	up, err := upload.New(upload.Options{
		Journal: j,
		Providers: func(remote string) (provider.Provider, bool) {
			if remote == "ali" {
				return fake, true
			}
			return nil, false
		},
		Policy: retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: time.Millisecond}, MaxAttempts: 2},
		Hooks:  fsys.UploadHooks(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fsys.SetWriteBackend(j, up)

	st, err := OpenStore(filepath.Join(dir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.fs, h.fake, h.up, h.store = fsys, fake, up, st
	h.cfg, h.opts = cfg, opts
	h.x = h.newIndexer(t)
	return h
}

// newIndexer builds an Indexer over the harness's FS and store with the
// harness's options; reopenIndexer replaces the current one, the way a
// daemon restart with a changed configuration would.
func (h *harness) newIndexer(t *testing.T) *Indexer {
	t.Helper()
	opt := Options{
		FS: h.fs, Store: h.store, Config: h.cfg, Now: h.now,
		StartDelay: time.Millisecond, ReconcileEvery: time.Hour,
		YieldMax: 200 * time.Millisecond, RiskSleep: time.Second,
	}
	if h.opts != nil {
		h.opts(&opt)
	}
	x, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { x.Close() })
	return x
}

func (h *harness) reopenIndexer(t *testing.T, opts func(*Options)) *Indexer {
	t.Helper()
	if err := h.x.Close(); err != nil {
		t.Fatal(err)
	}
	h.opts = opts
	h.x = h.newIndexer(t)
	return h.x
}

// list warms a directory listing so meta knows the tree the way a readdir
// or the crawler would have left it.
func (h *harness) list(t *testing.T, p string) {
	t.Helper()
	if _, err := h.fs.ReadDirPath(context.Background(), p); err != nil {
		t.Fatalf("list %s: %v", p, err)
	}
}

// holdForeground keeps a foreground read blocked inside the provider, so
// fs.Busy() stays true until the returned function is called.
func holdForeground(t *testing.T, h *harness, p string) func() {
	t.Helper()
	h.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.Latency = time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.fs.ReadFileRange(ctx, p, 0, 1)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !h.fs.Busy() {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the foreground read never registered as in flight")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return func() {
		cancel()
		<-done
		h.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.Latency = 0 })
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}
