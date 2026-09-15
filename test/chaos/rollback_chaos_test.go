package chaos

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"cloudfs/internal/agent"
	"cloudfs/internal/vfs"
)

// Session rollback (docs/agent-roadmap.md §4.8, TODO.md T-38) joins the
// reliability matrix here with its two crash cases: a process that dies
// between linking a preimage blob and inserting the session_ops row that
// names it, and a rollback interrupted halfway through its writes.

// openAgent opens agent.db under dir with a session mapper and a preimage
// store bound to the rig's cache, the way the daemon does. Nothing is
// started; the caller decides when to close it, because a "crash" is a
// store that is simply never told anything.
func openAgent(t *testing.T, r *rig, dir string) (*agent.Store, *agent.Sessions, *agent.Preimages) {
	t.Helper()
	st, err := agent.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := agent.NewSessions(st, agent.SessionOptions{})
	pre, err := agent.NewPreimages(st, filepath.Join(dir, "preimages"), r.cache, 32<<20)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	return st, m, pre
}

// stdioSession is the session a stdio MCP process would record its ops
// under: one principal, one connection.
func stdioSession(t *testing.T, m *agent.Sessions) agent.Session {
	t.Helper()
	ctx := context.Background()
	p, err := m.EnsurePrincipal(ctx, "stdio", "local", agent.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Resolve(ctx, agent.ConnInfo{Key: "stdio:1", Transport: "stdio", PrincipalID: p.ID, ClientName: "chaos"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func blobNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// errCrashed is what the test hook panics with where kill -9 would land.
var errCrashed = errors.New("chaos: process killed after os.Link")

// TestPreimageLinkThenCrashLeavesNoFalsePreimage covers "os.Link 后、
// session_ops 插入前 kill -9". Capture links the blob before Record inserts
// the row, so a crash in between (modelled by a hook that panics where the
// process would die, on a real VFS whose cache holds the hydrated file the
// blob links to) leaves an orphan blob and no row. The next owner's
// Recover removes the orphan, no session claims to have touched the path,
// and the cache still serves the file without a download: the unlink took
// only the preimage's name.
func TestPreimageLinkThenCrashLeavesNoFalsePreimage(t *testing.T) {
	r := newRig(t, rigOpt{})
	ctx := context.Background()
	agentDir := filepath.Join(r.dir, "agent")
	content := []byte(strings.Repeat("preimage chaos line\n", 512))
	if _, err := r.fs.WriteFile(ctx, "/notes.txt", content, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.up.DrainOnce(ctx, "ali"); err != nil {
		t.Fatal(err)
	}
	ops := agent.VFSOps(r.fs)
	if got, err := ops.ReadFileRange(ctx, "/notes.txt", 0, 0); err != nil || string(got) != string(content) {
		t.Fatalf("warm read: %d bytes, %v", len(got), err)
	}
	hydrated, ok := ops.HydratedPath(ctx, "/notes.txt")
	if !ok {
		t.Fatal("the cache holds no hydrated file for a file just written and read; the link path is not under test")
	}

	st, m, pre := openAgent(t, r, agentDir)
	sess := stdioSession(t, m)
	pre.HookAfterLink(func() { panic(errCrashed) })
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = pre.Capture(ctx, ops, "/notes.txt")
	}()
	if recovered != errCrashed {
		t.Fatalf("the hook did not fire where the process dies: %v", recovered)
	}
	// What the disk holds at the moment of the crash: the blob under its
	// hash, no row naming it. The crashed process closes nothing and
	// discards nothing; the store is closed here only so the next open is
	// the owner.
	if names := blobNames(t, pre.Dir()); len(names) != 1 {
		t.Fatalf("blobs after the crash: %v", names)
	}
	if rows, err := st.OpsOf(ctx, sess.ID); err != nil || len(rows) != 0 {
		t.Fatalf("a row was inserted before the crash point: %+v %v", rows, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// The restart: the owner opens agent.db and sweeps before any tool runs.
	st2, m2, pre2 := openAgent(t, r, agentDir)
	defer st2.Close()
	if !st2.Owner() {
		t.Fatal("the reopened store is not the owner")
	}
	removed, err := pre2.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("Recover removed %d blobs, want the one orphan", removed)
	}
	if names := blobNames(t, pre2.Dir()); len(names) != 0 {
		t.Fatalf("orphan blob survived recovery: %v", names)
	}
	if rows, err := st2.OpsOf(ctx, sess.ID); err != nil || len(rows) != 0 {
		t.Fatalf("session_ops after recovery: %+v %v", rows, err)
	}
	touching, err := m2.SessionsTouching(ctx, "/notes.txt", sess.StartedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(touching) != 0 {
		t.Fatalf("a session claims to have touched the path after a crash that recorded nothing: %+v", touching)
	}
	// The unlink took only the preimage's name: the cache's hydrated file
	// is still there and the file reads back without a provider call.
	if _, err := os.Stat(hydrated); err != nil {
		t.Fatalf("recovery removed the cache's hydrated file: %v", err)
	}
	reads := r.fake.Calls("ReadRange")
	if got, err := ops.ReadFileRange(ctx, "/notes.txt", 0, 0); err != nil || string(got) != string(content) {
		t.Fatalf("read after recovery: %d bytes, %v", len(got), err)
	}
	if d := r.fake.Calls("ReadRange") - reads; d != 0 {
		t.Fatalf("the read after recovery downloaded (%d ReadRange calls)", d)
	}

	// A capture that does reach its row on the recovered store keeps its
	// blob through the next sweep: Recover removes orphans, not preimages.
	captured, err := pre2.Capture(ctx, ops, "/notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if captured.Blob == "" {
		t.Fatalf("no preimage after recovery: %+v", captured)
	}
	if _, err := pre2.Record(ctx, sess.ID, agent.Op{Op: "overwrite", Path: "/notes.txt"}.WithPre(captured)); err != nil {
		t.Fatal(err)
	}
	if removed, err := pre2.Recover(ctx); err != nil || removed != 0 {
		t.Fatalf("a second sweep removed %d blobs (%v), want none", removed, err)
	}
	if names := blobNames(t, pre2.Dir()); len(names) != 1 || names[0] != captured.Blob {
		t.Fatalf("blobs after a recorded capture: %v", names)
	}
}

// blockingFS is an in-memory agent.FSOps whose n-th successful content
// write instead blocks until the context is cancelled, the way a rollback
// that dies mid-write leaves the rest of its plan undone. It is the fake
// the interrupted-rollback case needs: a real VFS cannot be told to hang on
// one particular write.
type blockingFS struct {
	mu       sync.Mutex
	files    map[string][]byte
	dirs     map[string]bool
	versions map[string]int
	// writes counts the mutating calls that changed the tree.
	writes int
	// blockAt is the ordinal of the WriteFile call to block on (0 never
	// blocks); blocked is closed when that call is waiting on its context.
	blockAt   int
	nthWrite  int
	blocked   chan struct{}
	blockOnce sync.Once
}

func newBlockingFS(blockAt int) *blockingFS {
	return &blockingFS{files: map[string][]byte{}, dirs: map[string]bool{"/": true}, versions: map[string]int{},
		blockAt: blockAt, blocked: make(chan struct{})}
}

func (f *blockingFS) put(p string, data []byte) {
	f.files[p] = append([]byte(nil), data...)
	f.versions[p]++
	for dir := path.Dir(p); dir != "/"; dir = path.Dir(dir) {
		f.dirs[dir] = true
	}
}

func (f *blockingFS) info(p string) agent.FileInfo {
	return agent.FileInfo{Remote: "ali", RemoteID: "id:" + p, Version: "v" + itoa(f.versions[p]), Size: int64(len(f.files[p]))}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func (f *blockingFS) StatPath(_ context.Context, p string) (agent.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirs[p] {
		return agent.FileInfo{IsDir: true, Remote: "ali", RemoteID: "id:" + p}, nil
	}
	if _, ok := f.files[p]; ok {
		return f.info(p), nil
	}
	return agent.FileInfo{}, vfs.ErrNotFound
}

func (f *blockingFS) ReadFileRange(_ context.Context, p string, off, length int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func (f *blockingFS) WriteFile(ctx context.Context, p string, data []byte, appendMode bool) (agent.FileInfo, error) {
	f.mu.Lock()
	f.nthWrite++
	block := f.nthWrite == f.blockAt
	f.mu.Unlock()
	if block {
		f.blockOnce.Do(func() { close(f.blocked) })
		<-ctx.Done()
		return agent.FileInfo{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.dirs[path.Dir(p)] {
		return agent.FileInfo{}, vfs.ErrNotFound
	}
	if f.dirs[p] {
		return agent.FileInfo{}, vfs.ErrIsDir
	}
	if appendMode {
		data = append(append([]byte(nil), f.files[p]...), data...)
	}
	f.put(p, data)
	f.writes++
	return f.info(p), nil
}

func (f *blockingFS) Mkdir(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.writes++
	return nil
}

func (f *blockingFS) Remove(_ context.Context, p string, recursive bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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
		f.writes++
		return nil
	}
	if _, ok := f.files[p]; !ok {
		return vfs.ErrNotFound
	}
	delete(f.files, p)
	f.writes++
	return nil
}

func (f *blockingFS) Rename(_ context.Context, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if data, ok := f.files[from]; ok {
		if _, exists := f.files[to]; exists {
			return vfs.ErrExists
		}
		f.files[to] = data
		f.versions[to] = f.versions[from]
		delete(f.files, from)
		f.writes++
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
		f.writes++
		return nil
	}
	return vfs.ErrNotFound
}

func (f *blockingFS) HydratedPath(context.Context, string) (string, bool) { return "", false }

// tree is the sorted listing of every path, "d <path>" or "f <path>
// <content>", so two trees compare item by item.
func (f *blockingFS) tree() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func (f *blockingFS) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

// record captures the preimage of p, records op under sess and runs the
// write, the sequence a mcpsrv write tool follows.
func record(t *testing.T, pre *agent.Preimages, fs agent.FSOps, sess agent.Session, op, p, to string, write func() (string, error)) {
	t.Helper()
	ctx := context.Background()
	captured, err := pre.Capture(ctx, fs, p)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := pre.Record(ctx, sess.ID, agent.Op{Op: op, Path: p, ToPath: to}.WithPre(captured))
	if err != nil {
		t.Fatal(err)
	}
	post, err := write()
	if err != nil {
		t.Fatalf("%s %s: %v", op, p, err)
	}
	if err := pre.Store().CompleteOp(ctx, seq, post); err != nil {
		t.Fatal(err)
	}
}

// TestRollbackInterruptedIsIdempotent covers "回滚中途 kill -9 重跑幂等". A
// session creates, overwrites, makes a directory, renames and deletes;
// the rollback of it is cancelled while its last write (the overwrite's
// undo) is in flight. Running the rollback again restores only what the
// first run did not reach: the tree ends identical to the pre-session
// tree, every op was undone exactly once (the fake counts mutations), the
// rows the first run completed are reported as already done and are not
// recorded a second time under the second rollback's session.
func TestRollbackInterruptedIsIdempotent(t *testing.T) {
	r := newRig(t, rigOpt{})
	ctx := context.Background()
	st, m, pre := openAgent(t, r, filepath.Join(r.dir, "agent"))
	defer st.Close()
	sess := stdioSession(t, m)

	// The rollback's undo writes, in reverse seq order, are: WriteFile
	// (undo delete /c), Rename (undo rename), Remove (undo mkdir), Remove
	// (undo create), WriteFile (undo overwrite /a). The second WriteFile is
	// the one that never returns.
	fs := newBlockingFS(0)
	fs.put("/a", []byte("a0"))
	fs.put("/b", []byte("b0"))
	fs.put("/c", []byte("c0"))
	before := fs.tree()

	record(t, pre, fs, sess, "overwrite", "/a", "", func() (string, error) {
		_, err := fs.WriteFile(ctx, "/a", []byte("a1"), false)
		return agent.ContentVersion([]byte("a1")), err
	})
	record(t, pre, fs, sess, "create", "/new", "", func() (string, error) {
		_, err := fs.WriteFile(ctx, "/new", []byte("n1"), false)
		return agent.ContentVersion([]byte("n1")), err
	})
	record(t, pre, fs, sess, "mkdir", "/d", "", func() (string, error) { return "", fs.Mkdir(ctx, "/d") })
	record(t, pre, fs, sess, "rename", "/b", "/d/b", func() (string, error) { return "", fs.Rename(ctx, "/b", "/d/b") })
	record(t, pre, fs, sess, "delete", "/c", "", func() (string, error) { return "", fs.Remove(ctx, "/c", false) })
	if _, err := m.Finish(ctx, sess.ID, "chaos"); err != nil {
		t.Fatal(err)
	}
	// The session's own writes are done recording; from here every
	// WriteFile is the rollback's, the counters start at zero and the
	// second WriteFile is the one that never returns.
	fs.mu.Lock()
	fs.nthWrite, fs.writes, fs.blockAt = 0, 0, 2
	fs.mu.Unlock()

	// The interrupted run.
	runCtx, cancel := context.WithCancel(ctx)
	type outcome struct {
		plan agent.Plan
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		plan, _, err := m.Rollback(runCtx, fs, pre, sess.ID, false)
		done <- outcome{plan, err}
	}()
	<-fs.blocked
	cancel()
	first := <-done
	if first.err == nil {
		t.Fatalf("a rollback cancelled mid-write returned no error: %+v", first.plan)
	}
	if got := fs.writeCount(); got != 4 {
		t.Fatalf("the interrupted run made %d writes, want the 4 before the blocked one", got)
	}
	if got, err := fs.ReadFileRange(ctx, "/a", 0, 0); err != nil || string(got) != "a1" {
		t.Fatalf("/a after the interrupted run: %q %v (the blocked write must not have landed)", got, err)
	}
	if s, err := m.Get(ctx, sess.ID); err != nil || s.State == "rolled_back" {
		t.Fatalf("the session is marked rolled back after an interrupted run: %+v %v", s, err)
	}

	// The rerun, after the "restart": a fresh preimage store over the same
	// directory, the way the next owner opens it.
	pre2, err := agent.NewPreimages(st, pre.Dir(), r.cache, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pre2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	second, s, err := m.Rollback(ctx, fs, pre2, sess.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := fs.tree(); strings.Join(got, "\n") != strings.Join(before, "\n") {
		t.Fatalf("tree after the rerun differs from the pre-session tree:\n got %v\nwant %v", got, before)
	}
	if len(second.Restored) != 1 || second.Restored[0].Path != "/a" {
		t.Fatalf("the rerun restored %+v, want only /a", second.Restored)
	}
	if len(second.Skipped) != 4 || len(second.Conflict) != 0 {
		t.Fatalf("the rerun's plan: skipped %+v conflict %+v", second.Skipped, second.Conflict)
	}
	for _, item := range second.Skipped {
		if item.Reason != "already" {
			t.Fatalf("a row the first run completed was not reported as already done: %+v", item)
		}
	}
	if got := fs.writeCount(); got != 5 {
		t.Fatalf("%d writes across both runs, want 5: one undo per op", got)
	}
	if s.State != "rolled_back" || s.RolledBackAt.IsZero() {
		t.Fatalf("session after the rerun: %+v", s)
	}
	rows, err := st.OpsOf(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if !row.RolledBack || row.RollbackResult != "restored" {
			t.Fatalf("row %d after the rerun: rolled_back=%v result=%q", row.Seq, row.RolledBack, row.RollbackResult)
		}
	}
	// The rollback sessions: the first recorded its four completed undos
	// and abandoned the fifth (its write never happened), the second
	// recorded only the one undo it made. Nothing was undone twice.
	if first.plan.RollbackSessionID == "" || second.RollbackSessionID == "" || first.plan.RollbackSessionID == second.RollbackSessionID {
		t.Fatalf("rollback sessions: first %q second %q", first.plan.RollbackSessionID, second.RollbackSessionID)
	}
	firstOps, err := st.OpsOf(ctx, first.plan.RollbackSessionID)
	if err != nil {
		t.Fatal(err)
	}
	completed, abandoned := 0, 0
	for _, row := range firstOps {
		switch {
		case row.RolledBack && row.RollbackResult == "write_failed":
			abandoned++
		case !row.RolledBack:
			completed++
		}
	}
	if completed != 4 || abandoned != 1 || len(firstOps) != 5 {
		t.Fatalf("the interrupted rollback's session recorded %d rows (%d completed, %d abandoned): %+v", len(firstOps), completed, abandoned, firstOps)
	}
	secondOps, err := st.OpsOf(ctx, second.RollbackSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondOps) != 1 || secondOps[0].Path != "/a" || secondOps[0].RolledBack {
		t.Fatalf("the rerun's session recorded %+v, want the one undo of /a", secondOps)
	}

	// A third run has nothing left to do and writes nothing.
	third, _, err := m.Rollback(ctx, fs, pre2, sess.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Restored) != 0 || len(third.Skipped) != 5 || fs.writeCount() != 5 {
		t.Fatalf("a third run changed something: %+v writes=%d", third, fs.writeCount())
	}
	if got := fs.tree(); strings.Join(got, "\n") != strings.Join(before, "\n") {
		t.Fatalf("tree after the third run differs from the pre-session tree:\n got %v\nwant %v", got, before)
	}
}
