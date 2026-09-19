package vfs

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

// pendingMkdirRow returns the queued directory creation for a node.
func (e *env) pendingMkdirRow(t *testing.T, n meta.Node) journal.Upload {
	t.Helper()
	rows, err := e.j.ByIno(context.Background(), n.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].IsMkdir() {
		t.Fatalf("directory %s has queue rows %+v, want one mkdir", n.Name, rows)
	}
	return rows[0]
}

// TestMkdirReturnsBeforeProviderMkdir: on a writeback mount mkdir is a local
// commit, the way close() is. The directory is visible and usable at once;
// the backend gets it from the queue.
func TestMkdirReturnsBeforeProviderMkdir(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	a, err := e.fs.Mkdir(ctx, root.Ino, "d")
	if err != nil {
		t.Fatal(err)
	}
	if e.fake.Calls("Mkdir") != 0 {
		t.Fatalf("mkdir waited for the backend: %d calls", e.fake.Calls("Mkdir"))
	}
	if !a.LocalOnly || !a.IsDir {
		t.Fatalf("attr = %+v", a)
	}
	n := e.nodeOf(t, "/ali/d")
	if !IsLocalOnly(n.RemoteID) || !n.Dirty {
		t.Fatalf("node = %+v", n)
	}
	row := e.pendingMkdirRow(t, n)
	if row.NeedsPublish || row.State != journal.StatePending || row.RemoteParentID != e.fake.RootID() || row.Name != "d" {
		t.Fatalf("row = %+v", row)
	}
	if _, err := e.fs.Mkdir(ctx, root.Ino, "d"); !errors.Is(err, ErrExists) {
		t.Fatalf("second mkdir: %v", err)
	}
	if kids, err := e.fs.ReadDirPath(ctx, "/ali/d"); err != nil || len(kids) != 0 {
		t.Fatalf("new directory listing: %v %v", kids, err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if e.fake.Calls("Mkdir") != 1 {
		t.Fatalf("backend mkdir calls = %d", e.fake.Calls("Mkdir"))
	}
	id, ok := e.fake.IDOf("d")
	if !ok {
		t.Fatalf("directory missing on the backend: %v", e.fake.Tree())
	}
	after := e.nodeOf(t, "/ali/d")
	if after.Ino != n.Ino || after.RemoteID != id || after.Dirty {
		t.Fatalf("after landing: %+v (backend id %s)", after, id)
	}
	if row, err := e.j.Get(ctx, row.ID); err != nil || row.State != journal.StateDone {
		t.Fatalf("row after landing: %+v %v", row, err)
	}
	if a, err := e.fs.StatPath(ctx, "/ali/d"); err != nil || a.LocalOnly {
		t.Fatalf("stat after landing: %+v %v", a, err)
	}
}

// TestFilesUnderPendingDirUploadAfterItLands: a file written into a
// directory the backend does not have yet waits for the directory, then
// lands in it — never in the root, never under an id the backend has not
// seen.
func TestFilesUnderPendingDirUploadAfterItLands(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if _, err := e.fs.Mkdir(ctx, root.Ino, "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/d/f", []byte("hello"), false); err != nil {
		t.Fatal(err)
	}
	d := e.nodeOf(t, "/ali/d")
	f := e.nodeOf(t, "/ali/d/f")
	rows, err := e.j.ByIno(ctx, f.Ino)
	if err != nil || len(rows) != 1 {
		t.Fatalf("file rows: %+v %v", rows, err)
	}
	if rows[0].RemoteParentID != d.RemoteID {
		t.Fatalf("file addresses %q, want the pending directory %q", rows[0].RemoteParentID, d.RemoteID)
	}
	// The file is claimable only once its directory exists: a worker that
	// looks now finds the mkdir and nothing else.
	claimed, err := e.j.Claim(ctx, "ali", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || !claimed[0].IsMkdir() {
		t.Fatalf("claimable now: %+v", claimed)
	}
	for _, c := range claimed {
		if err := e.j.Retry(ctx, c.ID, errors.New("put back"), 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, ok := e.fake.Content("d/f"); !ok || string(got) != "hello" {
		t.Fatalf("backend tree %v content %q", e.fake.Tree(), got)
	}
	if stats, _ := e.j.Stats(ctx); stats.Pending != 0 || stats.Dead != 0 || stats.Uploading != 0 {
		t.Fatalf("queue after drain: %+v", stats)
	}
	after := e.nodeOf(t, "/ali/d/f")
	if IsLocalOnly(after.RemoteID) {
		t.Fatalf("file never adopted its backend id: %+v", after)
	}
}

// TestNestedPendingDirs: a/b/c and a file at the bottom go out in order,
// each directory into the real id of the one above it.
func TestNestedPendingDirs(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	parent := e.nodeOf(t, "/ali").Ino
	for _, name := range []string{"a", "b", "c"} {
		a, err := e.fs.Mkdir(ctx, parent, name)
		if err != nil {
			t.Fatal(err)
		}
		parent = a.Ino
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/a/b/c/f", []byte("deep"), false); err != nil {
		t.Fatal(err)
	}
	if e.fake.Calls("Mkdir") != 0 {
		t.Fatalf("mkdir waited for the backend: %d calls", e.fake.Calls("Mkdir"))
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"/a/", "/a/b/", "/a/b/c/", "/a/b/c/f"}
	if got := e.fake.Tree(); !reflect.DeepEqual(got, want) {
		t.Fatalf("backend tree = %v, want %v", got, want)
	}
	if e.fake.Calls("Mkdir") != 3 {
		t.Fatalf("backend mkdir calls = %d", e.fake.Calls("Mkdir"))
	}
	for _, p := range []string{"/ali/a", "/ali/a/b", "/ali/a/b/c", "/ali/a/b/c/f"} {
		n := e.nodeOf(t, p)
		if IsLocalOnly(n.RemoteID) || n.Dirty {
			t.Fatalf("%s did not adopt its backend id: %+v", p, n)
		}
		if n.Kind == provider.KindDir {
			id, _ := e.fake.IDOf(p[len("/ali/"):])
			if n.RemoteID != id {
				t.Fatalf("%s has id %q, backend has %q", p, n.RemoteID, id)
			}
		}
	}
}

// TestRenameLocalOnlyDirRetargetsItsRow: renaming a directory the backend
// does not have yet renames the queued creation; the backend only ever sees
// the new name, and the files queued under it follow, since they address the
// directory's own id rather than its name.
func TestRenameLocalOnlyDirRetargetsItsRow(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if _, err := e.fs.Mkdir(ctx, root.Ino, "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/old/f", []byte("x"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Mkdir(ctx, root.Ino, "elsewhere"); err != nil {
		t.Fatal(err)
	}
	elsewhere := e.nodeOf(t, "/ali/elsewhere")
	if err := e.fs.Rename(ctx, root.Ino, "old", elsewhere.Ino, "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"/elsewhere/", "/elsewhere/new/", "/elsewhere/new/f"}
	if got := e.fake.Tree(); !reflect.DeepEqual(got, want) {
		t.Fatalf("backend tree = %v, want %v", got, want)
	}
	if e.fake.Calls("Rename") != 0 || e.fake.Calls("Move") != 0 {
		t.Fatalf("a queued directory was renamed on the backend: rename=%d move=%d", e.fake.Calls("Rename"), e.fake.Calls("Move"))
	}
}

// TestRenameFileIntoPendingDirKeepsLocalParent: a queued file moved into a
// queued directory addresses that directory, not the mount root, and lands
// inside it.
func TestRenameFileIntoPendingDirKeepsLocalParent(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if _, err := e.fs.WriteFile(ctx, "/ali/tmp", []byte("saved"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Mkdir(ctx, root.Ino, "d"); err != nil {
		t.Fatal(err)
	}
	d := e.nodeOf(t, "/ali/d")
	if err := e.fs.Rename(ctx, root.Ino, "tmp", d.Ino, "final"); err != nil {
		t.Fatal(err)
	}
	rows, err := e.j.ByIno(ctx, e.nodeOf(t, "/ali/d/final").Ino)
	if err != nil || len(rows) != 1 || rows[0].RemoteParentID != d.RemoteID {
		t.Fatalf("moved file addresses %+v, want the queued directory %s", rows, d.RemoteID)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, ok := e.fake.Content("d/final"); !ok || string(got) != "saved" {
		t.Fatalf("backend tree %v", e.fake.Tree())
	}
	if _, ok := e.fake.IDOf("final"); ok {
		t.Fatal("the file landed in the root instead of the queued directory")
	}
}

// TestMoveRemoteFileIntoPendingDirCreatesTheDirFirst: a file the backend has
// cannot be moved into a directory it does not have. The rename creates the
// directory first — the queued row runs in the caller's thread — and then
// moves the file.
func TestMoveRemoteFileIntoPendingDirCreatesTheDirFirst(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("remote.txt", []byte("r"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if _, err := e.fs.Mkdir(ctx, root.Ino, "a"); err != nil {
		t.Fatal(err)
	}
	a := e.nodeOf(t, "/ali/a")
	if _, err := e.fs.Mkdir(ctx, a.Ino, "b"); err != nil {
		t.Fatal(err)
	}
	b := e.nodeOf(t, "/ali/a/b")
	if err := e.fs.Rename(ctx, root.Ino, "remote.txt", b.Ino, "moved.txt"); err != nil {
		t.Fatal(err)
	}
	if e.fake.Calls("Mkdir") != 2 || e.fake.Calls("Move") != 1 {
		t.Fatalf("mkdir=%d move=%d", e.fake.Calls("Mkdir"), e.fake.Calls("Move"))
	}
	if _, ok := e.fake.IDOf("a/b/moved.txt"); !ok {
		t.Fatalf("backend tree %v", e.fake.Tree())
	}
	for _, p := range []string{"/ali/a", "/ali/a/b"} {
		if n := e.nodeOf(t, p); IsLocalOnly(n.RemoteID) {
			t.Fatalf("%s still local after the move needed it: %+v", p, n)
		}
	}
	if stats, _ := e.j.Stats(ctx); stats.Pending != 0 || stats.Uploading != 0 {
		t.Fatalf("queue after the move: %+v", stats)
	}
}

// TestRmdirPendingDirCancelsTheCreation: removing a directory the backend
// never had is a local matter; no creation is sent, nothing is deleted.
func TestRmdirPendingDirCancelsTheCreation(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if _, err := e.fs.Mkdir(ctx, root.Ino, "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/d/f", []byte("x"), false); err != nil {
		t.Fatal(err)
	}
	d := e.nodeOf(t, "/ali/d")
	if err := e.fs.Remove(ctx, root.Ino, "d", false); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("rmdir of a non-empty queued directory: %v", err)
	}
	if err := e.fs.Remove(ctx, d.Ino, "f", false); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Remove(ctx, root.Ino, "d", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if e.fake.Calls("Mkdir") != 0 || e.fake.Calls("Delete") != 0 || len(e.fake.Tree()) != 0 {
		t.Fatalf("mkdir=%d delete=%d tree=%v", e.fake.Calls("Mkdir"), e.fake.Calls("Delete"), e.fake.Tree())
	}
	if stats, _ := e.j.Stats(ctx); stats.Pending != 0 || stats.Dead != 0 {
		t.Fatalf("queue after rmdir: %+v", stats)
	}
}

// TestReplacingAPendingDirDropsEverythingQueuedUnderIt: rename over a queued
// directory takes its whole subtree out of the tree, and the rows queued
// under it must go too — they would otherwise wait forever for a parent
// that will never land.
func TestReplacingAPendingDirDropsEverythingQueuedUnderIt(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	for _, p := range []string{"/ali/victim/sub", "/ali/other"} {
		parent := root.Ino
		for _, seg := range strings.Split(strings.TrimPrefix(p, "/ali/"), "/") {
			a, err := e.fs.Mkdir(ctx, parent, seg)
			if err != nil {
				t.Fatal(err)
			}
			parent = a.Ino
		}
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/victim/sub/f", []byte("x"), false); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Rename(ctx, root.Ino, "other", root.Ino, "victim"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if stats, _ := e.j.Stats(ctx); stats.Pending != 0 || stats.Dead != 0 || stats.Uploading != 0 {
		t.Fatalf("rows stranded under the replaced directory: %+v", stats)
	}
	if got, want := e.fake.Tree(), []string{"/victim/"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("backend tree = %v, want %v", got, want)
	}
}

// gatedMkdir holds every Mkdir at the backend's door until released, so a
// test can act while a creation is on the wire.
type gatedMkdir struct {
	provider.Provider
	started chan struct{}
	release chan struct{}
}

func (g *gatedMkdir) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	g.started <- struct{}{}
	<-g.release
	return g.Provider.Mkdir(ctx, parentID, name)
}

// TestRmdirTombstonesInFlightMkdir: the creation is on the wire when the
// directory is removed locally. It finishes, and is then deleted from the
// backend, the way a tombstoned file is.
func TestRmdirTombstonesInFlightMkdir(t *testing.T) {
	gate := &gatedMkdir{started: make(chan struct{}), release: make(chan struct{})}
	e := newEnv(t, envOpt{wrapProvider: func(f *fakeprovider.Fake) provider.Provider {
		gate.Provider = f
		return gate
	}})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if _, err := e.fs.Mkdir(ctx, root.Ino, "d"); err != nil {
		t.Fatal(err)
	}
	row := e.pendingMkdirRow(t, e.nodeOf(t, "/ali/d"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.up.DrainAll(ctx)
	}()
	<-gate.started
	if err := e.fs.Remove(ctx, root.Ino, "d", false); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	<-done
	if e.fake.Calls("Mkdir") != 1 || e.fake.Calls("Delete") != 1 || len(e.fake.Tree()) != 0 {
		t.Fatalf("mkdir=%d delete=%d tree=%v", e.fake.Calls("Mkdir"), e.fake.Calls("Delete"), e.fake.Tree())
	}
	if _, err := e.j.Get(ctx, row.ID); !errors.Is(err, journal.ErrNotFound) {
		t.Fatalf("tombstoned row after landing: %v", err)
	}
}

// TestCopyIntoPendingDirCreatesTheDirFirst: a copy binds its destination to
// the parent's backend id, so a queued parent is created before the copy is
// planned; the copy then lands where it was asked to.
func TestCopyIntoPendingDirCreatesTheDirFirst(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("src", []byte("copy me"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if _, err := e.fs.Mkdir(ctx, root.Ino, "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Copy(ctx, "/ali/src", "/ali/d/dst"); err != nil {
		t.Fatal(err)
	}
	if e.fake.Calls("Mkdir") != 1 {
		t.Fatalf("copy planned against a queued directory: mkdir=%d", e.fake.Calls("Mkdir"))
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, ok := e.fake.Content("d/dst"); !ok || string(got) != "copy me" {
		t.Fatalf("backend tree %v", e.fake.Tree())
	}
}

// TestStrictMkdirStaysSynchronous: a strict mount promises the backend has
// it when the call returns, for directories as for file content.
func TestStrictMkdirStaysSynchronous(t *testing.T) {
	e := newEnv(t, envOpt{mode: config.ModeStrict})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	a, err := e.fs.Mkdir(ctx, root.Ino, "d")
	if err != nil {
		t.Fatal(err)
	}
	if e.fake.Calls("Mkdir") != 1 || a.LocalOnly {
		t.Fatalf("mkdir=%d attr=%+v", e.fake.Calls("Mkdir"), a)
	}
	if stats, _ := e.j.Stats(ctx); stats.Pending != 0 {
		t.Fatalf("strict mkdir queued a row: %+v", stats)
	}
}

// TestCommitAfterParentLandedRetargetsItself: a file opened while its
// directory was queued addresses the directory's local id. If its commit
// lands after the directory did — after the landing rewrote the rows it
// could see — the commit itself must notice and follow, or the row waits
// forever for a parent that already exists.
func TestCommitAfterParentLandedRetargetsItself(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	a, err := e.fs.Mkdir(ctx, root.Ino, "d")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Create(ctx, a.Ino, "late")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, h, []byte("late"), 0); err != nil {
		t.Fatal(err)
	}
	released := false
	e.fs.publishFault = func(phase string) error {
		if phase == "directory-children-retargeted" && !released {
			released = true
			if err := e.fs.Release(ctx, h); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("the seam did not fire")
	}
	if got, ok := e.fake.Content("d/late"); !ok || string(got) != "late" {
		t.Fatalf("late commit stranded: tree %v", e.fake.Tree())
	}
	if stats, _ := e.j.Stats(ctx); stats.Pending != 0 {
		t.Fatalf("queue after drain: %+v", stats)
	}
}

// TestInspectMkdirRowHasPathAndKind: a queued directory shows up in the
// queue under its path, marked as a directory creation, and a dead one can
// be retried by that path; cancelling it is refused, since a cancelled
// creation could never be resumed.
func TestInspectMkdirRowHasPathAndKind(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root := e.nodeOf(t, "/ali")
	if _, err := e.fs.Mkdir(ctx, root.Ino, "d"); err != nil {
		t.Fatal(err)
	}
	row := e.pendingMkdirRow(t, e.nodeOf(t, "/ali/d"))
	info, err := e.fs.InspectUpload(ctx, row.ID)
	if err != nil || info.Path != "/ali/d" || info.Kind != journal.KindMkdir || info.Size != 0 {
		t.Fatalf("inspection: %+v %v", info, err)
	}
	if _, err := e.fs.CancelUploadAt(ctx, row.ID, "/ali/d"); !errors.Is(err, journal.ErrCannotCancel) {
		t.Fatalf("cancel of a directory creation: %v", err)
	}
	if err := e.j.Fail(ctx, row.ID, errors.New("denied")); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.RetryUploadAt(ctx, row.ID, "/ali/d"); err != nil {
		t.Fatalf("retry by path: %v", err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.fake.IDOf("d"); !ok {
		t.Fatalf("retried creation did not land: %v", e.fake.Tree())
	}
}
