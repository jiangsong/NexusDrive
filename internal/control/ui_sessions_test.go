package control

import (
	"strings"
	"testing"
)

// The session panel lists what an agent delivered, and one of its actions
// hands out a signed download link. These tests read the embedded sources
// so that a table which printed the link, a marker without an accessible
// name, or an inspector that guessed the session instead of asking the
// daemon would each fail here rather than ship.

// A signed link is fetched when someone asks for it and goes to the
// clipboard; it never becomes part of a table that stays on screen.
func TestArtifactLinkIsFetchedOnlyOnClickAndNeverRendered(t *testing.T) {
	src := webSource(t, "web/session_panel.js")
	i := strings.Index(src, "api.get('/fs/download-url?path=")
	if i < 0 {
		t.Fatal("the artifact table cannot copy a link")
	}
	if !strings.Contains(src[max(0, i-200):i], "onclick") {
		t.Error("the link is requested outside a click handler")
	}
	if strings.Count(src, "r.url") != 1 || !strings.Contains(src, "clipboard.writeText(r.url)") {
		t.Error("the signed URL must appear only in the clipboard call")
	}
	if strings.Contains(src, "copyBtn(r.url") {
		t.Error("copyBtn falls back to a toast that would print the URL")
	}
	// The manifest may carry a link the agent asked for with share=true.
	// The panel never reads it: a link in the manifest is for the agent's
	// caller, and one on screen would outlive its expiry.
	if strings.Contains(src, "download_url") {
		t.Error("the panel reads the manifest's download_url")
	}
	if strings.Contains(src, "html:") {
		t.Error("the panel uses the html attribute; artifact paths and summaries are agent text")
	}
}

func TestArtifactTableShowsLiveStateAndActions(t *testing.T) {
	src := webSource(t, "web/session_panel.js")
	for _, want := range []string{
		"api.get('/fs/stat?path=", "'data-artifact'", "t('artifact.state.' + ", "onFsChange(",
		"'#/connections?path='", "t('session.sandbox')", "t('session.openworkspace')",
		"t('artifact.open')", "t('artifact.copylink')", "t('artifact.empty')",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("session panel lacks %s", want)
		}
	}
	// The change subscription is released when the panel closes; a panel
	// that kept it would re-stat artifacts of a session nobody is looking at.
	if !strings.Contains(src, "stopFs()") {
		t.Error("the panel never releases its change subscription")
	}
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, k := range []string{"artifact.state.synced", "artifact.state.local", "artifact.state.missing", "artifact.copied", "artifact.copyfailed", "session.artifacts"} {
		if !zh[k] {
			t.Errorf("missing %s", k)
		}
	}
}

func TestInspectorLinksBackToTheSession(t *testing.T) {
	src := webSource(t, "web/screens/main.js")
	for _, want := range []string{
		"api.get('/sessions?path=' + encodeURIComponent(", "openSessionPanel(", "sessionDirOf(",
		"t('inspector.fromsession')", "t('inspector.fromsession.none')",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("main.js lacks %s", want)
		}
	}
}

func TestWorkspaceMarkUsesBotIconWithALabel(t *testing.T) {
	src := webSource(t, "web/screens/main.js")
	if !strings.Contains(src, "iconEl('bot')") || !strings.Contains(src, "'aria-label': t('workspace.mark.' + mark)") {
		t.Error("the workspace mark needs the bot icon and a translated aria-label")
	}
	if !strings.Contains(src, "workspaceMark(") || !strings.Contains(src, "agent.workspace") {
		t.Error("the mark must come from workspace_view.js and the workspace from /status")
	}
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, k := range []string{"workspace.mark.root", "workspace.mark.session"} {
		if !zh[k] {
			t.Errorf("missing %s", k)
		}
	}
}

func TestSessionsTableHasArtifactsColumnAndSandboxFilter(t *testing.T) {
	src := webSource(t, "web/screens/agents_sessions.js")
	for _, want := range []string{"t('session.col.artifacts')", "'&sandbox=1'", "t('session.filter.sandbox')", "s.artifacts"} {
		if !strings.Contains(src, want) {
			t.Errorf("sessions tab lacks %s", want)
		}
	}
}

// workspace_view.js decides which rows get a mark and which files belong to
// a session. It runs under node in TestBrowserModuleBehaviour, which needs
// it to import nothing.
func TestWorkspaceViewImportsNothing(t *testing.T) {
	src := webSource(t, "web/workspace_view.js")
	if strings.Contains(src, "import ") {
		t.Error("workspace_view.js must stay a pure module")
	}
	for _, want := range []string{"export function workspaceMark(", "export function sessionDirOf("} {
		if !strings.Contains(src, want) {
			t.Errorf("workspace_view.js lacks %s", want)
		}
	}
}
