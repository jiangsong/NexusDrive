package control

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/agent"
	"cloudfs/test/fakeprovider"
)

// rollbackFixture is cacheControl with a writable VFS, an agent store and
// a preimage store wired into the agent view, the way the daemon wires
// them. The returned session has one recorded overwrite of /docs/a.
func rollbackFixture(t *testing.T) (*fixture, *fakeprovider.Fake, *agent.Store, *agent.Sessions, agent.Session) {
	t.Helper()
	f, p := cacheControl(t)
	f.coll.FS.SetWriteBackend(f.j, nil)
	st, err := agent.Open(filepath.Join(f.dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := agent.NewSessions(st, agent.SessionOptions{})
	pre, err := agent.NewPreimages(st, filepath.Join(f.dir, "agent", "preimages"), f.cache, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	f.coll.Agent = NewAgentView(st, m, "", RollbackDeps{FS: agent.VFSOps(f.coll.FS), Preimages: pre})
	s := openSession(t, m, "stdio:1", "codex")
	ctx := context.Background()
	fs := agent.VFSOps(f.coll.FS)
	captured, err := pre.Capture(ctx, fs, "/docs/a")
	if err != nil {
		t.Fatal(err)
	}
	seq, err := pre.Record(ctx, s.ID, agent.Op{Op: "overwrite", Path: "/docs/a"}.WithPre(captured))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.WriteFile(ctx, "/docs/a", []byte("by agent"), false); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteOp(ctx, seq, agent.ContentVersion([]byte("by agent"))); err != nil {
		t.Fatal(err)
	}
	return f, p, st, m, s
}

func TestRollbackRouteDryRunThenConfirm(t *testing.T) {
	f, _, st, m, s := rollbackFixture(t)
	ctx := context.Background()
	h := NewServer(f.coll).Handler()
	rowsBefore, err := f.j.All(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The detail lists the op with its preimage state.
	w := uiCallControl(t, h, "GET", "/sessions/"+s.ID, "")
	var detail SessionDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(detail.Ops) != 1 || detail.Ops[0].Op != "overwrite" || detail.Ops[0].Path != "/docs/a" || detail.Ops[0].PreState != "file" || detail.Ops[0].PreReason != "" {
		t.Fatalf("ops: %+v", detail.Ops)
	}
	if detail.Session.OpsCount != 1 || strings.Contains(w.Body.String(), "pre_blob") {
		t.Fatalf("session view: %s", w.Body)
	}

	// Neither flag: refused, with the consequence named.
	w = uiCallControl(t, h, "POST", "/sessions/"+s.ID+"/rollback", `{}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "confirm=true") {
		t.Fatalf("no flags: %d %s", w.Code, w.Body)
	}
	// Dry run: a plan, no writes, no state change.
	w = uiCallControl(t, h, "POST", "/sessions/"+s.ID+"/rollback", `{"dry_run":true}`)
	var plan agent.Plan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil || w.Code != 200 {
		t.Fatalf("dry run: %d %s", w.Code, w.Body)
	}
	if !plan.DryRun || len(plan.Restored) != 1 || plan.Restored[0].Path != "/docs/a" || plan.RollbackSessionID != "" {
		t.Fatalf("dry run plan: %+v", plan)
	}
	if !strings.Contains(w.Body.String(), `"skipped":[]`) || !strings.Contains(w.Body.String(), `"conflict":[]`) {
		t.Fatalf("empty groups must be arrays: %s", w.Body)
	}
	if got, _ := f.coll.FS.ReadFileRange(ctx, "/docs/a", 0, 0); string(got) != "by agent" {
		t.Fatalf("dry run changed the file: %q", got)
	}
	rowsAfterDry, err := f.j.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsAfterDry) != len(rowsBefore) {
		t.Fatalf("dry run touched the journal: %d -> %d rows", len(rowsBefore), len(rowsAfterDry))
	}
	if got, _ := m.Get(ctx, s.ID); got.State != "active" {
		t.Fatalf("dry run changed the session: %+v", got)
	}
	// Confirm: executed, the file is back, the session is rolled_back.
	w = uiCallControl(t, h, "POST", "/sessions/"+s.ID+"/rollback", `{"confirm":true}`)
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil || w.Code != 200 {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	if plan.DryRun || len(plan.Restored) != 1 || plan.RollbackSessionID == "" {
		t.Fatalf("plan: %+v", plan)
	}
	if got, _ := f.coll.FS.ReadFileRange(ctx, "/docs/a", 0, 0); string(got) != "cached content" {
		t.Fatalf("after rollback: %q", got)
	}
	w = uiCallControl(t, h, "GET", "/sessions/"+s.ID, "")
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if detail.Session.State != "rolled_back" || detail.Session.RolledBackAt == nil || !detail.Ops[0].RolledBack || detail.Ops[0].RollbackResult != "restored" {
		t.Fatalf("after rollback: %s", w.Body)
	}
	// The rollback session is the console's and shows up in the listing
	// under the rolled_back filter's sibling states.
	rb, err := m.Get(ctx, plan.RollbackSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if rb.Transport != "console" || rb.State != "finished" || rb.OpsCount != 1 {
		t.Fatalf("rollback session: %+v", rb)
	}
	if p, err := m.Principal(ctx, rb.PrincipalID); err != nil || p.Kind != "console" {
		t.Fatalf("rollback principal: %+v %v", p, err)
	}
	var list SessionsResponse
	w = uiCallControl(t, h, "GET", "/sessions?state=rolled_back", "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != 200 || len(list.Sessions) != 1 || list.Sessions[0].ID != s.ID {
		t.Fatalf("rolled_back filter: %d %s", w.Code, w.Body)
	}
	// Method, unknown session and a GET on the rollback path.
	if w := uiCallControl(t, h, "GET", "/sessions/"+s.ID+"/rollback", ""); w.Code != 405 {
		t.Fatalf("GET rollback: %d", w.Code)
	}
	if w := uiCallControl(t, h, "POST", "/sessions/nope/rollback", `{"dry_run":true}`); w.Code != 404 {
		t.Fatalf("unknown session: %d %s", w.Code, w.Body)
	}
	if got, _ := st.OpsOf(ctx, s.ID); len(got) != 1 {
		t.Fatalf("ops: %+v", got)
	}
}

func TestRollbackRouteWithoutADaemonIs503(t *testing.T) {
	f, st, m := agentFixture(t)
	s := openSession(t, m, "stdio:1", "codex")
	_ = st
	h := NewServer(f.coll).Handler()
	if w := uiCallControl(t, h, "POST", "/sessions/"+s.ID+"/rollback", `{"dry_run":true}`); w.Code != 503 {
		t.Fatalf("rollback without a VFS: %d %s", w.Code, w.Body)
	}
}

func TestSessionsByPathListsTouchingSessions(t *testing.T) {
	f, _, _, m, s := rollbackFixture(t)
	h := NewServer(f.coll).Handler()
	// A second session with a workspace but no op on the file.
	other := beginSession(t, m, "stdio:2", "claude", false)
	var list SessionsResponse
	w := uiCallControl(t, h, "GET", "/sessions?path="+url.QueryEscape("/docs/a"), "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != s.ID || list.Sessions[0].OpsCount != 1 {
		t.Fatalf("touching sessions: %+v", list.Sessions)
	}
	// The workspace lookup still answers for a path inside a session
	// directory.
	w = uiCallControl(t, h, "GET", "/sessions?path="+url.QueryEscape(other.Workspace+"/report.md"), "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Sessions) != 1 || list.Sessions[0].ID != other.ID {
		t.Fatalf("workspace lookup: %d %s", w.Code, w.Body)
	}
	// since in the future finds nothing; an invalid since is a 400.
	w = uiCallControl(t, h, "GET", "/sessions?path="+url.QueryEscape("/docs/a")+"&since=2200-01-01T00:00:00Z", "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Sessions) != 0 {
		t.Fatalf("since: %d %s", w.Code, w.Body)
	}
	if w := uiCallControl(t, h, "GET", "/sessions?since=yesterday", ""); w.Code != 400 {
		t.Fatalf("bad since: %d", w.Code)
	}
	w = uiCallControl(t, h, "GET", "/sessions?path="+url.QueryEscape("/docs/b"), "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Sessions) != 0 {
		t.Fatalf("untouched path: %d %s", w.Code, w.Body)
	}
}
