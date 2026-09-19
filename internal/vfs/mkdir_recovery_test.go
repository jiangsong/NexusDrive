package vfs

import (
	"context"
	"errors"
	"testing"

	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

// TestRecoverPublicationsFinishesInterruptedMkdir: the process died after
// the directory's row was durable and before the node took the row's local
// identity. Recovery finishes the publication, and the queue then creates
// the directory as if nothing had happened.
func TestRecoverPublicationsFinishesInterruptedMkdir(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if _, err := e.fs.Mkdir(ctx, root.Ino, "d"); err != nil {
		t.Fatal(err)
	}
	n := e.nodeOf(t, "/ali/d")
	row := e.pendingMkdirRow(t, n)
	// Rewind to the crash point: node without its local identity, row
	// still behind the publication gate.
	n.RemoteID, n.Version = "", ""
	if err := e.store.UpdateByIno(ctx, n); err != nil {
		t.Fatal(err)
	}
	row.NeedsPublish = true
	if err := e.j.Commit(ctx, row); err != nil {
		t.Fatal(err)
	}
	if _, err := e.j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
		t.Fatalf("recovery refused a queued directory: %v", err)
	}
	after := e.nodeOf(t, "/ali/d")
	if after.RemoteID != localRemoteID(row.ID) || after.Version != localVersion(row.ID) || !after.IsDir() {
		t.Fatalf("node after recovery: %+v", after)
	}
	if got, err := e.j.Get(ctx, row.ID); err != nil || got.NeedsPublish {
		t.Fatalf("row after recovery: %+v %v", got, err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fake.IDOf("d"); !ok || IsLocalOnly(e.nodeOf(t, "/ali/d").RemoteID) {
		t.Fatalf("directory not created after recovery: %v", e.fake.Tree())
	}
}

// TestRecoverPublicationsDropsMkdirRowWithoutNode: the node is gone but the
// row is not. A file's row is kept as a dead letter because its bytes are
// the only copy; a directory creation has no bytes to keep.
func TestRecoverPublicationsDropsMkdirRowWithoutNode(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if _, err := e.fs.Mkdir(ctx, root.Ino, "d"); err != nil {
		t.Fatal(err)
	}
	n := e.nodeOf(t, "/ali/d")
	row := e.pendingMkdirRow(t, n)
	if err := e.store.Remove(ctx, n.Ino); err != nil {
		t.Fatal(err)
	}
	for _, unpublished := range []bool{false, true} {
		if unpublished {
			row.NeedsPublish = true
			if err := e.j.Commit(ctx, row); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := e.j.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
			t.Fatal(err)
		}
		if _, err := e.j.Get(ctx, row.ID); !errors.Is(err, journal.ErrNotFound) {
			t.Fatalf("unpublished=%v: row for a missing directory kept: %v", unpublished, err)
		}
	}
	if stats, _ := e.j.Stats(ctx); stats.Dead != 0 {
		t.Fatalf("a directory creation became a dead letter: %+v", stats)
	}
}

// TestOrphanDirtyDirIsRemovedAtStartup: the process died between inserting
// the directory node and committing its row. Nothing was acknowledged, no
// row will create it; the node is removed so its name is free again.
func TestOrphanDirtyDirIsRemovedAtStartup(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	orphan, err := e.store.InsertCompleteDir(ctx, meta.Node{ParentIno: root.Ino, Name: "orphan", Kind: provider.KindDir, Remote: "ali", Dirty: true})
	if err != nil {
		t.Fatal(err)
	}
	// A directory the backend has, and a queued one, are untouched.
	e.fake.Seed("kept/x", []byte("x"))
	if err := e.fs.Refresh(ctx, root.Ino); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Mkdir(ctx, root.Ino, "queued"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Get(ctx, orphan.Ino); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("orphan directory survived recovery: %v", err)
	}
	for _, p := range []string{"/ali/kept", "/ali/queued"} {
		if _, err := e.store.Resolve(ctx, p); err != nil {
			t.Fatalf("%s removed by recovery: %v", p, err)
		}
	}
}

// TestQueuedTreeSurvivesRestart: a tree of queued directories and files
// interrupted mid-flight — one creation was on the wire — is requeued by
// recovery and goes out in order, parents first.
func TestQueuedTreeSurvivesRestart(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	parent := e.nodeOf(t, "/ali").Ino
	for _, name := range []string{"a", "b"} {
		at, err := e.fs.Mkdir(ctx, parent, name)
		if err != nil {
			t.Fatal(err)
		}
		parent = at.Ino
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/a/b/f", []byte("deep"), false); err != nil {
		t.Fatal(err)
	}
	// The outer directory's creation was claimed when the process died.
	claimed, err := e.j.Claim(ctx, "ali", 1)
	if err != nil || len(claimed) != 1 || claimed[0].Name != "a" {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	rec, err := e.j.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Lost) != 0 || len(rec.Requeued) != 1 {
		t.Fatalf("recovery: %+v", rec)
	}
	if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, ok := e.fake.Content("a/b/f"); !ok || string(got) != "deep" {
		t.Fatalf("backend tree %v", e.fake.Tree())
	}
	if st, _ := e.j.Stats(ctx); st.Pending != 0 || st.Dead != 0 || st.Blocked != 0 {
		t.Fatalf("queue after restart and drain: %+v", st)
	}
}
