package control

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
)

// plainTokenShape is what a plain token looks like: the prefix and at
// least 20 URL-safe characters. Any body scanned with it must never match,
// however the token got there.
var plainTokenShape = regexp.MustCompile(`cfs_[A-Za-z0-9_-]{20,}`)

// testSnippets renders a placeholder snippet the way cmd/cloudfs does with
// mcpsrv, without importing the adapter into this package's tests.
func testSnippets(url string) (map[string]string, map[string]string) {
	token := TokenPlaceholder
	return map[string]string{
		"claude": `{"mcpServers":{"cloudfs":{"type":"http","url":"` + url + `","headers":{"Authorization":"Bearer ` + token + `"}}}}`,
		"codex":  `[mcp_servers.cloudfs]` + "\n" + `url = "` + url + `"` + "\n" + `http_headers = { "Authorization" = "Bearer ` + token + `" }` + "\n",
	}, map[string]string{
		"claude": `claude mcp add --transport http cloudfs ` + url + ` --header "Authorization: Bearer ` + token + `"`,
	}
}

// tokensFixture wires the MCP view over the agent store the way the daemon
// does, through NewMCPView, with the HTTP transport reported as listening.
func tokensFixture(t *testing.T) (*fixture, *agent.Store) {
	t.Helper()
	f, st, _ := agentFixture(t)
	state := func() MCPHTTPState { return MCPHTTPState{Addr: "127.0.0.1:8765", Owner: true} }
	f.coll.MCP = NewMCPView(st, state, testSnippets)
	return f, st
}

