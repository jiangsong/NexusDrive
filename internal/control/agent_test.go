package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
)

// agentFixture wires an agent store into the collector the way the daemon
// does, through NewAgentView.
func agentFixture(t *testing.T) (*fixture, *agent.Store, *agent.Sessions) {
	t.Helper()
	f := newFixture(t)
	st, err := agent.Open(filepath.Join(f.dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := agent.NewSessions(st, agent.SessionOptions{})
	f.coll.Agent = NewAgentView(st, m, "", RollbackDeps{})
	return f, st, m
}

// uiCallControl sends one request the way the page does: Host cloudfs, and
// the control header on anything but a GET.
func uiCallControl(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "http://cloudfs"+target, nil)
	} else {
		r = httptest.NewRequest(method, "http://cloudfs"+target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		r.Header.Set("X-CloudFS-Control", "1")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func openSession(t *testing.T, m *agent.Sessions, key, client string) agent.Session {
	t.Helper()
	p, err := m.EnsurePrincipal(context.Background(), "stdio", "local", agent.Scope{Read: []string{"/work"}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Resolve(context.Background(), agent.ConnInfo{Key: key, Transport: "stdio", PrincipalID: p.ID, ClientName: client})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAuditRouteFollowsCursorAndFilters(t *testing.T) {
	f, st, _ := agentFixture(t)
	for i := 0; i < 5; i++ {
		res := "ok"
		if i%2 == 0 {
			res = "denied"
		}
		if _, err := st.AppendAudit(context.Background(), agent.AuditRow{TS: time.Now(), Tool: "stat", Paths: []string{"/a"}, Args: json.RawMessage(`{}`), Result: res}); err != nil {
			t.Fatal(err)
		}
	}
	h := NewServer(f.coll).Handler()
	w := uiCallControl(t, h, "GET", "/audit?limit=2", "")
	var page AuditResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != 200 || len(page.Rows) != 2 || page.NextCursor == "" {
		t.Fatalf("%d %s %v", w.Code, w.Body, err)
	}
	if page.Rows[0].ID < page.Rows[1].ID {
		t.Fatalf("rows are not newest first: %+v", page.Rows)
	}
	w = uiCallControl(t, h, "GET", "/audit?limit=10&cursor="+url.QueryEscape(page.NextCursor), "")
	var second AuditResponse
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil || len(second.Rows) != 3 || second.NextCursor != "" {
		t.Fatalf("second page: %d %s", w.Code, w.Body)
	}
	w = uiCallControl(t, h, "GET", "/audit?result=denied", "")
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Rows) != 3 {
		t.Fatalf("denied filter: %d %s", w.Code, w.Body)
	}
	// The two results the agent-first middleware added are filters the
	// console offers, so the route accepts them (rows or not).
	for _, result := range []string{"forwarded", "oversize"} {
		w = uiCallControl(t, h, "GET", "/audit?result="+result, "")
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != 200 {
			t.Fatalf("%s filter: %d %s", result, w.Code, w.Body)
		}
	}
	w = uiCallControl(t, h, "GET", "/audit?tool=nothing", "")
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Rows) != 0 || !strings.Contains(w.Body.String(), `"rows":[]`) {
		t.Fatalf("tool filter: %d %s", w.Code, w.Body)
	}
	w = uiCallControl(t, h, "GET", "/audit?since="+url.QueryEscape(time.Now().Add(time.Hour).UTC().Format(time.RFC3339)), "")
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Rows) != 0 {
		t.Fatalf("since filter: %d %s", w.Code, w.Body)
	}
	for _, bad := range []string{"/audit?result=maybe", "/audit?since=yesterday", "/audit?limit=0", "/audit?limit=x", "/audit?cursor=nope"} {
		if w := uiCallControl(t, h, "GET", bad, ""); w.Code != 400 {
			t.Fatalf("%s: %d %s", bad, w.Code, w.Body)
		}
	}
	if w := uiCallControl(t, h, "POST", "/audit", "{}"); w.Code != 405 {
		t.Fatalf("POST /audit: %d", w.Code)
	}
}

func TestAuditRowsCarryTheSessionsClientName(t *testing.T) {
	f, st, m := agentFixture(t)
	s := openSession(t, m, "stdio:1", "codex")
	if _, err := st.AppendAudit(context.Background(), agent.AuditRow{SessionID: s.ID, Tool: "delete", Paths: []string{"/x"}, Result: "denied"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendAudit(context.Background(), agent.AuditRow{Tool: "initialize", Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/audit", "")
	var page AuditResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Rows) != 2 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if page.Rows[0].Client != "" || page.Rows[1].Client != "codex" || page.Rows[1].SessionID != s.ID {
		t.Fatalf("client names: %+v", page.Rows)
	}
	if page.Rows[1].Paths[0] != "/x" || page.Rows[1].Result != "denied" || string(page.Rows[1].Args) != "{}" {
		t.Fatalf("denied row: %+v", page.Rows[1])
	}
}

func TestSessionsRouteListsShowsAndFinishes(t *testing.T) {
	f, st, m := agentFixture(t)
	s := openSession(t, m, "stdio:1", "codex")
	for _, tool := range []string{"write_file", "read_text", "delete"} {
		if _, err := st.AppendAudit(context.Background(), agent.AuditRow{SessionID: s.ID, Tool: tool, Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	h := NewServer(f.coll).Handler()
	w := uiCallControl(t, h, "GET", "/sessions", "")
	var list SessionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != 200 || len(list.Sessions) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	got := list.Sessions[0]
	if got.Client != "codex" || got.State != "active" || got.Writes != 2 || got.FinishedAt != nil || list.Summary.Active != 1 || list.Summary.WritesToday != 2 {
		t.Fatalf("listing: %+v summary %+v", got, list.Summary)
	}
	if !strings.Contains(w.Body.String(), `"read":["/work"]`) {
		t.Fatalf("scope missing: %s", w.Body)
	}
	w = uiCallControl(t, h, "GET", "/sessions/"+s.ID, "")
	var detail SessionDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if detail.Session.ID != s.ID || len(detail.Audit) != 3 || detail.Audit[0].Tool != "delete" || detail.Audit[0].Client != "codex" || detail.Artifacts == nil {
		t.Fatalf("detail: %s", w.Body)
	}
	w = uiCallControl(t, h, "POST", "/sessions/"+s.ID+"/finish", `{"summary":"done"}`)
	var finished SessionView
	if err := json.Unmarshal(w.Body.Bytes(), &finished); err != nil || w.Code != 200 || finished.State != "finished" || finished.Summary != "done" || finished.FinishedAt == nil {
		t.Fatalf("finish: %d %s", w.Code, w.Body)
	}
	w = uiCallControl(t, h, "GET", "/sessions?state=active", "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Sessions) != 0 {
		t.Fatalf("active filter after finish: %d %s", w.Code, w.Body)
	}
	for _, target := range []string{"/sessions/nope", "/sessions/nope/finish"} {
		method, body := "GET", ""
		if strings.HasSuffix(target, "/finish") {
			method, body = "POST", "{}"
		}
		if w := uiCallControl(t, h, method, target, body); w.Code != 404 {
			t.Fatalf("%s %s: %d %s", method, target, w.Code, w.Body)
		}
	}
	for _, bad := range []string{"/sessions?state=paused", "/sessions?sandbox=maybe", "/sessions?limit=0", "/sessions?cursor=nope"} {
		if w := uiCallControl(t, h, "GET", bad, ""); w.Code != 400 {
			t.Fatalf("%s: %d %s", bad, w.Code, w.Body)
		}
	}
	if w := uiCallControl(t, h, "GET", "/sessions/"+s.ID+"/finish", ""); w.Code != 405 {
		t.Fatalf("GET finish: %d", w.Code)
	}
	if w := uiCallControl(t, h, "GET", "/sessions/a/b", ""); w.Code != 404 {
		t.Fatalf("nested path: %d", w.Code)
	}
}

func TestSessionsRoutePagesByCursor(t *testing.T) {
	f, _, m := agentFixture(t)
	for i := 0; i < 3; i++ {
		openSession(t, m, "stdio:"+string(rune('a'+i)), "codex")
	}
	h := NewServer(f.coll).Handler()
	w := uiCallControl(t, h, "GET", "/sessions?limit=2", "")
	var page SessionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Sessions) != 2 || page.NextCursor == "" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	w = uiCallControl(t, h, "GET", "/sessions?limit=2&cursor="+url.QueryEscape(page.NextCursor), "")
	var second SessionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil || len(second.Sessions) != 1 || second.NextCursor != "" {
		t.Fatalf("second page: %d %s", w.Code, w.Body)
	}
}

func TestFinishSessionWithoutControlHeaderIs403(t *testing.T) {
	f, _, _ := agentFixture(t)
	req := httptest.NewRequest("POST", "http://cloudfs/sessions/x/finish", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	NewServer(f.coll).Handler().ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestAgentRoutesAnswer503WithoutAStore(t *testing.T) {
	f := newFixture(t)
	h := NewServer(f.coll).Handler()
	for _, target := range []string{"/audit", "/sessions", "/sessions/x"} {
		if w := uiCallControl(t, h, "GET", target, ""); w.Code != 503 {
			t.Fatalf("%s %d", target, w.Code)
		}
	}
	if w := uiCallControl(t, h, "POST", "/sessions/x/finish", "{}"); w.Code != 503 {
		t.Fatalf("finish: %d", w.Code)
	}
	// The status document and the metrics simply lack the agent line.
	if w := uiCallControl(t, h, "GET", "/status", ""); w.Code != 200 || strings.Contains(w.Body.String(), `"agent"`) {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
	if w := uiCallControl(t, h, "GET", "/metrics", ""); strings.Contains(w.Body.String(), "cloudfs_audit_write_failures_total") {
		t.Fatal("metrics report an audit trail that does not exist")
	}
}

func TestAgentRoutesAreRegistered(t *testing.T) {
	f, _, _ := agentFixture(t)
	want := map[string]bool{"/audit": false, "/sessions": false, "/sessions/": false}
	for _, r := range NewServer(f.coll).routes() {
		if _, ok := want[r.pattern]; ok {
			want[r.pattern] = true
			if r.open {
				t.Errorf("%s is marked open; every agent route names local state", r.pattern)
			}
		}
	}
	for pattern, found := range want {
		if !found {
			t.Errorf("%s is not registered in routes()", pattern)
		}
	}
}

func TestStatusReportsActiveSessions(t *testing.T) {
	f, _, m := agentFixture(t)
	openSession(t, m, "stdio:1", "codex")
	w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/status", "")
	var st Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if st.Agent == nil || st.Agent.ActiveSessions != 1 {
		t.Fatalf("agent status: %+v", st.Agent)
	}
}

func TestAuditWriteFailuresMetric(t *testing.T) {
	f, _, _ := agentFixture(t)
	w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/metrics", "")
	body := w.Body.String()
	for _, want := range []string{"cloudfs_audit_write_failures_total 0\n", "cloudfs_agent_sessions_active 0\n", "# TYPE cloudfs_audit_write_failures_total counter"} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics lack %q:\n%s", want, body)
		}
	}
}

func TestAgentClientHelpersRoundTrip(t *testing.T) {
	f, st, m := agentFixture(t)
	s := openSession(t, m, "stdio:1", "codex")
	if _, err := st.AppendAudit(context.Background(), agent.AuditRow{SessionID: s.ID, Tool: "stat", Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	sock := socketPath(t)
	running, err := NewServer(f.coll).Start(context.Background(), sock, "")
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()
	ctx := context.Background()
	audit, online, err := CallAudit(ctx, sock, "", agent.AuditQuery{Session: s.ID, Limit: 5})
	if err != nil || !online || len(audit.Rows) != 1 || audit.Rows[0].Client != "codex" {
		t.Fatalf("audit: %+v %v %v", audit, online, err)
	}
	list, online, err := CallSessions(ctx, sock, "", agent.ListQuery{State: "active"})
	if err != nil || !online || len(list.Sessions) != 1 {
		t.Fatalf("sessions: %+v %v %v", list, online, err)
	}
	detail, online, err := CallSession(ctx, sock, "", s.ID)
	if err != nil || !online || detail.Session.ID != s.ID || len(detail.Audit) != 1 {
		t.Fatalf("session: %+v %v %v", detail, online, err)
	}
	finished, online, err := CallFinishSession(ctx, sock, "", s.ID, "wrapped up")
	if err != nil || !online || finished.State != "finished" || finished.Summary != "wrapped up" {
		t.Fatalf("finish: %+v %v %v", finished, online, err)
	}
	if _, _, err := CallSession(ctx, sock, "", "nope"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("unknown session: %v", err)
	}
	// socketPath, not t.TempDir: the offline check needs a path that is merely
	// absent. A t.TempDir name derived from this test's own name overruns the
	// 104-byte sun_path limit on macOS, and the dial then fails with EINVAL
	// rather than the "nothing is listening" errno the offline classification
	// recognises — a fixture bug that reads as a product one.
	if _, online, err := CallAudit(ctx, filepath.Join(filepath.Dir(socketPath(t)), "absent.sock"), "", agent.AuditQuery{}); err != nil || online {
		t.Fatalf("no daemon: online=%v err=%v", online, err)
	}
}

// beginSession opens an explicit session with a workspace the way the MCP
// begin_session tool does.
func beginSession(t *testing.T, m *agent.Sessions, key, client string, sandbox bool) agent.Session {
	t.Helper()
	implicit := openSession(t, m, key, client)
	s, err := m.Begin(context.Background(), implicit, agent.BeginOptions{Sandbox: sandbox, Workspace: "/work/.agent"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSessionsByPathFindsTheSession(t *testing.T) {
	f, _, m := agentFixture(t)
	s := beginSession(t, m, "stdio:1", "codex", false)
	beginSession(t, m, "stdio:2", "claude", false)
	h := NewServer(f.coll).Handler()
	var list SessionsResponse
	w := uiCallControl(t, h, "GET", "/sessions?path="+url.QueryEscape(s.Workspace+"/a.md"), "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != s.ID || list.Sessions[0].Workspace != s.Workspace {
		t.Fatalf("%+v", list.Sessions)
	}
	w = uiCallControl(t, h, "GET", "/sessions?path="+url.QueryEscape("/work/.agent"), "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Sessions) != 0 {
		t.Fatalf("the workspace root belongs to no session: %d %s", w.Code, w.Body)
	}
}

func TestSessionsSandboxFilter(t *testing.T) {
	f, _, m := agentFixture(t)
	boxed := beginSession(t, m, "stdio:1", "codex", true)
	beginSession(t, m, "stdio:2", "claude", false)
	h := NewServer(f.coll).Handler()
	var list SessionsResponse
	w := uiCallControl(t, h, "GET", "/sessions?sandbox=1", "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != boxed.ID || !list.Sessions[0].Sandbox || list.Sessions[0].Scope.Sandbox != boxed.Workspace {
		t.Fatalf("%+v", list.Sessions)
	}
	w = uiCallControl(t, h, "GET", "/sessions", "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Sessions) != 4 {
		t.Fatalf("all sessions: %d %s", w.Code, w.Body)
	}
}

func TestSessionDetailListsArtifacts(t *testing.T) {
	f, _, m := agentFixture(t)
	s := beginSession(t, m, "stdio:1", "codex", false)
	arts := []agent.Artifact{
		{Path: s.Workspace + "/report.md", URI: "cloudfs://ali" + s.Workspace + "/report.md", Size: 10, State: "synced"},
		{Path: s.Workspace + "/data.csv", URI: "cloudfs://ali" + s.Workspace + "/data.csv", Size: 20, State: "local"},
	}
	if _, err := m.FinishWith(context.Background(), s.ID, "done", arts); err != nil {
		t.Fatal(err)
	}
	h := NewServer(f.coll).Handler()
	w := uiCallControl(t, h, "GET", "/sessions/"+s.ID, "")
	var detail SessionDetail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(detail.Artifacts) != 2 || detail.Artifacts[0].Path != arts[0].Path || detail.Artifacts[1].State != "local" {
		t.Fatalf("%+v", detail.Artifacts)
	}
	if detail.Session.ArtifactCount != 2 || detail.Session.Summary != "done" || detail.Session.State != "finished" {
		t.Fatalf("%+v", detail.Session)
	}
	if raw, _ := json.Marshal(detail.Artifacts); strings.Contains(string(raw), "0001-01-01") {
		t.Fatalf("an artifact without a link carries a zero expiry: %s", raw)
	}
	var list SessionsResponse
	w = uiCallControl(t, h, "GET", "/sessions?state=finished", "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Sessions) != 2 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for _, sv := range list.Sessions {
		if sv.ID == s.ID && sv.ArtifactCount != 2 {
			t.Fatalf("listing artifact count: %+v", sv)
		}
	}
}
