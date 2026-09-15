package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/vfs"
)

// fakeFS is an in-memory FSOps: a path tree of files and directories with
// counters for every write, so a test can say "dry run wrote nothing" and
// "rollback restored the tree" without a VFS behind it. Versions are
// "v<n>", bumped on every content change, the way a provider would.
type fakeFS struct {
	files    map[string][]byte
	dirs     map[string]bool
	versions map[string]int
	// hydrated maps a path to a local file holding its content, for the
	// link path of Capture; absent means "not hydrated".
	hydrated map[string]string
	// writes counts every mutating call.
	writes int
	reads  int
	// readErr, when set, makes ReadFileRange fail for that path.
	readErr map[string]error
	// beforeWrite, when set, runs at the start of every WriteFile and can
	// fail it; a test cancels a context from it to interrupt a rollback.
	beforeWrite func(p string) error
}

func newFakeFS() *fakeFS {
	return &fakeFS{files: map[string][]byte{}, dirs: map[string]bool{"/": true}, versions: map[string]int{},
		hydrated: map[string]string{}, readErr: map[string]error{}}
}

func (f *fakeFS) put(p string, data []byte) {
	f.files[p] = append([]byte(nil), data...)
	f.versions[p]++
	dir := path.Dir(p)
	for dir != "/" {
		f.dirs[dir] = true
		dir = path.Dir(dir)
	}
}

func (f *fakeFS) info(p string) FileInfo {
	return FileInfo{Remote: "ali", RemoteID: "id:" + p, Version: "v" + itoa(f.versions[p]), Size: int64(len(f.files[p]))}
}

func itoa(n int) string { return strconv.Itoa(n) }

func (f *fakeFS) StatPath(_ context.Context, p string) (FileInfo, error) {
	if f.dirs[p] {
		return FileInfo{IsDir: true, Remote: "ali", RemoteID: "id:" + p}, nil
	}
	if _, ok := f.files[p]; ok {
		return f.info(p), nil
	}
	return FileInfo{}, vfs.ErrNotFound
}

func (f *fakeFS) ReadFileRange(_ context.Context, p string, off, length int64) ([]byte, error) {
	f.reads++
	if err := f.readErr[p]; err != nil {
		return nil, err
	}
	if f.dirs[p] {
		return nil, vfs.ErrIsDir
	}
	data, ok := f.files[p]
	if !ok {
		return nil, vfs.ErrNotFound
	}
	if off >= int64(len(data)) {
		return nil, nil
	}
	if length <= 0 || off+length > int64(len(data)) {
		length = int64(len(data)) - off
	}
	return append([]byte(nil), data[off:off+length]...), nil
}

func (f *fakeFS) WriteFile(_ context.Context, p string, data []byte, appendMode bool) (FileInfo, error) {
	if f.beforeWrite != nil {
		if err := f.beforeWrite(p); err != nil {
			return FileInfo{}, err
		}
	}
	f.writes++
	if !f.dirs[path.Dir(p)] {
		return FileInfo{}, vfs.ErrNotFound
	}
	if f.dirs[p] {
		return FileInfo{}, vfs.ErrIsDir
	}
	if appendMode {
		data = append(append([]byte(nil), f.files[p]...), data...)
	}
	f.put(p, data)
	return f.info(p), nil
}

func (f *fakeFS) Mkdir(_ context.Context, p string) error {
	f.writes++
	if f.dirs[p] {
		return vfs.ErrExists
	}
	if _, ok := f.files[p]; ok {
		return vfs.ErrExists
	}
	if !f.dirs[path.Dir(p)] {
		return vfs.ErrNotFound
	}
	f.dirs[p] = true
	return nil
}

func (f *fakeFS) Remove(_ context.Context, p string, recursive bool) error {
	f.writes++
	if f.dirs[p] {
		for q := range f.files {
			if strings.HasPrefix(q, p+"/") {
				if !recursive {
					return vfs.ErrNotEmpty
				}
				delete(f.files, q)
			}
		}
		for q := range f.dirs {
			if strings.HasPrefix(q, p+"/") {
				if !recursive {
					return vfs.ErrNotEmpty
				}
				delete(f.dirs, q)
			}
		}
		delete(f.dirs, p)
		return nil
	}
	if _, ok := f.files[p]; !ok {
		return vfs.ErrNotFound
	}
	delete(f.files, p)
	return nil
}