func TestMCPConnectCarriesNoToken(t *testing.T) {
	f, st := tokensFixture(t)
	plain, _, err := st.CreateToken(context.Background(), agent.TokenSpec{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/mcp/connect", "")
	body := w.Body.String()
	if w.Code != 200 || strings.Contains(body, plain) || plainTokenShape.MatchString(body) {
		t.Fatalf("%d %s", w.Code, body)
	}
	if !strings.Contains(body, TokenPlaceholder) {
		t.Fatal("snippets must carry the placeholder")
	}
	var c MCPConnect
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if !c.HTTPListening || c.HTTPAddr != "127.0.0.1:8765" || c.URL != "http://127.0.0.1:8765/" || !c.Owner || c.StdioNonOwner {
		t.Fatalf("%+v", c)
	}
	if c.InstallTransport != "http" {
		t.Fatalf("install_transport = %q with a listener up", c.InstallTransport)
	}
	if !c.AuthRequired {
		t.Fatal("a live token makes the listener demand a bearer")
	}
	if c.Snippets["claude"] == "" || c.Snippets["codex"] == "" || c.AddCommands["claude"] == "" {
		t.Fatalf("%+v", c)
	}
}

// TestMCPConnectReportsAStdioServerBesideTheOwner: the connect panel's
// banner is driven by the heartbeat a non-owner stdio server keeps under
// the agent directory, read on every request so the banner follows the
// process up and down.
func TestMCPConnectReportsAStdioServerBesideTheOwner(t *testing.T) {
	f, st := tokensFixture(t)
	h := NewServer(f.coll).Handler()
	connect := func() MCPConnect {
		t.Helper()
		w := uiCallControl(t, h, "GET", "/mcp/connect", "")
		var c MCPConnect
		if err := json.Unmarshal(w.Body.Bytes(), &c); w.Code != 200 || err != nil {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		return c
	}
	if connect().StdioNonOwner {
		t.Fatal("no heartbeat, yet a stdio server was reported")
	}
	if err := agent.WriteHeartbeat(st.Dir(), 777); err != nil {
		t.Fatal(err)
	}
	if !connect().StdioNonOwner {
		t.Fatal("a live heartbeat was not reported")
	}
	if err := agent.RemoveHeartbeat(st.Dir(), 777); err != nil {
		t.Fatal(err)
	}
	if connect().StdioNonOwner {
		t.Fatal("the stdio server's exit was not noticed")
	}
}

func TestMCPConnectWithoutAListener(t *testing.T) {
	f, st, _ := agentFixture(t)
	f.coll.MCP = NewMCPView(st, func() MCPHTTPState { return MCPHTTPState{} }, testSnippets)
	w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/mcp/connect", "")
	var c MCPConnect
	if err := json.Unmarshal(w.Body.Bytes(), &c); w.Code != 200 || err != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if c.HTTPListening || c.URL != "" || c.AuthRequired || len(c.Snippets) != 0 || len(c.AddCommands) != 0 || c.InstallTransport != "stdio" {
		t.Fatalf("%+v", c)
	}
	if !strings.Contains(w.Body.String(), `"snippets":{}`) {
		t.Fatalf("snippets must be an object, not null: %s", w.Body)
	}
}

func TestMCPConnectReportsTheEnvironmentToken(t *testing.T) {
	f, st, _ := agentFixture(t)
	f.coll.MCP = NewMCPView(st, func() MCPHTTPState { return MCPHTTPState{Addr: "127.0.0.1:8765", EnvToken: true} }, nil)
	w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/mcp/connect", "")
	var c MCPConnect
	if err := json.Unmarshal(w.Body.Bytes(), &c); w.Code != 200 || err != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if !c.AuthRequired || c.Owner || len(c.Snippets) != 0 {
		t.Fatalf("%+v", c)
	}
}

func TestCreateTokenResponseIsNoStore(t *testing.T) {
	f, st := tokensFixture(t)
	w := uiCallControl(t, NewServer(f.coll).Handler(), "POST", "/mcp/tokens", `{"name":"codex","read":["/work"],"write":["/work/.agent"],"ttl_seconds":86400}`)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %q %s", w.Code, w.Header().Get("Cache-Control"), w.Body)
	}
	var r TokenCreateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if !plainTokenShape.MatchString(r.Token) || !strings.Contains(r.Snippets["claude"], r.Token) || !strings.Contains(r.Snippets["codex"], r.Token) {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.AddCommands["claude"], r.Token) || strings.Contains(r.AddCommands["claude"], TokenPlaceholder) {
		t.Fatalf("%+v", r.AddCommands)
	}
	p := r.Principal
	if p.Name != "codex" || p.Fingerprint != r.Token[4:8] || p.State != "active" || p.ExpiresAt == nil {
		t.Fatalf("%+v", p)
	}
	if strings.Join(p.Read, ",") != "/work" || strings.Join(p.Write, ",") != "/work/.agent" || p.ReadOnly {
		t.Fatalf("%+v", p)
	}
	if got, err := st.VerifyToken(context.Background(), r.Token); err != nil || got.ID != p.ID {
		t.Fatalf("the token does not verify: %v %+v", err, got)
	}
}

func TestCreateTokenRefusesABadSpec(t *testing.T) {
	f, _ := tokensFixture(t)
	h := NewServer(f.coll).Handler()
	for _, body := range []string{
		`{"name":"Bad Name"}`,
		`{"name":"a","read":["/work"],"write":["/other"]}`,
		`{"name":"a","ttl_seconds":-1}`,
	} {
		w := uiCallControl(t, h, "POST", "/mcp/tokens", body)
		if w.Code != 400 || plainTokenShape.MatchString(w.Body.String()) {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	if w := uiCallControl(t, h, "POST", "/mcp/tokens", `{"name":"a"}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := uiCallControl(t, h, "POST", "/mcp/tokens", `{"name":"a"}`); w.Code != 400 {
		t.Fatalf("a duplicate live name: %d %s", w.Code, w.Body)
	}
}

func TestTokensListNeverCarriesAPlainToken(t *testing.T) {
	f, st := tokensFixture(t)
	plain, _, err := st.CreateToken(context.Background(), agent.TokenSpec{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/mcp/tokens", "")
	body := w.Body.String()
	if w.Code != 200 || plainTokenShape.MatchString(body) || strings.Contains(body, plain) {
		t.Fatalf("%d %s", w.Code, body)
	}
	if !strings.Contains(body, `"fingerprint":"`+plain[4:8]+`"`) {
		t.Fatalf("fingerprint missing: %s", body)
	}
	for _, forbidden := range []string{"token_hash", "hash"} {
		if strings.Contains(body, `"`+forbidden+`"`) {
			t.Fatalf("%s leaked: %s", forbidden, body)
		}
	}
	var r TokensResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || len(r.Tokens) != 1 {
		t.Fatalf("%v %s", err, body)
	}
	if tok := r.Tokens[0]; tok.State != "active" || tok.ExpiresAt != nil || tok.LastUsedAt != nil || len(tok.Read) != 1 || tok.Read[0] != "/" {
		t.Fatalf("%+v", tok)
	}
}

func TestTokensListShowsExpiredAndRevoked(t *testing.T) {
	f, st := tokensFixture(t)
	now := time.Now()
	f.coll.Now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, _, err := st.CreateToken(context.Background(), agent.TokenSpec{Name: "short", TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	_, p, err := st.CreateToken(context.Background(), agent.TokenSpec{Name: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevokeToken(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	w := uiCallControl(t, NewServer(f.coll).Handler(), "GET", "/mcp/tokens", "")
	var r TokensResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || w.Code != 200 {
		t.Fatalf("%d %v %s", w.Code, err, w.Body)
	}
	states := map[string]string{}
	for _, tok := range r.Tokens {
		states[tok.Name] = tok.State
	}
	if states["short"] != "expired" || states["gone"] != "revoked" {
		t.Fatalf("%v", states)
	}
}

func TestRevokeTokenNeedsConfirm(t *testing.T) {
	f, st := tokensFixture(t)
	_, p, err := st.CreateToken(context.Background(), agent.TokenSpec{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	h := NewServer(f.coll).Handler()
	if w := uiCallControl(t, h, "POST", "/mcp/tokens/"+p.ID+"/revoke", `{}`); w.Code != 400 || !strings.Contains(w.Body.String(), p.ID) {
		t.Fatalf("no confirm: %d %s", w.Code, w.Body)
	}
	if w := uiCallControl(t, h, "GET", "/mcp/tokens", ""); !strings.Contains(w.Body.String(), `"state":"active"`) {
		t.Fatalf("a refused revoke must not revoke: %s", w.Body)
	}
	if w := uiCallControl(t, h, "POST", "/mcp/tokens/"+p.ID+"/revoke", `{"confirm":true}`); w.Code != 200 {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	w := uiCallControl(t, h, "GET", "/mcp/tokens", "")
	if !strings.Contains(w.Body.String(), `"state":"revoked"`) {
		t.Fatalf("%s", w.Body)
	}
	if w := uiCallControl(t, h, "POST", "/mcp/tokens/nope/revoke", `{"confirm":true}`); w.Code != 404 {
		t.Fatalf("unknown id: %d %s", w.Code, w.Body)
	}
}

func TestTokenRoutesRefuseOtherMethodsAndShapes(t *testing.T) {
	f, st := tokensFixture(t)
	_, p, err := st.CreateToken(context.Background(), agent.TokenSpec{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	h := NewServer(f.coll).Handler()
	if w := uiCallControl(t, h, "POST", "/mcp/connect", `{}`); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /mcp/connect: %d", w.Code)
	}
	if w := uiCallControl(t, h, "DELETE", "/mcp/tokens", `{}`); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /mcp/tokens: %d", w.Code)
	}
	if w := uiCallControl(t, h, "GET", "/mcp/tokens/"+p.ID+"/revoke", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET revoke: %d", w.Code)
	}
	for _, target := range []string{"/mcp/tokens/" + p.ID, "/mcp/tokens/a/b/revoke"} {
		if w := uiCallControl(t, h, "POST", target, `{"confirm":true}`); w.Code != 404 {
			t.Fatalf("%s: %d", target, w.Code)
		}
	}
}

func TestTokenRoutesAnswer503WithoutMCP(t *testing.T) {
	f := newFixture(t)
	h := NewServer(f.coll).Handler()
	for _, target := range []string{"/mcp/connect", "/mcp/tokens"} {
		if w := uiCallControl(t, h, "GET", target, ""); w.Code != 503 {
			t.Fatalf("%s %d", target, w.Code)
		}
	}
	if w := uiCallControl(t, h, "POST", "/mcp/tokens", `{"name":"a"}`); w.Code != 503 {
		t.Fatalf("POST /mcp/tokens %d", w.Code)
	}
	if w := uiCallControl(t, h, "POST", "/mcp/tokens/x/revoke", `{"confirm":true}`); w.Code != 503 {
		t.Fatalf("revoke %d", w.Code)
	}
}
