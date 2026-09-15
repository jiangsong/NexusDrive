package control

import (
	"strings"
	"testing"
)

// "Send to agent" is a prompt the person copies into their own MCP client.
// Phase one ships only that: the panel fetches the prompt, lets the person
// edit it, and copies it. There is no run button and no /agent/invoke yet
// (T-41/T-42 phase two). These tests read the shipped sources so a panel
// that wrote something, painted the prompt through innerHTML, or lost one
// of its two entrances would fail here rather than in a browser.

// TestSendToAgentOnlyReadsAndCopies: the panel's only request is the GET
// for the prompt; the prompt lands in a textarea's value and the copy takes
// whatever the person left in it.
func TestSendToAgentOnlyReadsAndCopies(t *testing.T) {
	src := webSource(t, "web/send_to_agent.js")
	if !strings.Contains(src, "api.get('/agent/prompt?path=' + encodeURIComponent(path)") || strings.Contains(src, "api.post(") {
		t.Error("the send panel must fetch the prompt and never write")
	}
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
	// Nothing about running an agent ships in phase one.
	for _, no := range []string{"/agent/invoke", "/agent/endpoints", "confirmDelete", "confirm: true"} {
		if strings.Contains(src, no) {
			t.Errorf("send_to_agent.js carries %q, which is phase two", no)
		}
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