func (f *fakeFS) Rename(_ context.Context, from, to string) error {
	f.writes++
	if data, ok := f.files[from]; ok {
		if _, exists := f.files[to]; exists {
			return vfs.ErrExists
		}
		f.files[to] = data
		f.versions[to] = f.versions[from]
		delete(f.files, from)
		return nil
	}
	if f.dirs[from] {
		if f.dirs[to] {
			return vfs.ErrExists
		}
		for q := range f.files {
			if strings.HasPrefix(q, from+"/") {
				f.files[to+q[len(from):]] = f.files[q]
				delete(f.files, q)
			}
		}
		for q := range f.dirs {
			if strings.HasPrefix(q, from+"/") {
				f.dirs[to+q[len(from):]] = true
				delete(f.dirs, q)
			}
		}
		delete(f.dirs, from)
		f.dirs[to] = true
		return nil
	}
	return vfs.ErrNotFound
}

func (f *fakeFS) HydratedPath(_ context.Context, p string) (string, bool) {
	hp, ok := f.hydrated[p]
	return hp, ok
}

// tree is the sorted listing of every path, "d" or "f:<content>", so two
// trees compare with one string.
func (f *fakeFS) tree() []string {
	out := []string{}
	for p := range f.dirs {
		out = append(out, "d "+p)
	}
	for p, data := range f.files {
		out = append(out, "f "+p+" "+string(data))
	}
	sort.Strings(out)
	return out
}

func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// rollbackEnv is a store, a session mapper, a preimage directory and a
// fake FS, plus the session every op is recorded under.
type rollbackEnv struct {
	st   *Store
	m    *Sessions
	pre  *Preimages
	fs   *fakeFS
	sess Session
	now  time.Time
}

