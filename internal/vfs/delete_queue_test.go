package vfs

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/test/fakeprovider"
)

// queuedDeleteRows returns the delete rows still in the queue.
func (e *env) queuedDeleteRows(t *testing.T) []journal.Upload {
	t.Helper()
	rows, err := e.j.QueuedDeletes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestUnlinkReturnsBeforeProviderDelete: on a writeback mount unlink is a
// local commit, the way close() and mkdir are. The name is gone at once;
// the backend loses the file from the queue.
func TestUnlinkReturnsBeforeProviderDelete(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	seeded := e.fake.Seed("a.txt", []byte("bytes"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if err := e.fs.Remove(ctx, root.Ino, "a.txt", false); err != nil {
		t.Fatal(err)
	}
	if e.fake.Calls("Delete") != 0 {
		t.Fatalf("unlink waited for the backend: %d delete calls", e.fake.Calls("Delete"))
	}
	if _, err := e.fs.StatPath(ctx, "/ali/a.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat after unlink: %v", err)
	}
	rows := e.queuedDeleteRows(t)
	if len(rows) != 1 || rows[0].Kind != journal.KindDelete || rows[0].RemoteID != seeded.ID ||
		rows[0].RemoteParentID != e.fake.RootID() || rows[0].Name != "a.txt" || rows[0].Ino != 0 {
		t.Fatalf("queued rows = %+v", rows)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if e.fake.Calls("Delete") != 1 || len(e.fake.Tree()) != 0 {
		t.Fatalf("delete=%d tree=%v", e.fake.Calls("Delete"), e.fake.Tree())
	}
	if rows := e.queuedDeleteRows(t); len(rows) != 0 {
		t.Fatalf("rows after landing: %+v", rows)
	}
}

// TestListingDoesNotResurrectAQueuedDelete: the backend still has the file
// while its delete is queued, and a listing of the directory — the TTL ran
// out, or a delta poll marked it stale — must not bring the name back.
func TestListingDoesNotResurrectAQueuedDelete(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("a.txt", []byte("bytes"))
	e.fake.Seed("keep.txt", []byte("bytes"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if err := e.fs.Remove(ctx, root.Ino, "a.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Refresh(ctx, root.Ino); err != nil {
		t.Fatal(err)
	}
	kids, err := e.fs.ReadDirPath(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 1 || kids[0].Name != "keep.txt" {
		t.Fatalf("listing while the delete is queued: %+v", kids)
	}
	if _, err := e.fs.StatPath(ctx, "/ali/a.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat after refresh: %v", err)
	}
	// Once the delete has landed the listing agrees on its own.
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Refresh(ctx, root.Ino); err != nil {
		t.Fatal(err)
	}
	if kids, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil || len(kids) != 1 {
		t.Fatalf("listing after landing: %+v %v", kids, err)
	}
}

// TestQueuedDeletesSurviveARestart: the rows are durable, and a VFS opened
// on the same journal knows not to list their targets before it has run
// them.
func TestQueuedDeletesSurviveARestart(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("a.txt", []byte("bytes"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if err := e.fs.Remove(ctx, root.Ino, "a.txt", false); err != nil {
		t.Fatal(err)
	}
	e2 := e.reopen(t)
	if err := e2.fs.Refresh(ctx, root.Ino); err != nil {
		t.Fatal(err)
	}
	if kids, err := e2.fs.ReadDirPath(ctx, "/ali"); err != nil || len(kids) != 0 {
		t.Fatalf("listing after restart with the delete queued: %+v %v", kids, err)
	}
	if _, err := e2.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e2.fake.Tree()) != 0 {
		t.Fatalf("backend after restart drain: %v", e2.fake.Tree())
	}
}

// TestRmdirOfAListedEmptyDirectoryIsLocal: rm -rf removed everything under
// the directory through this tree, so the tree knows it is empty; asking
// the backend again on the foreground path is what made rm -rf cost a
// listing plus a delete per directory. The queue asks instead, once,
// before it removes the directory.
func TestRmdirOfAListedEmptyDirectoryIsLocal(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("d/a.txt", []byte("x"))
	e.fake.Seed("d/b.txt", []byte("y"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali/d"); err != nil {
		t.Fatal(err)
	}
	d := e.nodeOf(t, "/ali/d")
	root := e.nodeOf(t, "/ali")
	// The listing is past its TTL, the way it is after a long rm -rf.
	e.clk.advance(2 * time.Hour)
	lists := e.fake.Calls("List")
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := e.fs.Remove(ctx, d.Ino, name, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.fs.Remove(ctx, root.Ino, "d", false); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.Calls("List") - lists; got != 0 || e.fake.Calls("Delete") != 0 {
		t.Fatalf("foreground rm -rf: lists=%d deletes=%d, want none", got, e.fake.Calls("Delete"))
	}
	rows := e.queuedDeleteRows(t)
	if len(rows) != 3 || rows[2].Kind != journal.KindRmdir || rows[2].RemoteID != d.RemoteID {
		t.Fatalf("rows = %+v", rows)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Tree()) != 0 || e.fake.Calls("Delete") != 3 {
		t.Fatalf("after drain: tree=%v deletes=%d", e.fake.Tree(), e.fake.Calls("Delete"))
	}
}

// TestRmdirRefusedRemotelyBringsTheDirectoryBack: another client put a
// file in the directory after this tree listed it empty. The queue refuses
// to remove it; the tree finds out and lists the directory again, with
// their file in it.
func TestRmdirRefusedRemotelyBringsTheDirectoryBack(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("d/a.txt", []byte("x"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali/d"); err != nil {
		t.Fatal(err)
	}
	d := e.nodeOf(t, "/ali/d")
	root := e.nodeOf(t, "/ali")
	if err := e.fs.Remove(ctx, d.Ino, "a.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Remove(ctx, root.Ino, "d", false); err != nil {
		t.Fatal(err)
	}
	e.fake.Seed("d/theirs.txt", []byte("not ours"))
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fake.IDOf("d/theirs.txt"); !ok {
		t.Fatalf("their file was deleted: %v", e.fake.Tree())
	}
	if e.fake.Calls("Delete") != 1 {
		t.Fatalf("deletes = %d, want only the file's", e.fake.Calls("Delete"))
	}
	kids, err := e.fs.ReadDirPath(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 1 || kids[0].Name != "d" {
		t.Fatalf("root after the refusal: %+v, want d back", kids)
	}
	if kids, err := e.fs.ReadDirPath(ctx, "/ali/d"); err != nil || len(kids) != 1 || kids[0].Name != "theirs.txt" {
		t.Fatalf("d after the refusal: %+v %v", kids, err)
	}
}

// TestRecreateAfterUnlinkLandsAfterTheDelete: rm then cp of the same name.
// The new content must be what the backend ends up with, not the empty
// space the delete leaves when it runs second.
func TestRecreateAfterUnlinkLandsAfterTheDelete(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("a.txt", []byte("old"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if err := e.fs.Remove(ctx, root.Ino, "a.txt", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/a.txt", []byte("new"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	got, ok := e.fake.Content("a.txt")
	if !ok || string(got) != "new" {
		t.Fatalf("backend has %q %v", got, ok)
	}
	if n := e.nodeOf(t, "/ali/a.txt"); IsLocalOnly(n.RemoteID) || n.Dirty {
		t.Fatalf("node after landing: %+v", n)
	}
}

// TestStrictMountDeletesSynchronously: strict means the backend has done it
// when the call returns; nothing is queued.
func TestStrictMountDeletesSynchronously(t *testing.T) {
	e := newEnv(t, envOpt{mode: config.ModeStrict})
	ctx := context.Background()
	e.fake.Seed("a.txt", []byte("bytes"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if err := e.fs.Remove(ctx, root.Ino, "a.txt", false); err != nil {
		t.Fatal(err)
	}
	if e.fake.Calls("Delete") != 1 || len(e.fake.Tree()) != 0 {
		t.Fatalf("delete=%d tree=%v", e.fake.Calls("Delete"), e.fake.Tree())
	}
	if rows := e.queuedDeleteRows(t); len(rows) != 0 {
		t.Fatalf("strict mount queued a delete: %+v", rows)
	}
}

// gatedDelete blocks every Delete until released, and says when one has
// started, so a test can hold a remote delete open while it tries other
// things.
type gatedDelete struct {
	provider.Provider
	started chan struct{}
	release chan struct{}
}

func (g *gatedDelete) Delete(ctx context.Context, id string) error {
	g.started <- struct{}{}
	<-g.release
	return g.Provider.Delete(ctx, id)
}

// TestOpenIsNotBlockedByARemoteDelete: a strict-mode unlink waits for the
// backend, and that wait must not hold the admission gate every open on the
// mount goes through — a slow delete stalled every open and every read for
// its duration, which with a 2 s round trip was every second cp of a file.
func TestOpenIsNotBlockedByARemoteDelete(t *testing.T) {
	gate := &gatedDelete{started: make(chan struct{}), release: make(chan struct{})}
	e := newEnv(t, envOpt{mode: config.ModeStrict, wrapProvider: func(f *fakeprovider.Fake) provider.Provider {
		gate.Provider = f
		return gate
	}})
	ctx := context.Background()
	e.fake.Seed("victim.txt", []byte("bytes"))
	e.fake.Seed("other.txt", []byte("bytes"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	other := e.nodeOf(t, "/ali/other.txt")
	done := make(chan error, 1)
	go func() { done <- e.fs.Remove(ctx, root.Ino, "victim.txt", false) }()
	<-gate.started
	opened := make(chan error, 1)
	go func() {
		h, err := e.fs.Open(ctx, other.Ino, true)
		if err == nil {
			err = e.fs.Release(ctx, h)
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("open waited behind the remote delete")
	}
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestRenameOverAnExistingNameDeletesItFirst: rename(2) replaces its
// target, and the backends refuse to clobber, so the target has to be gone
// from the backend before the rename is sent — the delete cannot sit in the
// queue behind a rename that has already been asked for.
func TestRenameOverAnExistingNameDeletesItFirst(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("src.txt", []byte("source"))
	e.fake.Seed("dst.txt", []byte("target"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if err := e.fs.Rename(ctx, root.Ino, "src.txt", root.Ino, "dst.txt"); err != nil {
		t.Fatal(err)
	}
	if got, ok := e.fake.Content("dst.txt"); !ok || string(got) != "source" {
		t.Fatalf("backend dst = %q %v", got, ok)
	}
	if _, ok := e.fake.IDOf("src.txt"); ok {
		t.Fatalf("src still on the backend: %v", e.fake.Tree())
	}
	if rows := e.queuedDeleteRows(t); len(rows) != 0 {
		t.Fatalf("rename left a delete queued: %+v", rows)
	}
}

// TestRenameOnPathIDsRunsQueuedDeletesBeneathFirst: on a backend whose ids
// are paths, the deletes queued under a directory name the paths they
// remove; moving the directory first would leave them pointing at nothing
// and bring the files back under the new name. They run before the move.
func TestRenameOnPathIDsRunsQueuedDeletesBeneathFirst(t *testing.T) {
	e := newEnv(t, envOpt{pathIDs: true})
	ctx := context.Background()
	e.fake.Seed("d/a.txt", []byte("x"))
	e.fake.Seed("d/keep.txt", []byte("y"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali/d"); err != nil {
		t.Fatal(err)
	}
	d := e.nodeOf(t, "/ali/d")
	root := e.nodeOf(t, "/ali")
	if err := e.fs.Remove(ctx, d.Ino, "a.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Rename(ctx, root.Ino, "d", root.Ino, "e"); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fake.IDOf("e/a.txt"); ok {
		t.Fatalf("the removed file survived the move: %v", e.fake.Tree())
	}
	if _, ok := e.fake.IDOf("e/keep.txt"); !ok {
		t.Fatalf("kept file missing after the move: %v", e.fake.Tree())
	}
	if rows := e.queuedDeleteRows(t); len(rows) != 0 {
		t.Fatalf("rows after the move: %+v", rows)
	}
	if err := e.fs.Refresh(ctx, e.nodeOf(t, "/ali/e").Ino); err != nil {
		t.Fatal(err)
	}
	if kids, err := e.fs.ReadDirPath(ctx, "/ali/e"); err != nil || len(kids) != 1 || kids[0].Name != "keep.txt" {
		t.Fatalf("e after the move: %+v %v", kids, err)
	}
}

// TestDeltaDeleteOfAQueuedTargetIsQuiet: the backend reports the delete we
// asked for (or one that raced ours) through its change feed. The tree
// already forgot the file; the event must not fail or resurrect anything.
func TestDeltaDeleteOfAQueuedTargetIsQuiet(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("a.txt", []byte("bytes"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	r := NewRefresher(e.fs, time.Hour)
	if _, err := r.PollOnce(ctx, e.mount()); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Remove(ctx, root.Ino, "a.txt", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PollOnce(ctx, e.mount()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Lookup(ctx, root.Ino, "a.txt"); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("lookup after the feed caught up: %v", err)
	}
}

// reopen stands in for a daemon restart: the VFS and the uploader are
// rebuilt over the same metadata, cache and journal directory, and the
// journal goes through recovery the way startup runs it. The fake backend
// is the same one, holding whatever it held.
func (e *env) reopen(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	e.fs.Close()
	e.j.Close()
	j, err := journal.Open(journal.Options{Dir: filepath.Join(e.dir, "journal"), Now: e.clk.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	if _, err := j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	m := e.mount()
	fs, err := New(Options{
		Meta: e.store, Cache: e.cache, Now: e.clk.now,
		DefaultDirTTL: m.DirTTL, AttrTTL: time.Minute, NegativeTTL: 5 * time.Second,
		Mounts: []Mount{{Prefix: m.Prefix, Remote: m.Remote, RootID: m.RootID, Provider: e.fake, Mode: m.Mode, DirTTL: m.DirTTL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	up, err := upload.New(upload.Options{
		Journal:   j,
		Providers: func(string) (provider.Provider, bool) { return e.fake, true },
		Policy:    retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: time.Millisecond}, MaxAttempts: 2},
		Now:       e.clk.now,
		Hooks:     fs.UploadHooks(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fs.SetWriteBackend(j, up)
	if err := fs.RecoverPublications(ctx, j); err != nil {
		t.Fatal(err)
	}
	return &env{fs: fs, fake: e.fake, j: j, up: up, clk: e.clk, store: e.store, cache: e.cache, dir: e.dir}
}

// gatedPut holds every upload at its first request until released, and
// says when one has started.
type gatedPut struct {
	provider.Provider
	started chan struct{}
	release chan struct{}
}

func (g *gatedPut) BeginUpload(ctx context.Context, parentID, name string, size int64, h provider.Hashes) (provider.UploadSession, error) {
	g.started <- struct{}{}
	<-g.release
	return g.Provider.BeginUpload(ctx, parentID, name, size, h)
}

// TestRenameWaitsForAnInFlightUploadOutsideTheGate: mv of a file whose
// upload is on the wire. The row cannot be retargeted, so the rename waits
// for the upload and then renames the finished file on the backend. The
// wait is a remote round trip and must not hold the admission gate — and
// the rename it runs afterwards takes that gate itself, which deadlocked
// when the wait was inside it.
func TestRenameWaitsForAnInFlightUploadOutsideTheGate(t *testing.T) {
	gate := &gatedPut{started: make(chan struct{}, 1), release: make(chan struct{})}
	e := newEnv(t, envOpt{wrapProvider: func(f *fakeprovider.Fake) provider.Provider {
		gate.Provider = f
		return gate
	}})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if _, err := e.fs.WriteFile(ctx, "/ali/a.txt", []byte("payload"), false); err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() { defer close(drained); _, _ = e.up.DrainAll(ctx) }()
	<-gate.started
	renamed := make(chan error, 1)
	go func() { renamed <- e.fs.Rename(ctx, root.Ino, "a.txt", root.Ino, "b.txt") }()
	// The rename is waiting for the upload; opens must still get through.
	other := e.nodeOf(t, "/ali")
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil || other.Ino == 0 {
		t.Fatal(err)
	}
	select {
	case err := <-renamed:
		t.Fatalf("rename returned %v before the upload it must wait for finished", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(gate.release)
	<-drained
	select {
	case err := <-renamed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("rename never returned after the upload landed")
	}
	if _, ok := e.fake.IDOf("b.txt"); !ok {
		t.Fatalf("backend after the rename: %v", e.fake.Tree())
	}
	if _, ok := e.fake.IDOf("a.txt"); ok {
		t.Fatalf("old name still on the backend: %v", e.fake.Tree())
	}
	if n := e.nodeOf(t, "/ali/b.txt"); IsLocalOnly(n.RemoteID) {
		t.Fatalf("node after the rename: %+v", n)
	}
}

// TestFeedCoverageSurvivesARestart: the cursor is durable, so the first
// poll after a restart delivers everything the backend did while the daemon
// was down, and a listing taken before the restart is as current as one
// taken after — provided the previous run had the feed covering it. The
// restarted daemon must therefore not re-list a whole tree just because
// its listings are older than the TTL.
func TestFeedCoverageSurvivesARestart(t *testing.T) {
	e := newEnv(t, envOpt{dirTTL: time.Minute})
	ctx := context.Background()
	e.fake.Seed("d/a.txt", []byte("x"))
	r := NewRefresher(e.fs, time.Minute)
	if _, err := r.PollOnce(ctx, e.mount()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.ReadDirPath(ctx, "/ali/d"); err != nil {
		t.Fatal(err)
	}
	e2 := e.reopen(t)
	e.clk.advance(2 * time.Hour)
	r2 := NewRefresher(e2.fs, time.Minute)
	if _, err := r2.PollOnce(ctx, e2.mount()); err != nil {
		t.Fatal(err)
	}
	lists := e2.fake.Calls("List")
	if kids, err := e2.fs.ReadDirPath(ctx, "/ali/d"); err != nil || len(kids) != 1 {
		t.Fatalf("listing after restart: %v %v", kids, err)
	}
	if got := e2.fake.Calls("List") - lists; got != 0 {
		t.Fatalf("a listing the feed had covered before the restart was fetched again (%d calls)", got)
	}
	// A change the backend made while the daemon was down still arrives.
	e2.fake.Seed("d/b.txt", []byte("y"))
	if _, err := r2.PollOnce(ctx, e2.mount()); err != nil {
		t.Fatal(err)
	}
	if kids, err := e2.fs.ReadDirPath(ctx, "/ali/d"); err != nil || len(kids) != 2 {
		t.Fatalf("after a remote create: %v %v", kids, err)
	}
}
