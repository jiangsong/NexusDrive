package vfs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func nextChange(t *testing.T, ch <-chan Change) Change {
	t.Helper()
	select {
	case c, ok := <-ch:
		if !ok {
			t.Fatal("change stream closed unexpectedly")
		}
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("change not delivered")
		return Change{}
	}
}

func noChange(t *testing.T, ch <-chan Change) {
	t.Helper()
	select {
	case c := <-ch:
		t.Fatalf("unexpected duplicate/change: %+v", c)
	default:
	}
}

func TestChangesIncludeKernelWritesWithoutKernelInvalidation(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("file", []byte("old"))
	if _, err := e.fs.StatPath(ctx, "/ali/file"); err != nil {
		t.Fatal(err)
	}
	var invalidations atomic.Int32
	e.fs.SetInvalidate(func(uint64) { invalidations.Add(1) })
	ch, cancel := e.fs.WatchChanges()
	defer cancel()
	if _, err := e.fs.WriteFile(FromKernel(ctx), "/ali/file", []byte("committed"), false); err != nil {
		t.Fatal(err)
	}
	c := nextChange(t, ch)
	if c.Rescan || !c.Affects("/ali/file") || !c.Affects("/ali") || c.Affects("/ali/neighbor") {
		t.Fatalf("wrong committed paths: %+v", c)
	}
	if invalidations.Load() != 0 {
		t.Fatal("change stream reintroduced kernel self-invalidations")
	}
	noChange(t, ch)
}

