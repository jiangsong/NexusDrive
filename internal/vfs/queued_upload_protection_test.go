package vfs

import (
	"context"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/provider"
)

// TestQueuedUploadSurvivesALostCacheEntry: a file whose upload is still
// queued lives in the journal, not on the drive, so a listing cannot see it.
// Losing the cache entry must not turn that node into a conflict loser.
// Pruning it orphans the upload row: the file leaves the namespace while its
// bytes sit in the journal, and the upload can never be validated again.
func TestQueuedUploadSurvivesALostCacheEntry(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("keep", []byte("x"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/local.txt", []byte("only in the journal"), false); err != nil {
		t.Fatal(err)
	}
	n := e.nodeOf(t, "/ali/local.txt")
	if !IsLocalOnly(n.RemoteID) {
		t.Fatalf("fixture did not queue an upload: %+v", n)
	}
	ups, err := e.fs.journal.ByIno(ctx, n.Ino)
	if err != nil || len(ups) == 0 {
		t.Fatalf("fixture has no queued upload: %v %v", ups, err)
	}

	// The cache entry goes away: eviction under a tight budget, or a restart
	// that did not repopulate it. The journal still holds the bytes.
	e.fs.cache.Forget(cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version})

	parent := e.nodeOf(t, "/ali")
	e.clk.advance(2 * time.Minute)
	if err := e.fs.Refresh(ctx, parent.Ino); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Resolve(ctx, "/ali/local.txt"); err != nil {
		t.Fatalf("a listing deleted a file whose upload is still queued: %v", err)
	}
}

// TestQueuedUploadSurvivesADeltaDeletingItsDirectory: the change feed can
// remove a whole directory, and the removal walks the subtree. A child whose
// upload is still queued exists only here, so the same rule the listing
// follows has to hold on the delta path: its bytes are in the journal, and a
// lost cache entry does not make it a landed conflict.
func TestQueuedUploadSurvivesADeltaDeletingItsDirectory(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("entry/old", []byte("old bytes"))
	if _, err := e.fs.ReadFileRange(ctx, "/ali/entry/old", 0, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/entry/local.txt", []byte("only in the journal"), false); err != nil {
		t.Fatal(err)
	}
	n := e.nodeOf(t, "/ali/entry/local.txt")
	if !IsLocalOnly(n.RemoteID) {
		t.Fatalf("fixture did not queue an upload: %+v", n)
	}
	e.fs.cache.Forget(cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version})

	dir := e.nodeOf(t, "/ali/entry")
	r := NewRefresher(e.fs, time.Minute)
	if _, err := r.apply(ctx, e.mount(), provider.Change{Op: provider.ChangeDelete, ID: dir.RemoteID}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Resolve(ctx, "/ali/entry/local.txt"); err != nil {
		t.Fatalf("a delta deleted a file whose upload is still queued: %v", err)
	}
}
