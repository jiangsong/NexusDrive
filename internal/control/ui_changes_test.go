package control

import (
	"strings"
	"testing"
)

// The provenance rows of the inspector (ui-plan G5-1, G6-4): who last
// changed the file — origin as an icon with a word, the actor, the time —
// and how often it was read in thirty days. provenance.js asks the change
// record and the heat table; main.js only mounts it. Origins are drawn
// through originCell, so every one the daemon can send has an icon and a
// word, and an unknown one is shown as text beside the generic icon.
func TestInspectorShowsProvenance(t *testing.T) {
	prov := webSource(t, "web/provenance.js")
	for _, want := range []string{
		"export async function mountProvenance(", "'/changes?path=' + encodeURIComponent(path) + '&limit=1'",
		"'/agent/heat?path=' + encodeURIComponent(path) + '&days=' + HEAT_DAYS + '&limit=1'", "const HEAT_DAYS = 30",
		"export function originCell(origin)", "const ORIGINS = { kernel: 'folder', mcp: 'bot', control: 'globe', webdav: 'cloud', remote: 'cloud' }",
		"t('origin.' + origin)", "t('inspector.lastwriter')", "t('inspector.history')", "t('inspector.reads', HEAT_DAYS)",
		"t('changes.unreliable')", "'data-last-writer'", "'data-history': path", "'data-reads'", "openSession(c.session_id)", "openHistory(path)",
	} {
		if !strings.Contains(prov, want) {
			t.Errorf("provenance.js lacks %s", want)
		}
	}
	if strings.Contains(prov, "html:") || strings.Contains(prov, "api.post(") {
		t.Error("provenance.js uses innerHTML or writes; it only reads and inserts text")
	}
	main := webSource(t, "web/screens/main.js")
	for _, want := range []string{"import { mountProvenance } from '/ui/provenance.js'", "import { openHistoryPanel } from '/ui/history_panel.js'", "mountProvenance(provHost, e.path, { api, openSession: openSessionPanel, openHistory:", "provHost,"} {
		if !strings.Contains(main, want) {
			t.Errorf("main.js lacks %s", want)
		}
	}
	src := webI18nSource(t)
	for _, lang := range []string{"zh", "en"} {
		keys := tableKeys(t, src, lang)
		for _, k := range []string{"inspector.lastwriter", "inspector.history", "inspector.reads", "origin.kernel", "origin.mcp", "origin.control", "origin.webdav", "origin.remote", "changes.unreliable"} {
			if !keys[k] {
				t.Errorf("%s lacks %s", lang, k)
			}
		}
	}
}

// The history overlay (ui-plan G5-2): the inspector has no tab bar, so
// history opens as a panel that pages GET /changes?path=&cursor= newest
// first, names a session actor as a button into the session detail, and
// tags a row after a feed overflow.
func TestHistoryPanelPagesTheChangeRecord(t *testing.T) {
	panel := webSource(t, "web/history_panel.js")
	for _, want := range []string{
		"export function openHistoryPanel(path, { openSession })", "openPanel({ title: t('history.title', path)",
		"'/changes?path=' + encodeURIComponent(path) + '&limit=50' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : '')",
		"moreRow(COLUMNS, () => load(r.next_cursor))", "pageFailureMode(cursor) === 'append'",
		"t('changes.kind.' + c.kind)", "originCell(c.origin)", "'data-reliable': c.reliable === false ? '0' : '1'", "t('changes.unreliable')",
		"t('changes.via_session')", "t('changes.disabled')", "t('history.empty')",
		"export const KINDS = ['write', 'create', 'mkdir', 'remove', 'rename', 'remote', 'rescan']",
	} {
		if !strings.Contains(panel, want) {
			t.Errorf("history_panel.js lacks %s", want)
		}
	}
	if strings.Contains(panel, "html:") || strings.Contains(panel, "api.post(") {
		t.Error("history_panel.js uses innerHTML or writes")
	}
	src := webI18nSource(t)
	for _, lang := range []string{"zh", "en"} {
		keys := tableKeys(t, src, lang)
		for _, k := range []string{"history.title", "history.note", "history.loading", "history.empty", "changes.col.kind", "changes.col.origin", "changes.col.actor", "changes.col.reversible", "changes.via_session", "changes.disabled"} {
			if !keys[k] {
				t.Errorf("%s lacks %s", lang, k)
			}
		}
		for _, kind := range []string{"write", "create", "mkdir", "remove", "rename", "remote", "rescan"} {
			if !keys["changes.kind."+kind] {
				t.Errorf("%s lacks changes.kind.%s", lang, kind)
			}
		}
	}
}

// The changes tab (ui-plan G5-3): the change record of the whole mount,
// paged by cursor with a prefix filter, refreshed from the SSE change
// event only while it shows the unfiltered first page; the audit tab
// shows each call's transport under the client, and the session detail
// the principal and the changes cursor the session pulled up to.
func TestChangesTabIsRoutedAndPaged(t *testing.T) {
	agents := webSource(t, "web/screens/agents.js")
	for _, want := range []string{"'changes'", "renderChangesTab", "agents_changes.js"} {
		if !strings.Contains(agents, want) {
			t.Errorf("agents.js lacks %s", want)
		}
	}
	tab := webSource(t, "web/screens/agents_changes.js")
	for _, want := range []string{
		"export function renderChangesTab(host)", "api.get('/changes?' + q.toString())", "q.set('path', prefix.value.trim())",
		"moreRow(COLUMNS, () => load(r.next_cursor))", "pageFailureMode(cursor) === 'append'",
		"onFsChange(", "if (paged || filtered() || document.hidden) return", "clearTimeout(timer)",
		"originCell(c.origin)", "t('changes.kind.' + c.kind)", "t('changes.unreliable')", "openSessionPanel(c.session_id)", "t('changes.note')",
	} {
		if !strings.Contains(tab, want) {
			t.Errorf("agents_changes.js lacks %s", want)
		}
	}
	if strings.Contains(tab, "api.post(") || strings.Contains(tab, "html:") {
		t.Error("the changes tab writes or uses innerHTML")
	}
	if n := strings.Count(tab, "\n"); n >= 800 {
		t.Errorf("agents_changes.js is %d lines; split it", n)
	}
	audit := webSource(t, "web/screens/agents_audit.js")
	if !strings.Contains(audit, "row.transport ? el('div', { class: 'dim', style: 'font-size:11px' }, row.transport) : null") {
		t.Error("the audit tab does not show the call's transport")
	}
	panel := webSource(t, "web/session_panel.js")
	for _, want := range []string{"export function isHookPrincipal(id)", "id.startsWith('hook:')", "t('session.principal')", "'data-principal': s.principal", "t('session.last_change_seen')", "String(s.last_change_seen)"} {
		if !strings.Contains(panel, want) {
			t.Errorf("session_panel.js lacks %s", want)
		}
	}
	sessions := webSource(t, "web/screens/agents_sessions.js")
	for _, want := range []string{"isHookPrincipal(s.principal)", "t('session.hook')", "iconEl('bolt')"} {
		if !strings.Contains(sessions, want) {
			t.Errorf("agents_sessions.js lacks %s", want)
		}
	}
	src := webI18nSource(t)
	for _, lang := range []string{"zh", "en"} {
		keys := tableKeys(t, src, lang)
		for _, k := range []string{"agents.tab.changes", "changes.filter.prefix", "changes.empty", "changes.note", "session.principal", "session.last_change_seen", "session.hook"} {
			if !keys[k] {
				t.Errorf("%s lacks %s", lang, k)
			}
		}
	}
}
