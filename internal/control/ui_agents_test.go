package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The agents screen is the console's view of the audit trail and the
// sessions behind it. These tests read the embedded sources: a screen the
// router does not know, a table that never asks the daemon, or a denied row
// that is only a colour would each ship as a blank or misleading page with
// nothing else to fail.
func TestAgentsScreenIsRoutedAndInTheNav(t *testing.T) {
	if _, err := webFS.ReadFile("web/screens/agents.js"); err != nil {
		t.Fatalf("there is no agents screen: %v", err)
	}
	router := webSource(t, "web/router.js")
	app := webSource(t, "web/app.js")
	for _, want := range []string{"'#/agents': 'agents-view'", "hash: '#/agents', icon: 'bot', key: 'nav.agents'"} {
		if !strings.Contains(router, want) {
			t.Errorf("router.js lacks %s", want)
		}
	}
	if !strings.Contains(app, "renderAgents") || !strings.Contains(app, "active_sessions") {
		t.Error("the shell never mounts the agents screen or never shows the active-session badge")
	}
	css := webSource(t, "web/app.css")
	if !strings.Contains(css, ".nav a .badge") {
		t.Error("app.css has no rule for the nav badge")
	}
}

// The tab container keeps its choice in the URL hash so a deep link and a
// reload land on the same tab.
func TestAgentsScreenKeepsItsTabInTheHash(t *testing.T) {
	agents := webSource(t, "web/screens/agents.js")
	for _, want := range []string{"'tab'", "'sessions'", "'audit'", "'tokens'", "'memory'", "renderSessionsTab", "renderAuditTab", "t('agents.tab.' + "} {
		if !strings.Contains(agents, want) {
			t.Errorf("agents.js lacks %s", want)
		}
	}
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, tab := range []string{"sessions", "audit", "tokens", "memory"} {
		if !zh["agents.tab."+tab] {
			t.Errorf("missing agents.tab.%s", tab)
		}
	}
}

func TestAgentsScreenReachesTheSessionAndAuditRoutes(t *testing.T) {
	sessions := webSource(t, "web/screens/agents_sessions.js")
	audit := webSource(t, "web/screens/agents_audit.js")
	panel := webSource(t, "web/session_panel.js")
	for _, want := range []string{"api.get('/sessions?", "next_cursor", "moreRow(", "scopeParts(", "openSessionPanel("} {
		if !strings.Contains(sessions, want) {
			t.Errorf("sessions tab lacks %s", want)
		}
	}
	for _, want := range []string{"api.get('/audit?", "next_cursor", "moreRow(", "'data-result'", "t('audit.result.' + "} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit tab lacks %s", want)
		}
	}
	for _, want := range []string{"api.get('/sessions/' + encodeURIComponent(", "api.post('/sessions/' + encodeURIComponent(", "+ '/finish'", "openPanel("} {
		if !strings.Contains(panel, want) {
			t.Errorf("session panel lacks %s", want)
		}
	}
	// Finishing a session is a state change: it must be a POST with the
	// control header, never a GET a link could trigger.
	if strings.Contains(panel, "api.get('/sessions/' + encodeURIComponent(id) + '/finish'") {
		t.Error("the session panel finishes a session with GET")
	}
}

// A denied row must say so in words; colour alone is not an accessible
// signal.
func TestDeniedAuditRowsCarryATextLabel(t *testing.T) {
	audit := webSource(t, "web/screens/agents_audit.js")
	if !strings.Contains(audit, "class: row.result === 'denied' ? 'denied' : ''") || !strings.Contains(audit, "t('audit.result.' + row.result)") {
		t.Error("denied rows must carry both the row class and the translated result label")
	}
	if !strings.Contains(webSource(t, "web/app.css"), "tr.denied td") {
		t.Error("app.css has no rule for a denied row")
	}
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, k := range []string{"audit.result.ok", "audit.result.denied", "audit.result.error"} {
		if !zh[k] {
			t.Errorf("missing %s", k)
		}
	}
}

// Audit arguments come from the agent, redacted or not. They are shown as
// text, never parsed as markup, and never kept in the shared store.
func TestAuditArgsRenderAsTextAndAreNotCached(t *testing.T) {
	audit := webSource(t, "web/screens/agents_audit.js")
	if strings.Contains(audit, "html:") {
		t.Error("audit tab uses the html attribute; args are untrusted")
	}
	if !strings.Contains(audit, "JSON.stringify(row.args, null, 2)") {
		t.Error("args are not shown as formatted text")
	}
	for _, name := range []string{"web/screens/agents_audit.js", "web/screens/agents_sessions.js", "web/session_panel.js"} {
		if strings.Contains(webSource(t, name), "store.js") {
			t.Errorf("%s reaches the shared store; audit and session rows are held by the screen", name)
		}
	}
}

