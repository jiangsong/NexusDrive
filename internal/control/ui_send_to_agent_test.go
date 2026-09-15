package control

import (
	"strings"
	"testing"
)

// "Send to agent" is a prompt the person copies into their own MCP client
// and, when the daemon has agents[] configured, hands to one of them. The
// panel fetches the prompt and the agent names, lets the person edit the
// prompt, copies it, or runs it after a typed confirmation. These tests
// read the shipped sources so a panel that wrote something on the copy
// path, ran without confirming, painted the prompt through innerHTML, or
// lost one of its two entrances would fail here rather than in a browser.

// TestSendToAgentOnlyReadsAndCopies: opening the panel reads the prompt
// and the endpoints and nothing else; the prompt lands in a textarea's
// value and the copy takes whatever the person left in it. The one write
// the file makes is the confirmed run.
func TestSendToAgentOnlyReadsAndCopies(t *testing.T) {
	src := webSource(t, "web/send_to_agent.js")
	if !strings.Contains(src, "api.get('/agent/prompt?path=' + encodeURIComponent(path)") {
		t.Error("the send panel must fetch the prompt")
	}
	for _, m := range []string{"api.get(", "api.post(", "api.put(", "api.patch(", "api.del("} {
		for i := strings.Index(src, m); i >= 0; i = strings.Index(src, m) {
			rest := src[i+len(m):]
			src = rest
			switch {
			case m == "api.get(" && strings.HasPrefix(rest, "'/agent/prompt?path="):
			case m == "api.get(" && strings.HasPrefix(rest, "'/agent/endpoints'"):
			case m == "api.post(" && strings.HasPrefix(rest, "'/agent/invoke'"):
			default:
				t.Errorf("unexpected request %s%s", m, rest[:min(40, len(rest))])
			}
		}
	}
	src = webSource(t, "web/send_to_agent.js")
	if !strings.Contains(src, "encodeURIComponent(heading)") {
		t.Error("the heading from a search hit does not reach the prompt route")
	}
	if !strings.Contains(src, "textarea.value = ") || strings.Contains(src, "html:") {
		t.Error("the prompt must be set as a value, not as markup")
	}
	if !strings.Contains(src, "clipboard.writeText(textarea.value)") {
		t.Error("copy must take the edited text")
	}
	if !strings.Contains(src, "openPanel({") {
		t.Error("the panel does not go through the shared openPanel shell")
	}
}

// copyHandler is the source of the copy button's click handler.
func copyHandler(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "const copy = el('button'")
	end := strings.Index(src, "t('agent.prompt.copy'))")
	if start < 0 || end < start {
		t.Fatal("copy button not found")
	}
	return src[start:end]
}

// TestCopyStillMakesNoWrite: the copy button writes the clipboard and
// nothing else — no request of any kind, whatever else the panel offers.
func TestCopyStillMakesNoWrite(t *testing.T) {
	src := webSource(t, "web/send_to_agent.js")
	h := copyHandler(t, src)
	if !strings.Contains(h, "clipboard.writeText(textarea.value)") {
		t.Error("copy must take the edited text")
	}
	if strings.Contains(h, "api.") || strings.Contains(h, "fetch(") || strings.Contains(h, "confirmDelete") {
		t.Errorf("the copy handler does more than copy:\n%s", h)
	}
}

// TestRunButtonOnlyWithEndpoints: the agent select and the run button
// exist only when /agent/endpoints returned at least one name; a daemon
// without agents[] shows the copy-only panel of phase one.
func TestRunButtonOnlyWithEndpoints(t *testing.T) {
	src := webSource(t, "web/send_to_agent.js")
	if !strings.Contains(src, "api.get('/agent/endpoints')") {
		t.Fatal("the panel does not ask for the agent names")
	}
	i := strings.Index(src, "agents.length")
	if i < 0 {
		t.Fatal("nothing is conditional on the agents list")
	}
	run := strings.Index(src, "t('agent.run.button')")
	sel := strings.Index(src, "el('select'")
	if run < 0 || sel < 0 {
		t.Fatal("run button or agent select missing")
	}
	if run < i || sel < i {
		t.Error("the run button and the select are built before the agents list is checked")
	}
	if !strings.Contains(src, "el('option', { value: a.name }, a.name)") {
		t.Error("agent names must be option text, not markup")
	}
	if strings.Contains(src, "html:") {
		t.Error("nothing in this panel may be set as markup")
	}
	// An endpoints failure degrades to the copy-only panel; the prompt is
	// still shown.
	if !strings.Contains(src, "catch (_) { agents = []; }") {
		t.Error("an endpoints error must leave the copy panel usable")
	}
}

// TestRunConfirmsWithConfirmTrue: running goes through the typed
// confirmation with the agent name as the token, then posts confirm: true
// with the edited prompt and the one path; an already-queued answer (409)
// is named, not shown as a raw error.
func TestRunConfirmsWithConfirmTrue(t *testing.T) {
	src := webSource(t, "web/send_to_agent.js")
	c := strings.Index(src, "confirmDelete({")
	p := strings.Index(src, "api.post('/agent/invoke'")
	if c < 0 || p < 0 {
		t.Fatal("confirmation or invoke request missing")
	}
	if c > p {
		t.Error("the request is sent before the confirmation")
	}
	conf := src[c:p]
	if !strings.Contains(conf, "confirmToken: agent") || !strings.Contains(conf, "if (!sure) return;") {
		t.Errorf("the confirmation does not gate on the typed agent name:\n%s", conf)
	}
	if !strings.Contains(src, "api.post('/agent/invoke', { agent, paths: [path], prompt: textarea.value, confirm: true })") {
		t.Error("the invoke request must carry the agent, the one path, the edited prompt and confirm: true")
	}
	if !strings.Contains(src, "'#/triggers?delivery=' + r.id") {
		t.Error("the submitted toast does not link to the delivery")
	}
	if !strings.Contains(src, "t('agent.run.submitted')") || !strings.Contains(src, "t('agent.run.view')") {
		t.Error("the submitted toast copy is not from the catalog")
	}
	if !strings.Contains(src, "e.status === 409") || !strings.Contains(src, "t('agent.run.queued')") {
		t.Error("an already-queued run is not named")
	}
}

// TestSendToAgentHasBothEntrances: the inspector offers it for files and
// directories alike, and a content-search row offers it with the hit's
// heading so the prompt points at the passage.
func TestSendToAgentHasBothEntrances(t *testing.T) {
	main := webSource(t, "web/screens/main.js")
	if !strings.Contains(main, "openSendToAgent({ path: e.path })") {
		t.Error("inspector entrance missing")
	}
	i := strings.Index(main, "openSendToAgent({ path: e.path })")
	if i >= 0 {
		line := main[strings.LastIndex(main[:i], "\n")+1 : i]
		if strings.Contains(line, "e.is_dir ?") {
			t.Error("the inspector button is hidden for one kind of entry; files and directories both get it")
		}
		if !strings.Contains(main[i:i+120], "iconEl('bot')") {
			t.Error("the inspector button has no bot icon")
		}
	}
	search := webSource(t, "web/content_search.js")
	if !strings.Contains(search, "openSendToAgent({ path: hit.path, heading: hit.heading })") {
		t.Error("search result entrance missing")
	}
	if !strings.Contains(search, "ev.stopPropagation()") {
		t.Error("the row button also opens the extracted text; the click must stop at the button")
	}
	if !strings.Contains(search, "'aria-label': t('action.sendtoagent')") {
		t.Error("the icon-only row button has no accessible name")
	}
}
