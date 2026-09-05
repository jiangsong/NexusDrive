package vfs

import (
	"context"
	"fmt"
	"testing"

	"cloudfs/internal/cache"
)

// TestReconcilingPinsCostsTheSizeOfThePinSetNotTheCache. Reconciliation runs
// from dropPaths, which every rename and every removal calls. Walking the
// whole cache there — a metadata query per cached object — makes an unrelated
// rename cost more the longer the daemon has been running, and on a large
// cache it exceeds dropPaths's own five-second budget and leaves the user with
// "pin retention could not be reconciled". The work belongs to the pinned set,
// which is what the user asked to keep, not to everything that happens to be
// cached.
func TestReconcilingPinsCostsTheSizeOfThePinSetNotTheCache(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("keep/wanted", []byte("wanted bytes"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali/keep"); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Pin(ctx, "/ali/keep"); err != nil {
		t.Fatal(err)
	}

	// A cache holding a lot of files that no pin covers. They are ordinary
	// cached objects: nothing about them should be examined again every time
	// something elsewhere in the tree is renamed.
	const unrelated = 2000
	for i := 0; i < unrelated; i++ {
		e.fs.cache.Pin(cache.FileKey{Remote: "ali", RemoteID: fmt.Sprintf("unrelated-%d", i), Version: "1"}, false)
	}

	before, _ := e.store.QueryStats()
	e.fs.dropPaths()
	cost, _ := e.store.QueryStats()
	queries := cost - before

	// The budget is the pinned set plus a small constant for resolving the
	// rules themselves — deliberately far below the cache size, so that a
	// regression to "look at every cached key" fails here rather than in a
	// user's log six months later.
	const budget = 64
	if queries > budget {
		t.Fatalf("reconciling pins after a tree change made %d metadata queries with %d unrelated files cached"+
			" (budget %d); the cost is following the cache, not the pin set", queries, unrelated, budget)
	}
	t.Logf("reconcile cost %d metadata queries with %d unrelated cached files", queries, unrelated)
}

// TestReconcileFollowsContentMovedIntoAndOutOfAPinnedDirectory. Computing the
// retained set from the rules rather than from the cache has to reach the same
// answer in both directions: a file moved under a pin becomes protected, and
// one moved out stops being protected even though nothing about its cached
// blocks changed.
func TestReconcileFollowsContentMovedIntoAndOutOfAPinnedDirectory(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("keep/inside", []byte("inside bytes"))
	e.fake.Seed("loose/outside", []byte("outside bytes"))
	for _, p := range []string{"/ali/keep", "/ali/loose"} {
		if _, err := e.fs.ReadDirPath(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	// Both files are cached before any pin exists, so the only thing that
	// changes below is where they live.
	for _, p := range []string{"/ali/keep/inside", "/ali/loose/outside"} {
		if _, err := e.fs.ReadFileRange(ctx, p, 0, 64); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.fs.Pin(ctx, "/ali/keep"); err != nil {
		t.Fatal(err)
	}
	pinned := func(p string) bool {
		t.Helper()
		a, err := e.fs.StatPath(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		n, err := e.store.Get(ctx, a.Ino)
		if err != nil {
			t.Fatal(err)
		}
		return e.fs.cache.IsPinned(cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version})
	}
	if !pinned("/ali/keep/inside") || pinned("/ali/loose/outside") {
		t.Fatalf("initial retention wrong: inside=%v outside=%v",
			pinned("/ali/keep/inside"), pinned("/ali/loose/outside"))
	}

	keep, err := e.fs.StatPath(ctx, "/ali/keep")
	if err != nil {
		t.Fatal(err)
	}
	loose, err := e.fs.StatPath(ctx, "/ali/loose")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Rename(ctx, loose.Ino, "outside", keep.Ino, "outside"); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Rename(ctx, keep.Ino, "inside", loose.Ino, "inside"); err != nil {
		t.Fatal(err)
	}
	if !pinned("/ali/keep/outside") {
		t.Fatal("a file moved into the pinned directory was not retained")
	}
	if pinned("/ali/loose/inside") {
		t.Fatal("a file moved out of the pinned directory is still retained")
	}
}
