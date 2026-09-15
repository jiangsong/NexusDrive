package control

import (
	"strings"
	"testing"
)

// The access-token screens are the only place a plain token is ever shown,
// and it is shown once. These tests read the embedded sources so that a
// module which parked the token somewhere durable, a revoke that skipped the
// typed confirmation, or a list that printed more than a fingerprint would
// each fail here rather than ship.
func TestTokenRevealKeepsTheTokenOutOfStorage(t *testing.T) {
	src := webSource(t, "web/token_reveal.js")
	for _, banned := range []string{"store.js", "localStorage", "sessionStorage", "indexedDB", "html:"} {
		if strings.Contains(src, banned) {
			t.Errorf("token_reveal.js mentions %s", banned)
		}
	}
	for _, want := range []string{"openPanel(", "copyBtn(token)", "t('tokens.reveal.once')", "snippets.claude", "snippets.codex"} {
		if !strings.Contains(src, want) {
			t.Errorf("token_reveal.js lacks %s", want)
		}
	}
	if strings.Contains(src, "let token") || strings.Contains(src, "var token") {
		t.Error("the token must not be held in a module-level variable")
	}
	// The panel only shows what it is handed: it never fetches, so the plain
	// token reaches it from exactly one response and nowhere else.
	if strings.Contains(src, "api.js") || strings.Contains(src, "fetch(") {
		t.Error("token_reveal.js reaches the network; the token must come in as a parameter")
	}
}

func TestTokensTabReachesTheTokenRoutes(t *testing.T) {
	src := webSource(t, "web/screens/agents_tokens.js")
	for _, want := range []string{
		"api.get('/mcp/tokens'", "api.post('/mcp/tokens'",
		"+ '/revoke'", "confirm: true", "confirmToken: tok.name", "validateTokenScope(", "openTokenReveal(",
		"t('tokens.state.' + tok.state)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("tokens tab lacks %s", want)
		}
	}
	if strings.Contains(src, "store.js") {
		t.Error("the tokens tab must not route a created token through the shared store")
	}
	// The plain token is read from the create response and handed straight
	// to the reveal panel; the list only ever renders the fingerprint.
	if !strings.Contains(src, "openTokenReveal({ token: r.token") {
		t.Error("the created token must go from the response straight into the reveal panel")
	}
	if !strings.Contains(src, "tok.fingerprint") || strings.Contains(src, "tok.token") {
		t.Error("the token list must render the fingerprint and nothing longer")
	}
	// Revoking is destructive: it goes through the typed confirmation, and
	// the button only exists on a token that is still live.
	if !strings.Contains(src, "confirmDelete({") || !strings.Contains(src, "tok.state === 'active'") {
		t.Error("revoke must use the typed confirmation and only be offered on active tokens")
	}
}

func TestConnectPanelCallsConnectAndLinksDiagnostics(t *testing.T) {
	src := webSource(t, "web/connect_panel.js")
	for _, want := range []string{"api.get('/mcp/connect'", "stdio_non_owner", "href: '#/diagnostics'", "http_listening", "'details'"} {
		if !strings.Contains(src, want) {
			t.Errorf("connect panel lacks %s", want)
		}
	}
	agents := webSource(t, "web/screens/agents.js")
	for _, want := range []string{"renderConnectPanel(", "params.get('connect') === '1'", "renderTokensTab"} {
		if !strings.Contains(agents, want) {
			t.Errorf("agents.js lacks %s", want)
		}
	}
}

// The finish page has two shapes — the daemon already serving the pool, and
// the restart still pending — and both end with the card that opens the
// agents screen with its connect panel unfolded.
func TestSetupFinishLinksToAgentConnect(t *testing.T) {
	src := webSource(t, "web/screens/setup.js")
	if !strings.Contains(src, "href: '#/agents?connect=1'") {
		t.Fatal("the finish page never links to the agents screen with the connect panel open")
	}
	if n := strings.Count(src, "agentCard(),"); n < 2 {
		t.Fatalf("both finish branches must offer the agent card, found %d", n)
	}
}

func TestTokenScreensHaveNoPasswordInput(t *testing.T) {
	for _, f := range []string{"web/token_reveal.js", "web/screens/agents_tokens.js", "web/connect_panel.js"} {
		src := webSource(t, f)
		if strings.Contains(src, "type: 'password'") || strings.Contains(src, `type="password"`) {
			t.Errorf("%s renders a password input", f)
		}
	}
}

func TestWebCatalogCoversTokenStates(t *testing.T) {
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, s := range []string{"active", "expired", "revoked"} {
		if !zh["tokens.state."+s] {
			t.Errorf("missing tokens.state.%s", s)
		}
	}
	for _, k := range []string{"agents.tab.tokens", "tokens.err.write_outside_read", "tokens.err.relative"} {
		if !zh[k] {
			t.Errorf("missing %s", k)
		}
	}
}

func TestTokenModulesStayShort(t *testing.T) {
	for _, name := range []string{"web/token_reveal.js", "web/screens/agents_tokens.js", "web/connect_panel.js", "web/screens/setup.js", "web/screens/agents.js"} {
		if n := strings.Count(webSource(t, name), "\n"); n >= 800 {
			t.Errorf("%s is %d lines; split it", name, n)
		}
	}
}
