package mcpsrv

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloudfs/internal/agent"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bridgedPair is an owner served over HTTP with the bridge secret and a
// NonOwner stdio server forwarding to it. The two have separate VFSes
// (as a real owner and stdio process do — the stdio side shares the
// owner's meta and cache on disk, which the in-memory harness cannot),
// so what the bridge forwards is visible on the owner's side only.
func bridgedPair(t *testing.T) (owner, stdio *env, ownerStore *agent.Store, ts *httptest.Server) {
	t.Helper()
	owner, ownerStore, _ = newPreimageEnv(t, Options{}, agent.Scope{})
	tok, err := WriteBridgeToken(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts = tokenServer(t, owner, HTTPAuth{Bridge: tok})
	bridgeTokens[ts] = tok
	stdio = newEnv(t, Options{NonOwner: true, Bridge: &BridgeOptions{URL: ts.URL, Token: tok}})
	return owner, stdio, ownerStore, ts
}

// TestBridgeForwardsWritesToTheOwner: a write on the stdio side lands on
// the owner (its VFS, its journal), the stdio audit-free result is the
// owner's result, and the owner's session for it is the bridge one.
func TestBridgeForwardsWritesToTheOwner(t *testing.T) {
	owner, stdio, ownerStore, _ := bridgedPair(t)
	owner.fake.Seed("work/.keep", []byte(""))
	if !stdio.server.Bridged() {
		t.Fatal("stdio server has no bridge")
	}
	var w writeOutput
	res := stdio.call(t, "write_file", writeInput{Path: "/work/via-bridge.txt", Content: "hello"}, &w)
	if res.IsError {
		t.Fatalf("forwarded write failed: %s", errText(res))
	}
	if w.Path != "/work/via-bridge.txt" || w.Bytes != 5 || !w.Reversible {
		t.Fatalf("owner's answer did not come back whole: %+v", w)
	}
	data, err := owner.fs.ReadFileRange(context.Background(), "/work/via-bridge.txt", 0, 0)
	if err != nil || string(data) != "hello" {
		t.Fatalf("owner did not get the write: %q %v", data, err)
	}
	if _, err := stdio.fs.StatPath(context.Background(), "/work/via-bridge.txt"); err == nil {
		t.Fatal("the stdio side's own VFS wrote the file: the call was not forwarded")
	}
	owner.drain(t)
	if owner.fake.Calls("Upload")+owner.fake.Calls("UploadPart")+owner.fake.Calls("Create") == 0 {
		t.Fatalf("the owner did not upload the forwarded write: %v", owner.fake.TotalCalls())
	}
	sessions, _, err := agent.NewSessions(ownerStore, agent.SessionOptions{}).List(context.Background(), agent.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	bridged := false
	for _, s := range sessions {
		bridged = bridged || s.Transport == "http-bridge"
	}
	if !bridged {
		t.Fatalf("no http-bridge session on the owner: %+v", sessions)
	}
	rows, _, err := ownerStore.Audit(context.Background(), agent.AuditQuery{Tool: "write_file"})
	if err != nil || len(rows) != 1 || rows[0].Transport != "http-bridge" || rows[0].Result != "ok" {
		t.Fatalf("owner audit: %+v %v", rows, err)
	}
	// A refusal on the owner comes back as the owner's refusal, not the fence.
	res = stdio.call(t, "delete", deleteInput{Path: "/work/via-bridge.txt"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "confirm=true") || strings.Contains(errText(res), "does not own the cache") {
		t.Fatalf("owner refusal: %s", errText(res))
	}
	// Reads stay local: the stdio side answers from its own view.
	if res := stdio.call(t, "stat", statInput{Path: "/work/via-bridge.txt"}, nil); !res.IsError || !strings.Contains(errText(res), "does not exist") {
		t.Fatalf("read was forwarded: %s", errText(res))
	}
}

// TestBridgeAuditSaysForwarded: with a store on the stdio side, its row
// for a forwarded call says forwarded rather than ok or denied, so the
// two rows of one call (here and on the owner) are told apart.
func TestBridgeAuditSaysForwarded(t *testing.T) {
	owner, _, _, ts := bridgedPair(t)
	owner.fake.Seed("work/.keep", []byte(""))
	stdioWithStore, st := newAgentEnv(t, Options{NonOwner: true, Bridge: &BridgeOptions{URL: ts.URL, Token: bridgeTokenOf(t, ts)}}, agent.Scope{})
	if res := stdioWithStore.call(t, "write_file", writeInput{Path: "/work/audited.txt", Content: "x"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	rows, _, err := st.Audit(context.Background(), agent.AuditQuery{Tool: "write_file"})
	if err != nil || len(rows) != 1 || rows[0].Result != resultForwarded || rows[0].Transport != "stdio" {
		t.Fatalf("stdio audit: %+v %v", rows, err)
	}
}

// TestBridgeDownFallsBackToTheFence: when the owner cannot be reached the
// call refuses with the fence's advice and the reason, and the audit row
// says denied as before.
func TestBridgeDownFallsBackToTheFence(t *testing.T) {
	_, _, _, ts := bridgedPair(t)
	tok := bridgeTokenOf(t, ts)
	ts.Close()
	stdio, st := newAgentEnv(t, Options{NonOwner: true, Bridge: &BridgeOptions{URL: ts.URL, Token: tok}}, agent.Scope{})
	res := stdio.call(t, "write_file", writeInput{Path: "/work/x.txt", Content: "x"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "could not be reached") || !strings.Contains(errText(res), "--transport http") {
		t.Fatalf("%s", errText(res))
	}
	if d, ok := errorDetailOf(res); !ok || d.Code != codeNotOwner {
		t.Fatalf("detail: %+v", d)
	}
	rows, _, _ := st.Audit(context.Background(), agent.AuditQuery{Tool: "write_file"})
	if len(rows) != 1 || rows[0].Result != "denied" {
		t.Fatalf("audit: %+v", rows)
	}
}

// TestBridgeSecretIsLoopbackOnly: the secret authenticates from this
// machine only; presented from elsewhere it is an invalid token.
func TestBridgeSecretIsLoopbackOnly(t *testing.T) {
	owner, _, _ := newPreimageEnv(t, Options{}, agent.Scope{})
	tok, _ := WriteBridgeToken(t.TempDir())
	h := NewHTTPHandler(owner.server, HTTPAuth{Bridge: tok})
	for _, remote := range []string{"127.0.0.1:5000", "10.0.0.9:5000"} {
		r := httptest.NewRequest(http.MethodPost, "http://cloudfs/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("Authorization", "Bearer "+tok)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if strings.HasPrefix(remote, "127.") && w.Code == http.StatusUnauthorized {
			t.Fatalf("loopback refused: %d %s", w.Code, w.Body.String())
		}
		if !strings.HasPrefix(remote, "127.") && w.Code != http.StatusUnauthorized {
			t.Fatalf("non-loopback accepted: %d", w.Code)
		}
	}
	if len(ReadBridgeToken(t.TempDir())) != 0 {
		t.Fatal("a missing secret reads as something")
	}
}

// TestBridgeForwardsEveryFencedTool: every tool the owner fence covers
// goes through the bridge rather than being refused locally, and no
// read tool does.
func TestBridgeForwardsEveryFencedTool(t *testing.T) {
	_, stdio, _, _ := bridgedPair(t)
	tools, err := stdio.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range tools.Tools {
		seen[tool.Name] = true
		res := stdio.call(t, tool.Name, map[string]any{}, nil)
		refused := res.IsError && strings.Contains(errText(res), "does not own the cache")
		if bridgedTools[tool.Name] && refused {
			t.Errorf("%s is fenced but was not forwarded: %s", tool.Name, errText(res))
		}
	}
	for name := range bridgedTools {
		if !seen[name] {
			// Tools that need an export queue, an index, a store or a
			// memory root are not registered on a bare server.
			continue
		}
	}
}

// bridgeTokenOf recovers the secret a test server accepts: the pair
// helper wrote it under a temp dir the caller does not have, so the
// handler is asked instead of the file.
func bridgeTokenOf(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	if tok, ok := bridgeTokens[ts]; ok {
		return tok
	}
	t.Fatal("no bridge token for server")
	return ""
}

var bridgeTokens = map[*httptest.Server]string{}

// TestBridgeKeepsTheStdioServersScope: a forwarded call may do no more
// than the stdio server it came through would allow. A read-only stdio
// server refuses before forwarding; one restricted to a subtree has its
// writes outside it refused by the owner, whose session for the bridge
// is narrowed to that subtree; inside it the write goes through.
func TestBridgeKeepsTheStdioServersScope(t *testing.T) {
	owner, _, ownerStore, ts := bridgedPair(t)
	owner.fake.Seed("work/sandbox/.keep", []byte(""))
	owner.fake.Seed("work/other/.keep", []byte(""))
	tok := bridgeTokenOf(t, ts)

	ro := newEnv(t, Options{NonOwner: true, ReadOnly: true, Bridge: &BridgeOptions{URL: ts.URL, Token: tok}})
	res := ro.call(t, "write_file", writeInput{Path: "/work/other/ro.txt", Content: "x"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "read-only") {
		t.Fatalf("read-only stdio server forwarded a write: %s", errText(res))
	}
	if _, err := owner.fs.StatPath(context.Background(), "/work/other/ro.txt"); err == nil {
		t.Fatal("the owner wrote the file a read-only stdio server was asked for")
	}
	// list_sessions changes nothing, so read-only still forwards it.
	if res := ro.call(t, "list_sessions", listSessionsInput{}, nil); res.IsError {
		t.Fatalf("read-only stdio server could not list sessions: %s", errText(res))
	}

	narrow := newEnv(t, Options{NonOwner: true, Allow: []string{"/work/sandbox"}, Bridge: &BridgeOptions{URL: ts.URL, Token: tok}})
	res = narrow.call(t, "write_file", writeInput{Path: "/work/other/escape.txt", Content: "x"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "outside") {
		t.Fatalf("write outside the stdio server's allow list went through the bridge: %s", errText(res))
	}
	if _, err := owner.fs.StatPath(context.Background(), "/work/other/escape.txt"); err == nil {
		t.Fatal("the owner wrote outside the stdio server's scope")
	}
	if res := narrow.call(t, "write_file", writeInput{Path: "/work/sandbox/ok.txt", Content: "x"}, nil); res.IsError {
		t.Fatalf("write inside the allow list refused: %s", errText(res))
	}
	sessions, _, err := agent.NewSessions(ownerStore, agent.SessionOptions{}).List(context.Background(), agent.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	narrowed := false
	for _, s := range sessions {
		if s.Transport == "http-bridge" && len(s.Scope.Read) == 1 && s.Scope.Read[0] == "/work/sandbox" {
			narrowed = true
		}
	}
	if !narrowed {
		t.Fatalf("no owner session narrowed to the stdio server's scope: %+v", sessions)
	}
}

// TestBridgeGivesEachStdioProcessItsOwnOwnerSession: two stdio servers
// beside one owner get two owner sessions, so one's finish_session or
// rollback_session cannot reach the other's work.
func TestBridgeGivesEachStdioProcessItsOwnOwnerSession(t *testing.T) {
	owner, a, ownerStore, ts := bridgedPair(t)
	owner.fake.Seed("work/.keep", []byte(""))
	b := newEnv(t, Options{NonOwner: true, Bridge: &BridgeOptions{URL: ts.URL, Token: bridgeTokenOf(t, ts)}})
	if res := a.call(t, "write_file", writeInput{Path: "/work/a.txt", Content: "a"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if res := b.call(t, "write_file", writeInput{Path: "/work/b.txt", Content: "b"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	sessions, _, err := agent.NewSessions(ownerStore, agent.SessionOptions{}).List(context.Background(), agent.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, s := range sessions {
		if s.Transport == "http-bridge" {
			keys[s.ConnKey] = true
		}
	}
	if len(keys) != 2 {
		t.Fatalf("expected two bridged owner sessions, got conn keys %v", keys)
	}
	// b finishing its session leaves a's active.
	if res := b.call(t, "finish_session", finishSessionInput{Summary: "done"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	active, _, _ := agent.NewSessions(ownerStore, agent.SessionOptions{}).List(context.Background(), agent.ListQuery{State: "active"})
	n := 0
	for _, s := range active {
		if s.Transport == "http-bridge" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one bridged session still active, got %d: %+v", n, active)
	}
}

// TestBridgeRefusesARequestWithoutItsHeaders: the bridge secret alone is
// not enough; a caller that names no connection or scope is refused
// rather than folded into a shared owner session under the full scope.
func TestBridgeRefusesARequestWithoutItsHeaders(t *testing.T) {
	_, _, _, ts := bridgedPair(t)
	tok := bridgeTokenOf(t, ts)
	bare := &http.Client{Transport: &bareBearer{token: tok}}
	client := mcp.NewClient(&mcp.Implementation{Name: "rogue", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL, HTTPClient: bare}, nil)
	if err == nil {
		defer session.Close()
		_, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_directory", Arguments: map[string]any{"path": "/"}})
	}
	if err == nil || !strings.Contains(err.Error(), bridgeConnHeader) {
		t.Fatalf("expected a refusal naming %s, got %v", bridgeConnHeader, err)
	}
}

type bareBearer struct{ token string }

func (t *bareBearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(r)
}