func newRollbackEnv(t *testing.T) *rollbackEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &rollbackEnv{st: st, fs: newFakeFS(), now: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)}
	st.now = func() time.Time { return e.now }
	e.m = NewSessions(st, SessionOptions{Now: func() time.Time { return e.now }})
	pre, err := NewPreimages(st, filepath.Join(dir, "agent", "preimages"), nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	e.pre = pre
	p, err := e.m.EnsurePrincipal(context.Background(), "stdio", "local", Scope{})
	if err != nil {
		t.Fatal(err)
	}
	e.sess, err = e.m.Resolve(context.Background(), ConnInfo{Key: "stdio:1", Transport: "stdio", PrincipalID: p.ID, ClientName: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// do captures the preimage of p, records the op and performs the write
// through the fake, the way a mcpsrv write tool does.
func (e *rollbackEnv) do(t *testing.T, op, p, to string, write func() (string, error)) Op {
	t.Helper()
	ctx := context.Background()
	pre, err := e.pre.Capture(ctx, e.fs, p)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := e.pre.Record(ctx, e.sess.ID, Op{Op: op, Path: p, ToPath: to}.withPre(pre))
	if err != nil {
		t.Fatal(err)
	}
	post, err := write()
	if err != nil {
		t.Fatalf("%s %s: %v", op, p, err)
	}
	if err := e.st.CompleteOp(ctx, seq, post); err != nil {
		t.Fatal(err)
	}
	ops, err := e.st.OpsOf(ctx, e.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ops[len(ops)-1]
}

func (e *rollbackEnv) write(t *testing.T, op, p string, data []byte) Op {
	t.Helper()
	return e.do(t, op, p, "", func() (string, error) {
		_, err := e.fs.WriteFile(context.Background(), p, data, false)
		return ContentVersion(data), err
	})
}

func (e *rollbackEnv) blobs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(e.pre.Dir())
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, en := range entries {
		names = append(names, en.Name())
	}
	sort.Strings(names)
	return names
}

func TestCapturePrefersALinkAndFallsBackToACopy(t *testing.T) {
	e := newRollbackEnv(t)
	ctx := context.Background()
	linked := []byte("linked content")
	e.fs.put("/work/linked.txt", linked)
	src := filepath.Join(t.TempDir(), "hydrated")
	if err := os.WriteFile(src, linked, 0o600); err != nil {
		t.Fatal(err)
	}
	e.fs.hydrated["/work/linked.txt"] = src
	pre, err := e.pre.Capture(ctx, e.fs, "/work/linked.txt")
	if err != nil {
		t.Fatal(err)
	}
	if pre.State != "file" || pre.Reason != "" || pre.Hash != sum(linked) || pre.Blob != sum(linked) || pre.Size != int64(len(linked)) {
		t.Fatalf("pre = %+v", pre)
	}
	if pre.Remote != "ali" || pre.RemoteID != "id:/work/linked.txt" || pre.Version != "v1" {
		t.Fatalf("pre identity = %+v", pre)
	}
	blob := filepath.Join(e.pre.Dir(), pre.Blob)
	if !sameFile(t, src, blob) {
		t.Fatal("a hydrated file must be hard-linked, not copied")
	}

	copied := []byte("copied content")
	e.fs.put("/work/copied.txt", copied)
	pre2, err := e.pre.Capture(ctx, e.fs, "/work/copied.txt")
	if err != nil {
		t.Fatal(err)
	}
	if pre2.State != "file" || pre2.Reason != "" || pre2.Blob != sum(copied) {
		t.Fatalf("pre = %+v", pre2)
	}
	if got, _ := os.ReadFile(filepath.Join(e.pre.Dir(), pre2.Blob)); string(got) != string(copied) {
		t.Fatalf("copied blob = %q", got)
	}
	if strays := e.blobs(t); len(strays) != 2 {
		t.Fatalf("blob directory: %v", strays)
	}

	// The same content again reuses the blob rather than writing a second.
	e.fs.put("/work/dup.txt", copied)
	pre3, err := e.pre.Capture(ctx, e.fs, "/work/dup.txt")
	if err != nil || pre3.Blob != pre2.Blob {
		t.Fatalf("dedupe: %+v %v", pre3, err)
	}
	if strays := e.blobs(t); len(strays) != 2 {
		t.Fatalf("blob directory after dedupe: %v", strays)
	}

	// Absent and directory paths carry no content.
	if pre, err := e.pre.Capture(ctx, e.fs, "/work/nope.txt"); err != nil || pre.State != "absent" || pre.Blob != "" {
		t.Fatalf("absent: %+v %v", pre, err)
	}
	if pre, err := e.pre.Capture(ctx, e.fs, "/work"); err != nil || pre.State != "dir" || pre.Blob != "" || pre.Reason != "" {
		t.Fatalf("dir: %+v %v", pre, err)
	}
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	ia, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(ia, ib)
}

func TestCaptureNeverRefusesTheWrite(t *testing.T) {
	e := newRollbackEnv(t)
	ctx := context.Background()
	big := make([]byte, 1<<20+1)
	e.fs.put("/work/big.bin", big)
	pre, err := e.pre.Capture(ctx, e.fs, "/work/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if pre.State != "file" || pre.Reason != "too_large" || pre.Blob != "" || pre.Size != int64(len(big)) {
		t.Fatalf("too large: %+v", pre)
	}
	e.fs.put("/work/broken.txt", []byte("x"))
	e.fs.readErr["/work/broken.txt"] = errors.New("provider: link expired")
	pre, err = e.pre.Capture(ctx, e.fs, "/work/broken.txt")
	if err != nil {
		t.Fatal(err)
	}
	if pre.State != "file" || pre.Reason != "not_cached" || pre.Blob != "" {
		t.Fatalf("unreadable: %+v", pre)
	}
	// Both are recorded as rows all the same, and the write goes ahead.
	for _, p := range []string{"/work/big.bin", "/work/broken.txt"} {
		pre, _ := e.pre.Capture(ctx, e.fs, p)
		if _, err := e.pre.Record(ctx, e.sess.ID, Op{Op: "overwrite", Path: p}.withPre(pre)); err != nil {
			t.Fatal(err)
		}
	}
	ops, err := e.st.OpsOf(ctx, e.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 || ops[0].PreReason != "too_large" || ops[1].PreReason != "not_cached" {
		t.Fatalf("ops: %+v", ops)
	}
	if got := e.blobs(t); len(got) != 0 {
		t.Fatalf("no blob should exist: %v", got)
	}
	// Rolling back skips them with their reason, and touches nothing.
	plan, _, err := e.m.Rollback(ctx, e.fs, e.pre, e.sess.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Skipped) != 2 || len(plan.Restored) != 0 || len(plan.Conflict) != 0 {
		t.Fatalf("plan: %+v", plan)
	}
	reasons := []string{plan.Skipped[0].Reason, plan.Skipped[1].Reason}
	sort.Strings(reasons)
	if strings.Join(reasons, ",") != "not_cached,too_large" {
		t.Fatalf("reasons: %v", reasons)
	}
}

func TestRecoverDropsOrphanBlobs(t *testing.T) {
	e := newRollbackEnv(t)
	ctx := context.Background()
	e.fs.put("/work/a.txt", []byte("kept"))
	e.write(t, "overwrite", "/work/a.txt", []byte("changed"))
	kept := sum([]byte("kept"))
	// An orphan: linked, but the crash came before the row.
	orphan := filepath.Join(e.pre.Dir(), sum([]byte("orphan")))
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A temporary file of an interrupted copy.
	tmp := filepath.Join(e.pre.Dir(), sum([]byte("half"))+".tmp")
	if err := os.WriteFile(tmp, []byte("ha"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := e.pre.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed %d, want 2", removed)
	}
	if got := e.blobs(t); len(got) != 1 || got[0] != kept {
		t.Fatalf("blobs after recover: %v", got)
	}
	// A referenced blob survives a second pass too.
	if n, err := e.pre.Recover(ctx); err != nil || n != 0 {
		t.Fatalf("second recover: %d %v", n, err)
	}
}

func TestRollbackRestoresInReverseOrder(t *testing.T) {
	e := newRollbackEnv(t)
	ctx := context.Background()
	e.fs.put("/work/keep.txt", []byte("untouched"))
	e.fs.dirs["/work/out"] = true
	before := e.fs.tree()

	// create -> move -> delete, plus an edit, a mkdir and an append.
	e.write(t, "create", "/work/new.txt", []byte("hello"))
	e.do(t, "rename", "/work/new.txt", "/work/out/moved.txt", func() (string, error) {
		return "", e.fs.Rename(ctx, "/work/new.txt", "/work/out/moved.txt")
	})
	e.do(t, "delete", "/work/keep.txt", "", func() (string, error) {
		return "", e.fs.Remove(ctx, "/work/keep.txt", false)
	})
	e.do(t, "mkdir", "/work/out/sub", "", func() (string, error) {
		return "", e.fs.Mkdir(ctx, "/work/out/sub")
	})
	e.write(t, "create", "/work/out/sub/note.md", []byte("# note"))
	e.write(t, "edit", "/work/out/sub/note.md", []byte("# note edited"))
	e.do(t, "append", "/work/out/moved.txt", "", func() (string, error) {
		_, err := e.fs.WriteFile(ctx, "/work/out/moved.txt", []byte(" world"), true)
		return ContentVersion([]byte("hello world")), err
	})
	e.do(t, "delete", "/work/out/moved.txt", "", func() (string, error) {
		return "", e.fs.Remove(ctx, "/work/out/moved.txt", false)
	})
	if strings.Join(e.fs.tree(), "\n") == strings.Join(before, "\n") {
		t.Fatal("the session changed nothing?")
	}

	// An MCP caller carries its session; the rollback session is its
	// principal's.
	plan, sess, err := e.m.Rollback(WithSession(ctx, e.sess), e.fs, e.pre, e.sess.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflict) != 0 || len(plan.Skipped) != 0 || len(plan.Restored) != 8 {
		t.Fatalf("plan: %+v", plan)
	}
	// Reverse seq: the last op is the first item.
	if plan.Restored[0].Op != "delete" || plan.Restored[0].Path != "/work/out/moved.txt" || plan.Restored[7].Op != "create" {
		t.Fatalf("order: %+v", plan.Restored)
	}
	if got, want := strings.Join(e.fs.tree(), "\n"), strings.Join(before, "\n"); got != want {
		t.Fatalf("tree after rollback:\n%s\nwant:\n%s", got, want)
	}
	if sess.State != "rolled_back" || sess.RolledBackAt.IsZero() || sess.OpsCount != 8 {
		t.Fatalf("session after rollback: %+v", sess)
	}
	// The rollback itself is a finished session of the same principal with
	// one op per restored item, so it can be rolled back in turn.
	if plan.RollbackSessionID == "" {
		t.Fatal("no rollback session id")
	}
	rb, err := e.m.Get(ctx, plan.RollbackSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if rb.State != "finished" || rb.PrincipalID != e.sess.PrincipalID || rb.Transport != "stdio" || !strings.HasPrefix(rb.Summary, "rollback of "+e.sess.ID) || rb.OpsCount != 8 {
		t.Fatalf("rollback session: %+v", rb)
	}
	ops, err := e.st.OpsOf(ctx, e.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if !op.RolledBack || op.RollbackResult != "restored" {
			t.Fatalf("op row after rollback: %+v", op)
		}
	}
	// Running it again is a no-op: every row is already restored.
	again, _, err := e.m.Rollback(ctx, e.fs, e.pre, e.sess.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Restored) != 0 || len(again.Skipped) != 8 || again.Skipped[0].Reason != "already" {
		t.Fatalf("second rollback: %+v", again)
	}
	if got, want := strings.Join(e.fs.tree(), "\n"), strings.Join(before, "\n"); got != want {
		t.Fatalf("tree after second rollback changed:\n%s", got)
	}
}

func TestRollbackSkipsConflictingFiles(t *testing.T) {
	e := newRollbackEnv(t)
	ctx := context.Background()
	e.fs.put("/work/a.txt", []byte("a0"))
	e.fs.put("/work/b.txt", []byte("b0"))
	e.fs.put("/work/d.txt", []byte("d0"))
	e.fs.put("/work/gone.txt", []byte("g0"))
	e.write(t, "overwrite", "/work/a.txt", []byte("a1"))
	e.write(t, "overwrite", "/work/b.txt", []byte("b1"))
	e.write(t, "overwrite", "/work/d.txt", []byte("d1"))
	e.write(t, "create", "/work/c.txt", []byte("c1"))
	e.do(t, "delete", "/work/gone.txt", "", func() (string, error) {
		return "", e.fs.Remove(ctx, "/work/gone.txt", false)
	})
	e.do(t, "rename", "/work/b.txt", "/work/b2.txt", func() (string, error) {
		return "", e.fs.Rename(ctx, "/work/b.txt", "/work/b2.txt")
	})
	// Someone else edits a.txt and c.txt after the session, recreates
	// gone.txt, and puts a new b.txt where the rename came from. Only
	// d.txt is as the session left it.
	e.fs.put("/work/a.txt", []byte("a2 by hand"))
	e.fs.put("/work/c.txt", []byte("c2 by hand"))
	e.fs.put("/work/gone.txt", []byte("back"))
	e.fs.put("/work/b.txt", []byte("new b"))

	plan, _, err := e.m.Rollback(ctx, e.fs, e.pre, e.sess.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflict) != 5 || len(plan.Restored) != 1 || len(plan.Skipped) != 0 {
		t.Fatalf("plan: %+v", plan)
	}
	reasons := map[string]string{}
	for _, c := range plan.Conflict {
		reasons[c.Op+" "+c.Path] = c.Reason
	}
	want := map[string]string{
		"overwrite /work/a.txt": "modified", "create /work/c.txt": "modified", "delete /work/gone.txt": "exists",
		"rename /work/b.txt": "from_exists", "overwrite /work/b.txt": "modified",
	}
	for k, v := range want {
		if reasons[k] != v {
			t.Errorf("%s: reason %q, want %q (all: %+v)", k, reasons[k], v, plan.Conflict)
		}
	}
	for p, want := range map[string]string{"/work/a.txt": "a2 by hand", "/work/c.txt": "c2 by hand", "/work/gone.txt": "back", "/work/b.txt": "new b", "/work/b2.txt": "b1"} {
		if got := string(e.fs.files[p]); got != want {
			t.Errorf("%s was overwritten: %q", p, got)
		}
	}
	if plan.Restored[0].Path != "/work/d.txt" || string(e.fs.files["/work/d.txt"]) != "d0" {
		t.Fatalf("restored: %+v, d.txt=%q", plan.Restored, e.fs.files["/work/d.txt"])
	}
	ops, _ := e.st.OpsOf(ctx, e.sess.ID)
	for _, op := range ops {
		if op.Path == "/work/a.txt" && (op.RolledBack || op.RollbackResult != "conflict: modified") {
			t.Fatalf("conflict row: %+v", op)
		}
		if op.Path == "/work/d.txt" && (!op.RolledBack || op.RollbackResult != "restored") {
			t.Fatalf("restored row: %+v", op)
		}
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	e := newRollbackEnv(t)
	ctx := context.Background()
	e.fs.put("/work/a.txt", []byte("a0"))
	e.write(t, "overwrite", "/work/a.txt", []byte("a1"))
	e.write(t, "create", "/work/n.txt", []byte("n"))
	e.do(t, "mkdir", "/work/d", "", func() (string, error) { return "", e.fs.Mkdir(ctx, "/work/d") })
	writes := e.fs.writes
	sessionsBefore, _, _ := e.m.List(ctx, ListQuery{})
	blobsBefore := e.blobs(t)

	plan, sess, err := e.m.Rollback(ctx, e.fs, e.pre, e.sess.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || len(plan.Restored) != 3 || plan.RollbackSessionID != "" {
		t.Fatalf("plan: %+v", plan)
	}
	if e.fs.writes != writes {
		t.Fatalf("dry run wrote %d times", e.fs.writes-writes)
	}
	if string(e.fs.files["/work/a.txt"]) != "a1" || !e.fs.dirs["/work/d"] {
		t.Fatal("dry run changed the tree")
	}
	if sess.State != "active" || !sess.RolledBackAt.IsZero() {
		t.Fatalf("dry run changed the session: %+v", sess)
	}
	sessionsAfter, _, _ := e.m.List(ctx, ListQuery{})
	if len(sessionsAfter) != len(sessionsBefore) {
		t.Fatalf("dry run opened a session: %d -> %d", len(sessionsBefore), len(sessionsAfter))
	}
	ops, _ := e.st.OpsOf(ctx, e.sess.ID)
	for _, op := range ops {
		if op.RolledBack || op.RollbackResult != "" {
			t.Fatalf("dry run touched a row: %+v", op)
		}
	}
	if got := e.blobs(t); strings.Join(got, ",") != strings.Join(blobsBefore, ",") {
		t.Fatalf("dry run changed the blobs: %v -> %v", blobsBefore, got)
	}
}

func TestRollbackOfARollbackRestoresTheEdit(t *testing.T) {
	e := newRollbackEnv(t)
	ctx := context.Background()
	e.fs.put("/work/a.txt", []byte("original"))
	e.write(t, "edit", "/work/a.txt", []byte("edited"))
	e.write(t, "create", "/work/new.txt", []byte("new"))
	e.fs.put("/work/doomed.txt", []byte("doomed"))
	e.do(t, "delete", "/work/doomed.txt", "", func() (string, error) {
		return "", e.fs.Remove(ctx, "/work/doomed.txt", false)
	})
	edited := e.fs.tree()

	first, _, err := e.m.Rollback(ctx, e.fs, e.pre, e.sess.ID, false)
	if err != nil || len(first.Restored) != 3 {
		t.Fatalf("first rollback: %+v %v", first, err)
	}
	if string(e.fs.files["/work/a.txt"]) != "original" || e.fs.files["/work/new.txt"] != nil || string(e.fs.files["/work/doomed.txt"]) != "doomed" {
		t.Fatalf("tree after first rollback: %v", e.fs.tree())
	}
	second, rb, err := e.m.Rollback(ctx, e.fs, e.pre, first.RollbackSessionID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Restored) != 3 || len(second.Conflict) != 0 || len(second.Skipped) != 0 {
		t.Fatalf("second rollback: %+v", second)
	}
	if rb.State != "rolled_back" {
		t.Fatalf("rollback session after being rolled back: %+v", rb)
	}
	// Without a caller session (the control plane, the CLI) the rollback
	// runs as the console principal over the console transport.
	rb2, err := e.m.Get(ctx, second.RollbackSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := e.m.Principal(ctx, rb2.PrincipalID); err != nil || p.Kind != "console" || rb2.Transport != "console" {
		t.Fatalf("console rollback session: %+v %+v %v", rb2, p, err)
	}
	if got, want := strings.Join(e.fs.tree(), "\n"), strings.Join(edited, "\n"); got != want {
		t.Fatalf("tree after rolling back the rollback:\n%s\nwant:\n%s", got, want)
	}
}

func TestGCKeepsBlobsInsideRetention(t *testing.T) {
	e := newRollbackEnv(t)
	ctx := context.Background()
	e.fs.put("/work/old.txt", []byte("old"))
	e.write(t, "overwrite", "/work/old.txt", []byte("old2"))
	oldSess := e.sess
	if _, err := e.m.Finish(ctx, oldSess.ID, "done"); err != nil {
		t.Fatal(err)
	}
	// A second session, three days later, shares one blob with the first
	// and has one of its own.
	e.now = e.now.Add(3 * 24 * time.Hour)
	p, _ := e.m.Principal(ctx, oldSess.PrincipalID)
	newSess, err := e.m.Resolve(ctx, ConnInfo{Key: "stdio:2", Transport: "stdio", PrincipalID: p.ID, ClientName: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	e.sess = newSess
	e.fs.put("/work/shared.txt", []byte("old"))
	e.write(t, "overwrite", "/work/shared.txt", []byte("shared2"))
	e.fs.put("/work/fresh.txt", []byte("fresh"))
	e.write(t, "overwrite", "/work/fresh.txt", []byte("fresh2"))
	if _, err := e.m.Finish(ctx, newSess.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if got := e.blobs(t); len(got) != 2 {
		t.Fatalf("blobs: %v", got)
	}

	// Eight days after the first session finished, five after the second:
	// only the first is past a 7-day retention.
	e.now = e.now.Add(5 * 24 * time.Hour)
	res, err := e.pre.GC(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions != 1 || res.Ops != 1 || res.Blobs != 0 {
		t.Fatalf("gc: %+v", res)
	}
	if ops, _ := e.st.OpsOf(ctx, oldSess.ID); len(ops) != 0 {
		t.Fatalf("old ops survived: %+v", ops)
	}
	if ops, _ := e.st.OpsOf(ctx, newSess.ID); len(ops) != 2 {
		t.Fatalf("new ops lost: %+v", ops)
	}
	if got := e.blobs(t); len(got) != 2 {
		t.Fatalf("a blob still referenced by the newer session was removed: %v", got)
	}
	// Once the second session ages out too, both blobs go.
	e.now = e.now.Add(3 * 24 * time.Hour)
	res, err = e.pre.GC(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions != 1 || res.Ops != 2 || res.Blobs != 2 {
		t.Fatalf("gc: %+v", res)
	}
	if got := e.blobs(t); len(got) != 0 {
		t.Fatalf("blobs after gc: %v", got)
	}
	// An active session is never collected, however old.
	if _, err := e.m.Resolve(ctx, ConnInfo{Key: "stdio:3", Transport: "stdio", PrincipalID: p.ID}); err != nil {
		t.Fatal(err)
	}
	if res, err := e.pre.GC(ctx, 0); err != nil || res.Sessions != 0 {
		t.Fatalf("gc with retain 0 collected an active session: %+v %v", res, err)
	}
}

func TestSessionsTouchingFindsOpsByPath(t *testing.T) {
	e := newRollbackEnv(t)
	ctx := context.Background()
	e.fs.put("/work/a.txt", []byte("a"))
	e.write(t, "overwrite", "/work/a.txt", []byte("a1"))
	e.do(t, "rename", "/work/a.txt", "/work/b.txt", func() (string, error) {
		return "", e.fs.Rename(ctx, "/work/a.txt", "/work/b.txt")
	})
	for _, p := range []string{"/work/a.txt", "/work/b.txt"} {
		got, err := e.m.SessionsTouching(ctx, p, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != e.sess.ID {
			t.Fatalf("%s: %+v", p, got)
		}
	}
	if got, _ := e.m.SessionsTouching(ctx, "/work/c.txt", time.Time{}); len(got) != 0 {
		t.Fatalf("untouched path: %+v", got)
	}
	if got, _ := e.m.SessionsTouching(ctx, "/work/a.txt", e.now.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("since in the future: %+v", got)
	}
}

func TestRollbackFinishesAnActiveSessionBeforeReadingItsRows(t *testing.T) {
	e := newRollbackEnv(t)
	ctx := context.Background()
	conn := ConnInfo{Key: "stdio:1", Transport: "stdio", PrincipalID: e.sess.PrincipalID, ClientName: "codex"}
	e.fs.put("/work/a.txt", []byte("a0"))
	e.write(t, "overwrite", "/work/a.txt", []byte("a1"))

	// A dry run is only a look: the session stays the connection's.
	if _, s, err := e.m.Rollback(ctx, e.fs, e.pre, e.sess.ID, true); err != nil || s.State != "active" {
		t.Fatalf("after a dry run: %+v %v", s, err)
	}
	if cur, err := e.m.Resolve(ctx, conn); err != nil || cur.ID != e.sess.ID {
		t.Fatalf("the connection lost its session to a dry run: %+v %v", cur, err)
	}

	// A real rollback ends the session first, so a write the connection
	// makes while the rollback runs cannot join the rows the plan was
	// drawn from; the connection carries on in a new session, as after
	// finish_session. The write here arrives in the middle of the undo.
	e.now = e.now.Add(time.Minute)
	var during Session
	e.fs.beforeWrite = func(string) error {
		var err error
		if during, err = e.m.Resolve(ctx, conn); err != nil {
			return err
		}
		_, err = e.st.RecordOp(ctx, during.ID, Op{Op: "create", Path: "/work/late.txt", PreState: "absent"})
		return err
	}
	plan, s, err := e.m.Rollback(ctx, e.fs, e.pre, e.sess.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Restored) != 1 || s.State != "rolled_back" || s.FinishedAt.IsZero() || s.OpsCount != 1 {
		t.Fatalf("after the rollback: %+v %+v", plan, s)
	}
	if during.ID == "" || during.ID == e.sess.ID {
		t.Fatalf("a write during the rollback joined the session being rolled back: %+v", during)
	}
	cur, err := e.m.Resolve(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if cur.ID != during.ID || cur.State != "active" || cur.OpsCount != 1 {
		t.Fatalf("the connection after the rollback: %+v", cur)
	}
	active, _, err := e.m.List(ctx, ListQuery{State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != cur.ID {
		t.Fatalf("active sessions after the rollback: %+v", active)
	}
}

func TestAnInterruptedRollbackClosesItsOwnSession(t *testing.T) {
	e := newRollbackEnv(t)
	e.fs.put("/work/a.txt", []byte("a0"))
	e.fs.put("/work/b.txt", []byte("b0"))
	e.write(t, "overwrite", "/work/a.txt", []byte("a1"))
	e.write(t, "overwrite", "/work/b.txt", []byte("b1"))

	// The undo of b (the last op) goes through; the undo of a is where the
	// context is cancelled, and the write refuses like the VFS would.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.fs.beforeWrite = func(p string) error {
		if p == "/work/a.txt" {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	plan, _, err := e.m.Rollback(ctx, e.fs, e.pre, e.sess.ID, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted rollback: %v %+v", err, plan)
	}
	if plan.RollbackSessionID == "" {
		t.Fatal("no rollback session id")
	}
	if string(e.fs.files["/work/a.txt"]) != "a1" || string(e.fs.files["/work/b.txt"]) != "b0" {
		t.Fatalf("files after the interrupted run: a=%q b=%q", e.fs.files["/work/a.txt"], e.fs.files["/work/b.txt"])
	}
	// The rollback's own session is finished, not left active for the
	// console to wonder about, and says why.
	rb, err := e.m.Get(context.Background(), plan.RollbackSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if rb.State != "finished" || rb.FinishedAt.IsZero() || !strings.HasPrefix(rb.Summary, "rollback interrupted: ") {
		t.Fatalf("rollback session after the interruption: %+v", rb)
	}
	active, _, err := e.m.List(context.Background(), ListQuery{State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("sessions left active: %+v", active)
	}
	// The rerun finishes the job under a session of its own.
	e.fs.beforeWrite = nil
	again, s, err := e.m.Rollback(context.Background(), e.fs, e.pre, e.sess.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Restored) != 1 || again.Restored[0].Path != "/work/a.txt" || len(again.Skipped) != 1 || again.Skipped[0].Reason != "already" || s.State != "rolled_back" {
		t.Fatalf("rerun: %+v %+v", again, s)
	}
	if string(e.fs.files["/work/a.txt"]) != "a0" || string(e.fs.files["/work/b.txt"]) != "b0" {
		t.Fatalf("files after the rerun: a=%q b=%q", e.fs.files["/work/a.txt"], e.fs.files["/work/b.txt"])
	}
}

// TestSweepLeavesAnInFlightCopyAlone: the copy of a preimage runs outside
// the lock, so a sweep can meet its temporary file; the in-flight mark
// taken before the lock was dropped is what protects that file and the
// blob until the row is recorded.
func TestSweepLeavesAnInFlightCopyAlone(t *testing.T) {
	e := newRollbackEnv(t)
	ctx := context.Background()
	e.fs.put("/work/a.txt", []byte("live"))
	pre, err := e.pre.Capture(ctx, e.fs, "/work/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if pre.Blob == "" {
		t.Fatalf("no blob: %+v", pre)
	}
	// What a concurrent copy of the same content would leave mid-way.
	tmp := filepath.Join(e.pre.Dir(), pre.Blob+".tmp-abcd")
	if err := os.WriteFile(tmp, []byte("li"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := e.pre.Recover(ctx); err != nil || removed != 0 {
		t.Fatalf("sweep during a capture removed %d (%v)", removed, err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("the in-flight temporary file is gone: %v", err)
	}
	if _, err := e.pre.Open(pre.Blob); err != nil {
		t.Fatalf("the in-flight blob is gone: %v", err)
	}
	// Once the capture is let go without a row, both are orphans.
	e.pre.Discard(pre)
	if removed, err := e.pre.Recover(ctx); err != nil || removed != 2 {
		t.Fatalf("sweep after discard removed %d (%v), want the blob and its temporary file", removed, err)
	}
}