// The events stream carries audit and session events; the shell fans them
// out to whichever tab is mounted.
func TestAgentEventsReachTheScreen(t *testing.T) {
	api := webSource(t, "web/api.js")
	for _, want := range []string{"onAudit", "onSession", "addEventListener('audit'", "addEventListener('session'"} {
		if !strings.Contains(api, want) {
			t.Errorf("api.js lacks %s", want)
		}
	}
	app := webSource(t, "web/app.js")
	if !strings.Contains(app, "export function onAgentEvent(") {
		t.Error("app.js does not export onAgentEvent")
	}
	for _, name := range []string{"web/screens/agents_sessions.js", "web/screens/agents_audit.js"} {
		if !strings.Contains(webSource(t, name), "onAgentEvent(") {
			t.Errorf("%s never subscribes to agent events", name)
		}
	}
}

func TestWebCatalogCoversSessionStates(t *testing.T) {
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, s := range []string{"active", "finished", "expired"} {
		if !zh["session.state."+s] {
			t.Errorf("missing session.state.%s", s)
		}
	}
	// scope_view.js hands back keys for t(); the screen cannot spell them as
	// literals, so they are checked here.
	for _, k := range []string{"scope.all", "scope.read", "scope.write", "scope.readonly", "scope.expires", "scope.sandbox"} {
		if !zh[k] {
			t.Errorf("missing %s", k)
		}
	}
}

// Every module the screen ships stays small enough to read in one sitting.
func TestAgentsModulesStayShort(t *testing.T) {
	for _, name := range []string{
		"web/screens/agents.js", "web/screens/agents_sessions.js", "web/screens/agents_audit.js",
		"web/session_panel.js", "web/scope_view.js", "web/app.js", "web/api.js", "web/router.js",
	} {
		if n := strings.Count(webSource(t, name), "\n"); n >= 800 {
			t.Errorf("%s is %d lines; split it", name, n)
		}
	}
}

// TestConnectPanelWarnsAboutStdioNonOwner: the banner that tells a person
// to switch to the HTTP transport is driven by /mcp/connect's
// stdio_non_owner alone and links to the diagnostics screen, where the
// doctor item explains it. The decision lives in connect_view.js, a module
// with no DOM, and the node suite exercises both answers; this checks the
// panel renders that decision and nothing else decides it.
func TestConnectPanelWarnsAboutStdioNonOwner(t *testing.T) {
	view := webSource(t, "web/connect_view.js")
	for _, want := range []string{"c.stdio_non_owner !== true) return null", "href: '#/diagnostics'", "key: 'connect.stdio.banner'", "linkKey: 'connect.stdio.link'"} {
		if !strings.Contains(view, want) {
			t.Errorf("connect_view.js lacks %s", want)
		}
	}
	panel := webSource(t, "web/connect_panel.js")
	for _, want := range []string{"import { stdioWarning, bridgeBanner } from '/ui/connect_view.js'", "const warning = stdioWarning(c)", "if (warning) {", "role: 'alert'", "'data-stdio-warning'", "el('a', { href: warning.href }, t(warning.linkKey))", "t(warning.key)"} {
		if !strings.Contains(panel, want) {
			t.Errorf("connect_panel.js lacks %s", want)
		}
	}
	if strings.Contains(panel, "c.stdio_non_owner") {
		t.Error("the panel decides the banner itself instead of through connect_view.js")
	}
	if _, err := os.Stat(filepath.Join("web", "_tests", "connect_view.test.mjs")); err != nil {
		t.Fatalf("the node suite for connect_view.js is missing: %v", err)
	}
	for _, lang := range []string{"zh", "en"} {
		keys := tableKeys(t, webI18nSource(t), lang)
		for _, k := range []string{"connect.stdio.banner", "connect.stdio.link"} {
			if !keys[k] {
				t.Errorf("%s lacks %s", lang, k)
			}
		}
	}
}

