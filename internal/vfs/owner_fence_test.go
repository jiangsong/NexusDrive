package vfs

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
)

// TestWritesNeedTheJournalOwner: a VFS whose journal is another process's
// (a `cloudfs mcp` stdio server started beside `cloudfs mount`) refuses
// every mutation with ErrNotOwner before it touches meta, the journal or
// the provider. T-43 found that letting such a write reach commitWrite
// leaves a node in shared meta and a journal row nobody publishes. A
// second journal.Open on the same directory is that other process: the
// flock is per open file description.
func TestWritesNeedTheJournalOwner(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("docs/a.txt", []byte("alpha"))
	e.fake.Seed("docs/b.txt", []byte("bravo"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali/docs"); err != nil {
		t.Fatal(err)
	}
	docs, err := e.fs.StatPath(ctx, "/ali/docs")
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.fs.StatPath(ctx, "/ali/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}

	other, err := journal.Open(journal.Options{Dir: filepath.Join(e.dir, "journal"), Now: e.clk.now})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.Owner() {
		t.Fatal("the second open of the journal became its owner; the flock did not hold across file descriptions")
	}
	e.fs.SetWriteBackend(other, nil)
	calls := e.fake.TotalCalls()
	before, err := e.store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}

	attempts := map[string]func() error{
		"WriteFile": func() error { _, err := e.fs.WriteFile(ctx, "/ali/docs/new.txt", []byte("x"), false); return err },
		"Append":    func() error { _, err := e.fs.WriteFile(ctx, "/ali/docs/a.txt", []byte("x"), true); return err },
		"Create":    func() error { _, err := e.fs.Create(ctx, docs.Ino, "created.txt"); return err },
		"OpenWrite": func() error { _, err := e.fs.Open(ctx, a.Ino, true); return err },
		"Truncate":  func() error { return e.fs.TruncatePath(ctx, a.Ino, 0) },
		"Mkdir":     func() error { _, err := e.fs.Mkdir(ctx, docs.Ino, "sub"); return err },
		"Remove":    func() error { return e.fs.Remove(ctx, docs.Ino, "a.txt", false) },
		"Rename":    func() error { return e.fs.Rename(ctx, docs.Ino, "a.txt", docs.Ino, "moved.txt") },
		"Copy":      func() error { _, err := e.fs.Copy(ctx, "/ali/docs/a.txt", "/ali/docs/c.txt"); return err },
	}
	for name, attempt := range attempts {
		if err := attempt(); !errors.Is(err, ErrNotOwner) {
			t.Errorf("%s on a non-owner VFS: %v, want ErrNotOwner", name, err)
		}
	}

	// Nothing below the fence moved.
	if n := e.fake.TotalCalls(); n != calls {
		t.Errorf("refused writes reached the provider: %d calls", n-calls)
	}
	for _, j := range []*journal.Journal{e.j, other} {
		rows, err := j.All(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Errorf("refused writes left journal rows: %+v", rows)
		}
	}
	after, err := e.store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Nodes != before.Nodes {
		t.Errorf("refused writes changed meta: %d nodes, was %d", after.Nodes, before.Nodes)
	}
	for _, ghost := range []string{"/ali/docs/new.txt", "/ali/docs/created.txt", "/ali/docs/sub", "/ali/docs/moved.txt", "/ali/docs/c.txt"} {
		if _, err := e.store.Resolve(ctx, ghost); !errors.Is(err, meta.ErrNotFound) {
			t.Errorf("%s is in meta after a refused write: %v", ghost, err)
		}
	}
	if n, err := e.store.Resolve(ctx, "/ali/docs/a.txt"); err != nil || n.Size != 5 {
		t.Errorf("/ali/docs/a.txt after refused writes: %+v, %v", n, err)
	}
	if got, _ := e.fake.Content("docs/a.txt"); string(got) != "alpha" {
		t.Errorf("backend content changed: %q", got)
	}
	if len(e.fs.writeHandles(a.Ino)) != 0 {
		t.Error("a refused write open left a write handle behind")
	}

	// Reads still work: the tree is shared and the block cache directory
	// too, so a non-owner fetches what it reads like any other process.
	if got, err := e.fs.ReadFileRange(ctx, "/ali/docs/a.txt", 0, 0); err != nil || string(got) != "alpha" {
		t.Errorf("read on a non-owner VFS: %q, %v", got, err)
	}
	if h, err := e.fs.Open(ctx, a.Ino, false); err != nil {
		t.Errorf("read open on a non-owner VFS: %v", err)
	} else {
		e.fs.Release(ctx, h)
	}

	// With the owner's journal back, the same writes go through: the fence
	// is about ownership, not about this VFS.
	e.fs.SetWriteBackend(e.j, e.up)
	if _, err := e.fs.WriteFile(ctx, "/ali/docs/new.txt", []byte("x"), false); err != nil {
		t.Fatalf("write as the owner after the fence: %v", err)
	}
	if _, err := e.fs.Mkdir(ctx, docs.Ino, "sub"); err != nil {
		t.Fatalf("mkdir as the owner after the fence: %v", err)
	}
}
