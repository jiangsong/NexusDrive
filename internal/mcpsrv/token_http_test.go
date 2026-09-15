package mcpsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/agent"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bearerRT stamps one bearer token on every request of an HTTP client.
type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func tokenServer(t *testing.T, e *env, a HTTPAuth) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(NewHTTPHandler(e.server, a))
	t.Cleanup(ts.Close)
	return ts
}

// connectWith opens an MCP client session over HTTP with the given bearer
// token. It returns the error rather than failing the test so callers on
// other goroutines can report it themselves.
func connectWith(t *testing.T, url, token string) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "token-test", Version: "0"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: url, HTTPClient: &http.Client{Transport: bearerRT{token}}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { s.Close() })
	return s, nil
}

// rawInitialize posts a bare initialize request with the given bearer
// token ("" sends no Authorization header) and returns the HTTP status.
func rawInitialize(t *testing.T, url, token string) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"0"}}}`
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func resourceURIForTest(p string) string { return resourceURI("ali", p) }

func mustCreateToken(t *testing.T, st *agent.Store, spec agent.TokenSpec) (string, agent.Principal) {
	t.Helper()
	plain, p, err := st.CreateToken(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return plain, p
}

// tokenRPC posts one legacy-protocol JSON-RPC request with a bearer token and
// returns the response and its body.
func tokenRPC(t *testing.T, endpoint, token, session, method string, params any) (*http.Response, []byte) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	req.Header.Set("Authorization", "Bearer "+token)
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func TestTwoTokensSeeDisjointTrees(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	e.fake.Seed("work/a.txt", []byte("a"))
	e.fake.Seed("gd/x.txt", []byte("x"))
	ctx := context.Background()
	ta, pa := mustCreateToken(t, st, agent.TokenSpec{Name: "a", Read: []string{"/work"}})
	tb, pb := mustCreateToken(t, st, agent.TokenSpec{Name: "b", Read: []string{"/gd"}})
	ts := tokenServer(t, e, HTTPAuth{Verify: st.VerifyToken})
	var wg sync.WaitGroup
	check := func(token, allowed, denied string) {
		defer wg.Done()
		s, err := connectWith(t, ts.URL, token)
		if err != nil {
			t.Errorf("token %s: connect: %v", token[:8], err)
			return
		}
		for i := 0; i < 10; i++ {
			ok, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "stat", Arguments: map[string]any{"path": allowed}})
			if err != nil {
				t.Errorf("token %s: stat %s: %v", token[:8], allowed, err)
				return
			}
			no, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "stat", Arguments: map[string]any{"path": denied}})
			if err != nil {
				t.Errorf("token %s: stat %s: %v", token[:8], denied, err)
				return
			}
			if ok.IsError || !no.IsError {
				t.Errorf("token %s: allowed err=%v denied err=%v", token[:8], ok.IsError, no.IsError)
			}
		}
		// The modern client's Subscribe opens a listen stream without
		// waiting for the answer, so its refusal only shows in the audit
		// trail, which the end of the test checks. The legacy
		// resources/subscribe answers in-band.
		_ = s.Subscribe(ctx, &mcp.SubscribeParams{URI: resourceURIForTest(denied)})
		resp, data := tokenRPC(t, ts.URL, token, "", "initialize", map[string]any{
			"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "legacy", "version": "0"}})
		id := resp.Header.Get("Mcp-Session-Id")
		if resp.StatusCode != 200 || id == "" {
			t.Errorf("token %s: legacy initialize: %d %s", token[:8], resp.StatusCode, data)
			return
		}
		if resp, data := tokenRPC(t, ts.URL, token, id, "resources/subscribe", map[string]string{"uri": resourceURIForTest(denied)}); resp.StatusCode != 200 || !bytes.Contains(data, []byte(`"error"`)) {
			t.Errorf("token %s subscribed outside its scope: %d %s", token[:8], resp.StatusCode, data)
		}
		if resp, data := tokenRPC(t, ts.URL, token, id, "resources/subscribe", map[string]string{"uri": resourceURIForTest(allowed)}); resp.StatusCode != 200 || bytes.Contains(data, []byte(`"error"`)) {
			t.Errorf("token %s could not subscribe inside its scope: %d %s", token[:8], resp.StatusCode, data)
		}
	}
	wg.Add(2)
	go check(ta, "/work/a.txt", "/gd/x.txt")
	go check(tb, "/gd/x.txt", "/work/a.txt")
	wg.Wait()

	// Each token ran under its own principal, and the audit trail shows the
	// refused listen streams against the right principal and path.
	sessions, _, err := e.server.opt.Sessions.List(ctx, agent.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	byPrincipal := map[string]int{}
	for _, s := range sessions {
		if s.Transport == "http-token" {
			byPrincipal[s.PrincipalID]++
		}
	}
	if byPrincipal[pa.ID] != 1 || byPrincipal[pb.ID] != 1 || len(byPrincipal) != 2 {
		t.Fatalf("expected one token session per principal, got %v", byPrincipal)
	}
	deniedListen := map[string]string{}
	deadline := time.Now().Add(3 * time.Second)
	for len(deniedListen) < 2 && time.Now().Before(deadline) {
		for _, r := range auditRows(t, st) {
			if r.Tool == "subscriptions/listen" && r.Result == "denied" && len(r.Paths) == 1 {
				deniedListen[r.PrincipalID] = r.Paths[0]
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if deniedListen[pa.ID] != "/gd/x.txt" || deniedListen[pb.ID] != "/work/a.txt" {
		t.Fatalf("denied listen rows by principal: %v", deniedListen)
	}
	for _, r := range auditRows(t, st) {
		if r.Tool == "subscriptions/listen" && r.Result == "ok" {
			t.Fatalf("a listen outside the scope was accepted: %+v", r)
		}
	}
}

func TestExpiredTokenInitializeIs401(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	tok, _ := mustCreateToken(t, st, agent.TokenSpec{Name: "short", TTL: time.Millisecond})
	time.Sleep(5 * time.Millisecond)
	ts := tokenServer(t, e, HTTPAuth{Verify: st.VerifyToken})
	req, err := http.NewRequest("POST", ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"0"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("WWW-Authenticate %q", resp.Header.Get("WWW-Authenticate"))
	}
	// So are an unknown token, a revoked token and no token at all.
	if code := rawInitialize(t, ts.URL, "cfs_not-a-token"); code != 401 {
		t.Fatalf("unknown token: %d", code)
	}
	live, p := mustCreateToken(t, st, agent.TokenSpec{Name: "live"})
	if code := rawInitialize(t, ts.URL, live); code != 200 {
		t.Fatalf("live token: %d", code)
	}
	if _, err := st.RevokeToken(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	if code := rawInitialize(t, ts.URL, live); code != 401 {
		t.Fatalf("revoked token: %d", code)
	}
	if code := rawInitialize(t, ts.URL, ""); code != 401 {
		t.Fatalf("no token: %d", code)
	}
}

func TestRevokeClosesLegacySessionWithin5s(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	tok, p := mustCreateToken(t, st, agent.TokenSpec{Name: "legacy"})
	h := NewHTTPHandler(e.server, HTTPAuth{Verify: st.VerifyToken})
	inject := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+tok)
		h.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(inject)
	defer ts.Close()
	resp, data := subscriptionRPC(t, ts.URL, "2025-06-18", "", "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "legacy", "version": "0"}})
	id := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || id == "" {
		t.Fatalf("no legacy session id: %d %s", resp.StatusCode, data)
	}
	if resp, data := subscriptionRPC(t, ts.URL, "2025-06-18", id, "tools/list", map[string]any{}); resp.StatusCode != 200 {
		t.Fatalf("tools/list: %d %s", resp.StatusCode, data)
	}
	alive := func() bool {
		for ss := range e.server.MCP().Sessions() {
			if ss.ID() == id {
				return true
			}
		}
		return false
	}
	if !alive() {
		t.Fatal("the legacy session is not listed before the revoke")
	}
	if _, err := st.RevokeToken(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !alive() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the revoked token's legacy session is still open after 5s")
}

func TestCloseSessionsOfOnlyTouchesThatPrincipal(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	ta, pa := mustCreateToken(t, st, agent.TokenSpec{Name: "a"})
	tb, _ := mustCreateToken(t, st, agent.TokenSpec{Name: "b"})
	ts := tokenServer(t, e, HTTPAuth{Verify: st.VerifyToken})
	open := func(token string) string {
		req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"0"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Mcp-Session-Id") == "" {
			t.Fatalf("initialize: %d", resp.StatusCode)
		}
		return resp.Header.Get("Mcp-Session-Id")
	}
	ida, idb := open(ta), open(tb)
	if n := e.server.CloseSessionsOf(pa.ID); n != 1 {
		t.Fatalf("closed %d sessions of a, want 1", n)
	}
	live := map[string]bool{}
	for ss := range e.server.MCP().Sessions() {
		live[ss.ID()] = true
	}
	if live[ida] || !live[idb] {
		t.Fatalf("after closing a: a alive=%v b alive=%v", live[ida], live[idb])
	}
	if n := e.server.CloseSessionsOf(pa.ID); n != 0 {
		t.Fatalf("a second close found %d sessions", n)
	}
}

func TestEnvTokenKeepsFullAccess(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	e.fake.Seed("gd/x.txt", []byte("x"))
	ts := tokenServer(t, e, HTTPAuth{Token: "env-token", Verify: st.VerifyToken})
	s, err := connectWith(t, ts.URL, "env-token")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "stat", Arguments: map[string]any{"path": "/gd/x.txt"}})
	if err != nil || res.IsError {
		t.Fatalf("the legacy env token lost access: %v %+v", err, res)
	}
	sessions, _, err := e.server.opt.Sessions.List(context.Background(), agent.ListQuery{State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	var envSessions int
	for _, sess := range sessions {
		p, err := e.server.opt.Sessions.Principal(context.Background(), sess.PrincipalID)
		if err != nil {
			t.Fatal(err)
		}
		if p.Kind == "env" {
			envSessions++
		}
	}
	if envSessions != 1 {
		t.Fatalf("expected one session under the env principal, got %d of %d", envSessions, len(sessions))
	}
	// The env token is not a stored token: the wrong secret is still refused.
	if code := rawInitialize(t, ts.URL, "env-token-but-wrong"); code != 401 {
		t.Fatalf("wrong env token: %d", code)
	}
}

func TestLoopbackStaysOpenUntilTheFirstToken(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	open := func(ctx context.Context) bool {
		live, _ := st.HasLiveTokens(ctx)
		return !live
	}
	ts := tokenServer(t, e, HTTPAuth{Verify: st.VerifyToken, Open: open})
	if code := rawInitialize(t, ts.URL, ""); code != 200 {
		t.Fatalf("before any token: %d", code)
	}
	mustCreateToken(t, st, agent.TokenSpec{Name: "first"})
	if code := rawInitialize(t, ts.URL, ""); code != 401 {
		t.Fatalf("after the first token: %d", code)
	}
}

func TestServeHTTPWithAuthRefusesAPublicListenerWithoutAnyAuth(t *testing.T) {
	e, st := newAgentEnv(t, Options{}, agent.Scope{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ServeHTTPWithAuth(ctx, e.server, "0.0.0.0:0", HTTPAuth{Open: func(context.Context) bool { return true }})
	if err == nil || !strings.Contains(err.Error(), "without a token") {
		t.Fatalf("public listener with only Open: %v", err)
	}
	// A verifier alone is enough authentication for a public address; the
	// listener then closes with the context.
	e2, _ := newAgentEnv(t, Options{}, agent.Scope{})
	if err := ServeHTTPWithAuth(ctx, e2.server, "127.0.0.1:0", HTTPAuth{Verify: st.VerifyToken}); err != nil {
		t.Fatalf("cancelled loopback listener: %v", err)
	}
}

func TestRequireAuthLeavesAnOpenHandlerAlone(t *testing.T) {
	e, _ := newAgentEnv(t, Options{}, agent.Scope{})
	if fmt.Sprintf("%T", NewHTTPHandler(e.server, HTTPAuth{})) != fmt.Sprintf("%T", newMCPHTTPHandler(e.server)) {
		t.Fatal("an HTTPAuth with nothing configured must not wrap the handler")
	}
}
