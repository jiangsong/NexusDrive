package vfs

import (
	"context"
	"testing"
	"time"

	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

func TestRemoteProtectionRetainsAllWritersUntilLastClose(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("file", []byte("old"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	n, err := e.store.Resolve(ctx, "/ali/file")
	if err != nil {
		t.Fatal(err)
	}
	first, err := e.fs.Open(ctx, n.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, first)
	second, err := e.fs.Open(ctx, n.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, second)
	reader, err := e.fs.Open(ctx, n.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, reader)
	if err := e.fs.Release(ctx, first); err != nil {
		t.Fatal(err)
	}
	// A duplicate Release must not consume the other handle's reference.
	if err := e.fs.Release(ctx, first); err != nil {
		t.Fatal(err)
	}
	r := NewRefresher(e.fs, time.Minute)
	event := provider.Change{Op: provider.ChangeDelete, ID: n.RemoteID}
	if applied, err := r.apply(ctx, e.mount(), event); err != nil || applied {
		t.Fatalf("first close released another writer's protection: %v %v", applied, err)
	}
	if err := e.fs.Release(ctx, second); err != nil {
		t.Fatal(err)
	}
	if applied, err := r.apply(ctx, e.mount(), event); err != nil || !applied {
		t.Fatalf("last close or remaining reader pinned a clean node: %v %v", applied, err)
	}
	e.fs.mu.Lock()
	count := len(e.fs.writers)
	e.fs.mu.Unlock()
	if count != 0 {
		t.Fatalf("writer reference leaked: %d", count)
	}
}

func TestRemoteProtectionSurvivesClosePublicationGap(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("file", []byte("old"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	n, err := e.store.Resolve(ctx, "/ali/file")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Open(ctx, n.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, h)
	if _, err := e.fs.Write(ctx, h, []byte("new"), 0); err != nil {
		t.Fatal(err)
	}
	called := false
	e.fs.commitFault = func() error {
		called = true
		if _, registered := e.fs.HandleByFH(h.FH); !registered {
			t.Fatal("writer retired before close publication completed")
		}
		r := NewRefresher(e.fs, time.Minute)
		applied, err := r.apply(ctx, e.mount(), provider.Change{Op: provider.ChangeDelete, ID: n.RemoteID})
		if err != nil || applied {
			t.Fatalf("close publication lost protection: %v %v", applied, err)
		}
		return nil
	}
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("close publication fixture was not exercised")
	}
	if got, err := e.fs.ReadFileRange(ctx, "/ali/file", 0, 100); err != nil || string(got) != "new" {
		t.Fatalf("close publication lost local data: %q %v", got, err)
	}
	e.fs.mu.Lock()
	count := len(e.fs.writers)
	e.fs.mu.Unlock()
	if count != 0 {
		t.Fatalf("writer reference leaked after publication: %d", count)
	}
}

// TestPendingDirectorySurvivesParentListing: a directory whose remote copy has
// not been created yet has no cache entry to lose, so a listing of its parent
// must keep it the way it keeps a pending file, not treat the missing cache
// entry as a lost conflict and prune it.
func TestPendingDirectorySurvivesParentListing(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("keep", []byte("x"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	parent := e.nodeOf(t, "/ali")
	pending, err := e.store.Insert(ctx, meta.Node{
		ParentIno: parent.Ino, Name: "staged", Kind: provider.KindDir, Mode: 0o755,
		Remote: "ali", RemoteID: localRemoteID("dir-1"), Version: localVersion("dir-1"), Dirty: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.clk.advance(2 * time.Minute)
	if err := e.fs.Refresh(ctx, parent.Ino); err != nil {
		t.Fatal(err)
	}
	after, err := e.store.Resolve(ctx, "/ali/staged")
	if err != nil {
		t.Fatalf("listing removed the pending directory: %v", err)
	}
	if after.Ino != pending.Ino || after.RemoteID != pending.RemoteID {
		t.Fatalf("pending directory was replaced: %+v", after)
	}
}