func TestChangesPreserveRenameAndDeletedSubtreePaths(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("dir/child", []byte("readable"))
	if _, err := e.fs.StatPath(ctx, "/ali/dir/child"); err != nil {
		t.Fatal(err)
	}
	parent, err := e.fs.StatPath(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel := e.fs.WatchChanges()
	defer cancel()
	if err := e.fs.Rename(ctx, parent.Ino, "dir", parent.Ino, "moved"); err != nil {
		t.Fatal(err)
	}
	c := nextChange(t, ch)
	if len(c.Paths) != 2 || !c.Subtree || !c.Affects("/ali/dir/child") || !c.Affects("/ali/moved/child") || !c.Affects("/ali") || c.Affects("/ali/dir-other") {
		t.Fatalf("rename lost old/new paths: %+v", c)
	}
	noChange(t, ch)
	if err := e.fs.Remove(ctx, parent.Ino, "moved", true); err != nil {
		t.Fatal(err)
	}
	c = nextChange(t, ch)
	if c.Rescan || !c.Subtree || !c.Affects("/ali/moved/child") || c.Affects("/ali/dir/child") {
		t.Fatalf("delete lost subtree identity: %+v", c)
	}
}

func TestChangesTrackRemoteDeltaAndUnchangedRefreshIsQuiet(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("file", []byte("old"))
	if _, err := e.fs.StatPath(ctx, "/ali/file"); err != nil {
		t.Fatal(err)
	}
	root, _ := e.fs.StatPath(ctx, "/ali")
	if err := e.store.SetCursor(ctx, "ali", e.fake.Cursor()); err != nil {
		t.Fatal(err)
	}
	ch, cancel := e.fs.WatchChanges()
	defer cancel()
	r := NewRefresher(e.fs, time.Hour)
	e.fake.Seed("file", []byte("changed remotely"))
	if _, err := r.PollOnce(ctx, e.mount()); err != nil {
		t.Fatal(err)
	}
	c := nextChange(t, ch)
	if c.Rescan || !c.Affects("/ali/file") {
		t.Fatalf("remote update not identified: %+v", c)
	}
	if _, err := r.PollOnce(ctx, e.mount()); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Refresh(ctx, root.Ino); err != nil {
		t.Fatal(err)
	}
	noChange(t, ch)
	n, _ := e.store.Resolve(ctx, "/ali/file")
	if err := e.fake.Delete(ctx, n.RemoteID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PollOnce(ctx, e.mount()); err != nil {
		t.Fatal(err)
	}
	c = nextChange(t, ch)
	if c.Rescan || len(c.Paths) != 1 || c.Paths[0] != "/ali/file" {
		t.Fatalf("remote deletion lost vanished path: %+v", c)
	}
}

func TestChangesListingIncludesAddedAndRemovedNames(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("old/child", []byte("old"))
	if _, err := e.fs.StatPath(ctx, "/ali/old/child"); err != nil {
		t.Fatal(err)
	}
	root, _ := e.fs.StatPath(ctx, "/ali")
	old, _ := e.store.Resolve(ctx, "/ali/old")
	ch, cancel := e.fs.WatchChanges()
	defer cancel()
	if err := e.fake.Delete(ctx, old.RemoteID); err != nil {
		t.Fatal(err)
	}
	e.fake.Seed("new", []byte("new"))
	// The fake clock must distinguish this listing from cached entries;
	// entries created at/after listing start are intentionally protected.
	e.clk.advance(time.Second)
	if err := e.fs.Refresh(ctx, root.Ino); err != nil {
		t.Fatal(err)
	}
	c := nextChange(t, ch)
	if c.Rescan || !c.Affects("/ali/old/child") || !c.Affects("/ali/new") || !c.Affects("/ali") {
		t.Fatalf("listing lost removed subtree: %+v", c)
	}
	noChange(t, ch)
}

func TestChangesBoundSlowConsumersAndIsolateTheirPayloads(t *testing.T) {
	e := newEnv(t, envOpt{})
	slow, cancelSlow := e.fs.WatchChanges()
	defer cancelSlow()
	fast, cancelFast := e.fs.WatchChanges()
	defer cancelFast()
	for i := range 2000 {
		e.fs.emitChange(Change{Paths: []string{fmt.Sprintf("/ali/%d", i)}})
		c := nextChange(t, fast)
		if c.Rescan {
			t.Fatal("slow consumer caused peer overflow")
		}
	}
	rescan := false
	count := 0
	for len(slow) > 0 {
		c := <-slow
		rescan = rescan || c.Rescan
		count++
	}
	if !rescan || count > 64 {
		t.Fatalf("unbounded or silently lost queue: rescan=%v count=%d", rescan, count)
	}
	e.fs.emitChange(Change{Paths: []string{"/ali/unchanged"}})
	a := nextChange(t, slow)
	a.Paths[0] = "/tampered"
	if b := nextChange(t, fast); b.Paths[0] != "/ali/unchanged" {
		t.Fatal("subscriber mutated another subscriber's event")
	}
}

func TestChangesConcurrentCancellationAndClose(t *testing.T) {
	e := newEnv(t, envOpt{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_, cancel := e.fs.WatchChanges()
				e.fs.emitChange(Change{Paths: []string{"/ali/file"}})
				cancel()
				cancel()
			}
		}()
	}
	wg.Wait()
	ch, cancel := e.fs.WatchChanges()
	if err := e.fs.Close(); err != nil {
		t.Fatal(err)
	}
	if _, open := <-ch; open {
		t.Fatal("FS close left watcher open")
	}
	cancel()
	closed, cancelClosed := e.fs.WatchChanges()
	defer cancelClosed()
	if _, open := <-closed; open || e.fs.hasChangeWatchers.Load() {
		t.Fatal("subscription reopened a closed FS")
	}
}

func TestChangesNotifyVisibleCopyEvenWhenUploadHandoffFails(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("source", []byte("complete local content"))
	if _, err := e.fs.StatPath(ctx, "/ali/source"); err != nil {
		t.Fatal(err)
	}
	ch, cancel := e.fs.WatchChanges()
	defer cancel()
	fault := errors.New("upload handoff interrupted")
	e.fs.copyBindFault = func() error { return fault }
	if _, err := e.fs.Copy(ctx, "/ali/source", "/ali/dest"); !errors.Is(err, fault) {
		t.Fatalf("copy fault not exercised: %v", err)
	}
	c := nextChange(t, ch)
	if c.Rescan || len(c.Paths) != 1 || c.Paths[0] != "/ali/dest" {
		t.Fatalf("visible copy was not announced: %+v", c)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/dest", 0, 22)
	if err != nil || string(got) != "complete local content" {
		t.Fatalf("event preceded readable content: %q %v", got, err)
	}
	noChange(t, ch)
}

func TestChangesCloseRacesWithEmitAndSubscribe(t *testing.T) {
	e := newEnv(t, envOpt{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				_, cancel := e.fs.WatchChanges()
				e.fs.emitChange(Change{Paths: []string{"/ali/path"}})
				cancel()
			}
		}()
	}
	e.fs.closeChanges()
	wg.Wait()
	if len(e.fs.changeWatchers) != 0 || e.fs.hasChangeWatchers.Load() {
		t.Fatal("closed event source retained subscribers")
	}
}

// TestOutOfKernelRemoveAndRenameDropTheKernelDentry. Invalidating the parent
// inode tells the kernel its directory stream is stale; it does not touch the
// positive dentry the kernel holds for the name, and that keeps answering
// stat(2) for the whole entry timeout. A delete or rename that did not come
// through the mount — MCP, the control API — must drop the name itself. The
// kernel's own unlink/rename drops its own dentries, so those send nothing.
func TestOutOfKernelRemoveAndRenameDropTheKernelDentry(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("dir/gone", []byte("x"))
	e.fake.Seed("dir/moved", []byte("y"))
	e.fake.Seed("dir/kernel", []byte("z"))
	dir, err := e.fs.StatPath(ctx, "/ali/dir")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	dropped := []string{}
	e.fs.SetInvalidateEntry(func(parent uint64, name string) {
		mu.Lock()
		defer mu.Unlock()
		if parent == dir.Ino {
			dropped = append(dropped, name)
		}
	})
	if err := e.fs.Remove(ctx, dir.Ino, "gone", false); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Rename(ctx, dir.Ino, "moved", dir.Ino, "arrived"); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Remove(FromKernel(ctx), dir.Ino, "kernel", false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	got := map[string]bool{}
	for _, n := range dropped {
		got[n] = true
	}
	if !got["gone"] || !got["moved"] || !got["arrived"] {
		t.Fatalf("dentries dropped: %v; want gone, moved and arrived", dropped)
	}
	if got["kernel"] {
		t.Fatal("a removal the kernel itself made was invalidated back at it")
	}
}