// TestConnectPanelShowsBridgeState: the line under the stdio warning that
// says whether that server's writes reach the owner is decided by
// connect_view.js from /mcp/connect's bridge field — connected green with
// the session count, disabled yellow with the reason, n/a nothing — and
// the panel renders that decision without reading the field itself.
func TestConnectPanelShowsBridgeState(t *testing.T) {
	view := webSource(t, "web/connect_view.js")
	for _, want := range []string{"export function bridgeBanner(c)", "b.state === 'connected'", "cls: 'ok'", "key: 'connect.bridge.connected'", "b.state === 'disabled'", "cls: 'warn'", "'connect.bridge.unknown'"} {
		if !strings.Contains(view, want) {
			t.Errorf("connect_view.js lacks %s", want)
		}
	}
	panel := webSource(t, "web/connect_panel.js")
	for _, want := range []string{"const bridge = bridgeBanner(c)", "if (bridge) {", "'data-bridge-banner': bridge.cls", "role: 'status'", "t(bridge.key, bridge.arg)", "t(bridge.reasonKey)"} {
		if !strings.Contains(panel, want) {
			t.Errorf("connect_panel.js lacks %s", want)
		}
	}
	if strings.Contains(panel, "c.bridge") {
		t.Error("the panel decides the bridge line itself instead of through connect_view.js")
	}
	if !strings.Contains(webSource(t, "web/app.css"), ".banner.ok {") {
		t.Error("app.css has no green banner")
	}
	for _, lang := range []string{"zh", "en"} {
		keys := tableKeys(t, webI18nSource(t), lang)
		for _, k := range []string{"connect.bridge.connected", "connect.bridge.disabled", "connect.bridge.http_off", "connect.bridge.not_loopback", "connect.bridge.no_token", "connect.bridge.unknown"} {
			if !keys[k] {
				t.Errorf("%s lacks %s", lang, k)
			}
		}
	}
}

// TestHooksCardIsReadOnly (ui-plan G7): the connect panel's Hooks card
// reads /agent/hooks and shows each client's registration, whether its
// shape is verified, the context mode and the shell lines to copy; it
// never posts to the hooks route or offers a button that edits a settings
// file. The row and command decisions live in hooks_view.js under node.
func TestHooksCardIsReadOnly(t *testing.T) {
	view := webSource(t, "web/hooks_view.js")
	if strings.Contains(view, "import ") {
		t.Error("hooks_view.js imports; it must run under node with no DOM")
	}
	for _, want := range []string{"export function clientRow(c)", "export function commands(r)", "'hooks.state.' + state", "'hooks.verified' : 'hooks.unverified'", "' --client ' + todo.join(',')"} {
		if !strings.Contains(view, want) {
			t.Errorf("hooks_view.js lacks %s", want)
		}
	}
	if _, err := os.Stat(filepath.Join("web", "_tests", "hooks_view.test.mjs")); err != nil {
		t.Fatalf("the node suite for hooks_view.js is missing: %v", err)
	}
	panel := webSource(t, "web/connect_panel.js")
	for _, want := range []string{"import { clientRow, commands } from '/ui/hooks_view.js'", "api.get('/agent/hooks')", "function hooksCard(h)", "'data-hooks'", "'data-hook-client': r.client", "'data-hook-state': r.state", "t(r.stateKey)", "t(r.verifiedKey)", "commands(h).map(", "copyBtn(c.text)", "t('hooks.note')"} {
		if !strings.Contains(panel, want) {
			t.Errorf("connect_panel.js lacks %s", want)
		}
	}
	if strings.Contains(panel, "api.post(") || strings.Contains(panel, "html:") {
		t.Error("the connect panel writes or uses innerHTML")
	}
	if n := strings.Count(panel, "\n"); n >= 800 {
		t.Errorf("connect_panel.js is %d lines; split it", n)
	}
	src := webI18nSource(t)
	for _, lang := range []string{"zh", "en"} {
		keys := tableKeys(t, src, lang)
		for _, k := range []string{"hooks.title", "hooks.context.off", "hooks.context.minimal", "hooks.context.full", "hooks.col.client", "hooks.col.state", "hooks.col.verified", "hooks.col.path", "hooks.state.installed", "hooks.state.absent", "hooks.state.missing", "hooks.verified", "hooks.unverified", "hooks.registry", "hooks.cmd.install", "hooks.cmd.uninstall", "hooks.note"} {
			if !keys[k] {
				t.Errorf("%s lacks %s", lang, k)
			}
		}
	}
}
