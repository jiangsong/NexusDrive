package vfs

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/testx"
	"cloudfs/test/fakeprovider"
)

// TestStatDoesNotScanEveryOpenHandle: every FUSE GETATTR lands in Stat, and
// Stat has to know whether a write handle holds bytes the node's size does
// not reflect yet. Answering that by walking every open handle in the
// process makes an `lstat` of one file cost the whole handle table, so a
// `git diff` over a tree pays that table once per file while a build with a
// thousand open descriptors is running beside it. The inode either has a
// write handle or it does not, and that is a lookup.
func TestStatDoesNotScanEveryOpenHandle(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("docs/target.txt", []byte("target"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali/docs"); err != nil {
		t.Fatal(err)
	}
	dir, err := e.store.Resolve(ctx, "/ali/docs")
	if err != nil {
		t.Fatal(err)
	}
	target, err := e.store.Resolve(ctx, "/ali/docs/target.txt")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		h, err := e.fs.Create(ctx, dir.Ino, fmt.Sprintf("w%02d.txt", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.fs.Write(ctx, h, []byte("x"), 0); err != nil {
			t.Fatal(err)
		}
		defer e.fs.Release(ctx, h)
	}

	before := e.fs.pendingSizeScans.Load()
	if _, err := e.fs.Stat(ctx, target.Ino); err != nil {
		t.Fatal(err)
	}
	if got := e.fs.pendingSizeScans.Load() - before; got != 0 {
		t.Fatalf("stat of an inode with no open write handle examined %d handles, want 0", got)
	}
}

// TestStatSeesABytesWrittenButNotPublished is the guard on the shortcut
// above: a reader that stats a file through the same handle it is writing
// must still see what it has written, not the size the tree last committed.
func TestStatSeesABytesWrittenButNotPublished(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Create(ctx, root.Ino, "growing.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, h)
	payload := bytes.Repeat([]byte("g"), 321)
	if _, err := e.fs.Write(ctx, h, payload, 0); err != nil {
		t.Fatal(err)
	}
	a, err := e.fs.Stat(ctx, h.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if a.Size != int64(len(payload)) {
		t.Fatalf("stat during a write reported %d bytes, want %d", a.Size, len(payload))
	}
	if e.fs.pendingSizeScans.Load() == 0 {
		t.Fatal("the pending-write handle was never examined, so the size above was a coincidence")
	}
}

// TestAttrsDoNotResolvePathsWithACTEWhenPinned: Pinned is a property of a
// path, so once any pin rule exists every attribute reply needs one. Asking
// the metadata store for it means a recursive CTE to the root — per LOOKUP,
// per GETATTR, per readdir entry. The node already knows its parent and its
// name, and the parent's path is memoised, so a directory's worth of entries
// costs one resolution between them.
func TestAttrsDoNotResolvePathsWithACTEWhenPinned(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	const entries = 24
	for i := 0; i < entries; i++ {
		e.fake.Seed(fmt.Sprintf("docs/f%02d.txt", i), []byte("x"))
	}
	if _, err := e.fs.ReadDirPath(ctx, "/ali/docs"); err != nil {
		t.Fatal(err)
	}
	dir, err := e.store.Resolve(ctx, "/ali/docs")
	if err != nil {
		t.Fatal(err)
	}
	// A configured pin, without the fill a real Pin would run: what matters
	// here is only that attrAt has a reason to want a path at all.
	e.fs.pinMu.Lock()
	e.fs.pins = []meta.Pin{{Path: "/ali", Recursive: true, Mode: "keep"}}
	e.fs.hasPins.Store(true)
	e.fs.pinMu.Unlock()

	before := e.fs.pathQueries.Load()
	for i := 0; i < entries; i++ {
		a, err := e.fs.Lookup(ctx, dir.Ino, fmt.Sprintf("f%02d.txt", i))
		if err != nil {
			t.Fatal(err)
		}
		if !a.Pinned {
			t.Fatalf("%s is under a recursive pin but reported unpinned", a.Name)
		}
		s, err := e.fs.Stat(ctx, a.Ino)
		if err != nil {
			t.Fatal(err)
		}
		if !s.Pinned {
			t.Fatalf("stat of %s reported unpinned", a.Name)
		}
	}
	if got := e.fs.pathQueries.Load() - before; got > 1 {
		t.Fatalf("%d lookups and stats under a pin cost %d recursive path queries, want at most 1", entries, got)
	}
}

// listWatcher runs a callback inside every provider listing, which is where
// a test can look at the filesystem while a metadata request is outstanding.
type listWatcher struct {
	provider.Provider
	on func()
}

func (w *listWatcher) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	w.on()
	return w.Provider.List(ctx, dirID, cursor)
}

// TestMetadataStormMarksTheFilesystemBusy: the counter behind Busy() was
// incremented by reads and writes only, so a flood of lookups — what `git
// status` in a mounted tree is — left the filesystem looking idle. The
// crawler, the listing prefetcher and the indexer then spent the same
// SQLite file and the same provider budget the lookups were queued behind.
func TestMetadataStormMarksTheFilesystemBusy(t *testing.T) {
	var e *env
	var armed, busyDuringLookup atomic.Bool
	e = newEnv(t, envOpt{wrapProvider: func(f *fakeprovider.Fake) provider.Provider {
		return &listWatcher{Provider: f, on: func() {
			if armed.Load() && e.fs.Busy() {
				busyDuringLookup.Store(true)
			}
		}}
	}})
	ctx := context.Background()
	e.fake.Seed("docs/a.txt", []byte("alpha"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	dir, err := e.store.Resolve(ctx, "/ali/docs")
	if err != nil {
		t.Fatal(err)
	}
	if e.fs.Busy() {
		t.Fatal("nothing is outstanding, but the filesystem reports itself busy")
	}

	armed.Store(true)
	// An uncached name: the lookup has to freshen the directory first, which
	// is exactly the request a background sweep must not race.
	if _, err := e.fs.Lookup(ctx, dir.Ino, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if !busyDuringLookup.Load() {
		t.Fatal("a lookup that was listing a directory did not make the filesystem busy")
	}
	if e.fs.Busy() {
		t.Fatal("the lookup returned, but the filesystem still reports itself busy")
	}
}

// TestForegroundReadIsNotHeldByAStuckPrefetch: waiting for a sibling
// prefetch of the same file saves the drive a second request for the same
// bytes, which is worth a moment. It was worth thirty seconds, during which
// a read the kernel was blocked on waited for speculation.
func TestForegroundReadIsNotHeldByAStuckPrefetch(t *testing.T) {
	if testx.RaceEnabled {
		t.Skip("bounds a foreground read against the clock (TODO.md T-45)")
	}
	e := newEnv(t, envOpt{blockSize: 64})
	ctx := context.Background()
	content := bytes.Repeat([]byte("s"), 32)
	other := bytes.Repeat([]byte("c"), 48)
	e.fake.Seed("docs/small.txt", content)
	e.fake.Seed("docs/cold.txt", other)
	// Read it once so the bytes are cached: what is measured below is the
	// wait, not a fetch.
	if _, err := e.fs.ReadFileRange(ctx, "/ali/docs/small.txt", 0, int64(len(content))); err != nil {
		t.Fatal(err)
	}
	n, err := e.store.Resolve(ctx, "/ali/docs/small.txt")
	if err != nil {
		t.Fatal(err)
	}
	// A prefetch of this very file that never finishes.
	stuck := make(chan struct{})
	e.fs.dirAheadFlight.Store(n.Ino, stuck)
	defer close(stuck)

	read := func(p string, want []byte) error {
		got, err := e.fs.ReadFileRange(ctx, p, 0, int64(len(want)))
		if err == nil && !bytes.Equal(got, want) {
			err = fmt.Errorf("read %q, want %q", got, want)
		}
		return err
	}
	// The bytes are already here, so this read has nothing to wait for and
	// must not wait at all.
	done := make(chan error, 1)
	go func() { done <- read("/ali/docs/small.txt", content) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cached read waited on a stalled prefetch of the same file")
	}

	// A read that does have to go to the drive gives the prefetch a moment
	// and then goes, rather than holding the caller for the prefetch's own
	// lifetime.
	cold, err := e.store.Resolve(ctx, "/ali/docs/cold.txt")
	if err != nil {
		t.Fatal(err)
	}
	coldStuck := make(chan struct{})
	e.fs.dirAheadFlight.Store(cold.Ino, coldStuck)
	defer close(coldStuck)
	go func() { done <- read("/ali/docs/cold.txt", other) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cold read was held for the lifetime of a stalled prefetch of the same file")
	}
}
