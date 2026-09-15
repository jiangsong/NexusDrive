package mcpsrv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/agent"
)

// currentSession is the one active session of the test client.
func currentSession(t *testing.T, st *agent.Store) agent.Session {
	t.Helper()
	m := agent.NewSessions(st, agent.SessionOptions{})
	list, _, err := m.List(context.Background(), agent.ListQuery{State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("active sessions: %+v", list)
	}
	return list[0]
}

func sha(data string) string {
	s := sha256.Sum256([]byte(data))
	return hex.EncodeToString(s[:])
}

func TestWriteToolsRecordOpsWithPreimages(t *testing.T) {
	e, st, pre := newPreimageEnv(t, Options{}, agent.Scope{})
	e.fake.Seed("work/a.txt", []byte("hello"))
	e.fake.Seed("work/b.txt", []byte("edit me"))
	e.fake.Seed("work/gone.txt", []byte("doomed"))
	e.fake.Seed("work/src.txt", []byte("source"))
	e.fake.Seed("work/old/inner.txt", []byte("inner"))
	ctx := context.Background()
	calls := []struct {
		tool string
		args map[string]any
	}{
		{"write_file", map[string]any{"path": "/work/a.txt", "content": "bye", "mode": "overwrite"}},
		{"write_file", map[string]any{"path": "/work/new.txt", "content": "fresh", "mode": "create"}},
		{"write_file", map[string]any{"path": "/work/new.txt", "content": "!", "mode": "append"}},
		{"edit_file", map[string]any{"path": "/work/b.txt", "edits": []map[string]any{{"old_text": "edit", "new_text": "edited"}}}},
		{"create_directory", map[string]any{"path": "/work/d1/d2"}},
		{"move", map[string]any{"from": "/work/b.txt", "to": "/work/d1/b.txt"}},
		{"copy", map[string]any{"from": "/work/src.txt", "to": "/work/copy.txt"}},
		{"delete", map[string]any{"path": "/work/gone.txt", "confirm": true}},
		{"delete", map[string]any{"path": "/work/old", "confirm": true, "recursive": true}},
	}
	for _, c := range calls {
		if res := e.call(t, c.tool, c.args, nil); res.IsError {
			t.Fatalf("%s: %s", c.tool, errText(res))
		}
	}
	// A dry-run edit and a refused write record nothing.
	if res := e.call(t, "edit_file", map[string]any{"path": "/work/a.txt", "dry_run": true, "edits": []map[string]any{{"old_text": "bye", "new_text": "x"}}}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if res := e.call(t, "write_file", map[string]any{"path": "/work/a.txt", "content": "x", "mode": "create"}, nil); !res.IsError {
		t.Fatal("create over an existing file succeeded")
	}
	sess := currentSession(t, st)
	ops, err := st.OpsOf(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		op, path, to, preState, preReason, preHash string
	}
	wants := []want{
		{"overwrite", "/work/a.txt", "", "file", "", sha("hello")},
		{"create", "/work/new.txt", "", "absent", "", ""},
		{"append", "/work/new.txt", "", "file", "", sha("fresh")},
		{"edit", "/work/b.txt", "", "file", "", sha("edit me")},
		{"mkdir", "/work/d1", "", "absent", "", ""},
		{"mkdir", "/work/d1/d2", "", "absent", "", ""},
		{"rename", "/work/b.txt", "/work/d1/b.txt", "file", "", ""},
		{"create", "/work/copy.txt", "", "absent", "", ""},
		{"delete", "/work/gone.txt", "", "file", "", sha("doomed")},
		{"delete", "/work/old", "", "dir", "dir", ""},
	}
	if len(ops) != len(wants) {
		t.Fatalf("%d ops, want %d: %+v", len(ops), len(wants), ops)
	}
	for i, w := range wants {
		o := ops[i]
		if o.Op != w.op || o.Path != w.path || o.ToPath != w.to || o.PreState != w.preState || o.PreReason != w.preReason {
			t.Errorf("op %d = %+v, want %+v", i, o, w)
		}
		if w.preHash != "" {
			if o.PreHash != w.preHash {
				t.Errorf("op %d pre_hash = %s, want %s", i, o.PreHash, w.preHash)
			}
			if _, err := os.Stat(filepath.Join(pre.Dir(), w.preHash)); err != nil {
				t.Errorf("op %d preimage blob: %v", i, err)
			}
		}
		if o.AuditID == 0 {
			t.Errorf("op %d is not linked to its audit row: %+v", i, o)
		}
		if o.SessionID != sess.ID {
			t.Errorf("op %d belongs to %s, not the caller's session %s", i, o.SessionID, sess.ID)
		}
	}
	// Content writes carry the hash of what they wrote; the rename carries
	// nothing.
	if ops[0].PostVersion != agent.ContentVersion([]byte("bye")) || ops[2].PostVersion != agent.ContentVersion([]byte("fresh!")) ||
		ops[3].PostVersion != agent.ContentVersion([]byte("edited me")) || ops[6].PostVersion != "" {
		t.Errorf("post versions: %q %q %q %q", ops[0].PostVersion, ops[2].PostVersion, ops[3].PostVersion, ops[6].PostVersion)
	}
	// The audit link points at the row of the very call.
	rows, err := st.AuditForSession(ctx, sess.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]agent.AuditRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if r := byID[ops[0].AuditID]; r.Tool != "write_file" || r.Result != "ok" {
		t.Errorf("audit row of the first op: %+v", r)
	}
	if r := byID[ops[8].AuditID]; r.Tool != "delete" {
		t.Errorf("audit row of the delete: %+v", r)
	}
	if sess2 := currentSession(t, st); sess2.OpsCount != len(wants) {
		t.Errorf("ops_count = %d", sess2.OpsCount)
	}
}

func TestRollbackSessionNeedsConfirm(t *testing.T) {
	e, st, _ := newPreimageEnv(t, Options{}, agent.Scope{})
	e.fake.Seed("work/a.txt", []byte("hello"))
	if res := e.call(t, "write_file", map[string]any{"path": "/work/a.txt", "content": "bye"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	sess := currentSession(t, st)
	res := e.call(t, "rollback_session", map[string]any{"session_id": sess.ID}, nil)
	if !res.IsError || !strings.Contains(errText(res), "confirm") {
		t.Fatalf("without confirm: %v %s", res.IsError, errText(res))
	}
	if got, _ := e.fs.ReadFileRange(context.Background(), "/work/a.txt", 0, 0); string(got) != "bye" {
		t.Fatalf("a refused rollback changed the file: %q", got)
	}
	// dry_run needs no confirm and changes nothing.
	var plan agent.Plan
	res = e.call(t, "rollback_session", map[string]any{"session_id": sess.ID, "dry_run": true}, &plan)
	if res.IsError {
		t.Fatal(errText(res))
	}
	if !plan.DryRun || len(plan.Restored) != 1 || plan.Restored[0].Path != "/work/a.txt" || plan.RollbackSessionID != "" {
		t.Fatalf("dry run plan: %+v", plan)
	}
	if got, _ := e.fs.ReadFileRange(context.Background(), "/work/a.txt", 0, 0); string(got) != "bye" {
		t.Fatalf("dry run changed the file: %q", got)
	}
	if s, _ := agent.NewSessions(st, agent.SessionOptions{}).Get(context.Background(), sess.ID); s.State != "active" {
		t.Fatalf("dry run changed the session: %+v", s)
	}
	res = e.call(t, "rollback_session", map[string]any{"session_id": sess.ID, "confirm": true}, &plan)
	if res.IsError {
		t.Fatal(errText(res))
	}
	if plan.DryRun || len(plan.Restored) != 1 || plan.RollbackSessionID == "" {
		t.Fatalf("plan: %+v", plan)
	}
	if got, _ := e.fs.ReadFileRange(context.Background(), "/work/a.txt", 0, 0); string(got) != "hello" {
		t.Fatalf("after rollback: %q", got)
	}
	if s, _ := agent.NewSessions(st, agent.SessionOptions{}).Get(context.Background(), sess.ID); s.State != "rolled_back" || s.RolledBackAt.IsZero() {
		t.Fatalf("session after rollback: %+v", s)
	}
	// The rollback's own writes are the rollback session's, recorded with
	// preimages, and belong to the caller's principal.
	rb, err := agent.NewSessions(st, agent.SessionOptions{}).Get(context.Background(), plan.RollbackSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if rb.PrincipalID != sess.PrincipalID || rb.OpsCount != 1 || rb.State != "finished" {
		t.Fatalf("rollback session: %+v", rb)
	}
	if res := e.call(t, "rollback_session", map[string]any{"session_id": "nope", "confirm": true}, nil); !res.IsError {
		t.Fatal("unknown session accepted")
	}
}

func TestRollbackSessionRestoresContentWithoutRedownload(t *testing.T) {
	e, st, _ := newPreimageEnv(t, Options{}, agent.Scope{})
	original := make([]byte, 1<<20)
	for i := range original {
		original[i] = byte('a' + i%26)
	}
	e.fake.Seed("work/big.txt", original)
	ctx := context.Background()
	// The file is cached: read it once through the normal path.
	if got, err := e.fs.ReadFileRange(ctx, "/work/big.txt", 0, 0); err != nil || len(got) != len(original) {
		t.Fatalf("warm read: %d %v", len(got), err)
	}
	reads, links := e.fake.Calls("ReadRange"), e.fake.Calls("DownloadURL")
	if res := e.call(t, "write_file", map[string]any{"path": "/work/big.txt", "content": "replaced"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	sess := currentSession(t, st)
	var plan agent.Plan
	if res := e.call(t, "rollback_session", map[string]any{"session_id": sess.ID, "confirm": true}, &plan); res.IsError {
		t.Fatal(errText(res))
	}
	if len(plan.Restored) != 1 {
		t.Fatalf("plan: %+v", plan)
	}
	var out readTextOutput
	if res := e.call(t, "read_text", map[string]any{"path": "/work/big.txt", "max_bytes": 32}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if out.Size != int64(len(original)) || out.Content != string(original[:32]) {
		t.Fatalf("read after rollback: size %d content %q", out.Size, out.Content)
	}
	if got, _ := e.fs.ReadFileRange(ctx, "/work/big.txt", 0, 0); string(got) != string(original) {
		t.Fatal("content after rollback differs from the original")
	}
	if d := e.fake.Calls("ReadRange") - reads; d != 0 {
		t.Errorf("rollback of a cached file downloaded: %d ReadRange calls", d)
	}
	if d := e.fake.Calls("DownloadURL") - links; d != 0 {
		t.Errorf("rollback of a cached file asked for %d download links", d)
	}
	// The restored content reaches the provider through the upload queue.
	e.drain(t)
	if got, _ := e.fake.Content("work/big.txt"); string(got) != string(original) {
		t.Fatalf("provider content after rollback: %d bytes", len(got))
	}
}

func TestRollbackSessionOnUncachedFileDownloadsOnce(t *testing.T) {
	e, st, _ := newPreimageEnv(t, Options{}, agent.Scope{})
	original := make([]byte, 300<<10)
	for i := range original {
		original[i] = byte('0' + i%10)
	}
	e.fake.Seed("work/cold.txt", original)
	ctx := context.Background()
	// Warm the listing only, so the content is not cached.
	e.listDirs(t, "/", "/work")
	reads, bytes := e.fake.Calls("ReadRange"), e.fake.ReadBytes()
	if res := e.call(t, "write_file", map[string]any{"path": "/work/cold.txt", "content": "replaced"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	afterWrite := e.fake.Calls("ReadRange")
	if afterWrite-reads < 1 {
		t.Fatalf("the preimage of an uncached file needs one download; got %d ReadRange calls", afterWrite-reads)
	}
	if got := e.fake.ReadBytes() - bytes; got > int64(len(original)) {
		t.Fatalf("the preimage read %d bytes of a %d byte file", got, len(original))
	}
	sess := currentSession(t, st)
	var plan agent.Plan
	if res := e.call(t, "rollback_session", map[string]any{"session_id": sess.ID, "confirm": true}, &plan); res.IsError {
		t.Fatal(errText(res))
	}
	if len(plan.Restored) != 1 {
		t.Fatalf("plan: %+v", plan)
	}
	if d := e.fake.Calls("ReadRange") - afterWrite; d != 0 {
		t.Errorf("the rollback itself downloaded: %d ReadRange calls", d)
	}
	if got, _ := e.fs.ReadFileRange(ctx, "/work/cold.txt", 0, 0); string(got) != string(original) {
		t.Fatal("content after rollback differs from the original")
	}
}

func TestRollbackSessionReportsConflictsWithoutOverwriting(t *testing.T) {
	e, st, _ := newPreimageEnv(t, Options{}, agent.Scope{})
	e.fake.Seed("work/a.txt", []byte("a0"))
	e.fake.Seed("work/b.txt", []byte("b0"))
	ctx := context.Background()
	for _, p := range []string{"/work/a.txt", "/work/b.txt"} {
		if res := e.call(t, "write_file", map[string]any{"path": p, "content": "by agent"}, nil); res.IsError {
			t.Fatal(errText(res))
		}
	}
	// Someone edits a.txt outside the session.
	if _, err := e.fs.WriteFile(ctx, "/work/a.txt", []byte("by hand"), false); err != nil {
		t.Fatal(err)
	}
	sess := currentSession(t, st)
	var plan agent.Plan
	if res := e.call(t, "rollback_session", map[string]any{"session_id": sess.ID, "confirm": true}, &plan); res.IsError {
		t.Fatal(errText(res))
	}
	if len(plan.Conflict) != 1 || plan.Conflict[0].Path != "/work/a.txt" || plan.Conflict[0].Reason != "modified" || len(plan.Restored) != 1 {
		t.Fatalf("plan: %+v", plan)
	}
	if got, _ := e.fs.ReadFileRange(ctx, "/work/a.txt", 0, 0); string(got) != "by hand" {
		t.Fatalf("the conflicting file was overwritten: %q", got)
	}
	if got, _ := e.fs.ReadFileRange(ctx, "/work/b.txt", 0, 0); string(got) != "b0" {
		t.Fatalf("b.txt after rollback: %q", got)
	}
}

func TestWriteToolsWithoutPreimagesRecordNothing(t *testing.T) {
	e, st := newAgentEnvWithoutPreimages(t, Options{}, agent.Scope{})
	e.fake.Seed("work/a.txt", []byte("hello"))
	if res := e.call(t, "write_file", map[string]any{"path": "/work/a.txt", "content": "bye"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	sess := currentSession(t, st)
	if ops, _ := st.OpsOf(context.Background(), sess.ID); len(ops) != 0 {
		t.Fatalf("ops without a preimage store: %+v", ops)
	}
	tools, err := e.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "rollback_session" {
			t.Fatal("rollback_session is registered without a preimage store")
		}
	}
}
