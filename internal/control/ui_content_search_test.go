package control

import (
	"strings"
	"testing"
)

// The content search is the main window's search box answered by the
// content index, and the inspector's index line is the same index seen
// from one entry. These tests read the shipped assets: a segment offered
// while the daemon has no index, a result set whose degraded or truncated
// notes are dropped, a snippet from a file's content painted through
// innerHTML, or an inspector action wired to the wrong route would each
// ship as a page that looks right and is not.

// TestContentSearchToggleOnlyWhenIndexEnabled: the "content" segment is an
// extra mode of the name-search control, gated on status.index.enabled,
// and the choice is remembered through content_search.js.
func TestContentSearchToggleOnlyWhenIndexEnabled(t *testing.T) {
	src := webSource(t, "web/screens/main.js")
	for _, want := range []string{"function indexEnabled()", "status.index && status.index.enabled", "readSearchMode()", "writeSearchMode(", "extraModes: ["} {
		if !strings.Contains(src, want) {
			t.Errorf("main.js lacks %s", want)
		}
	}
	i := strings.Index(src, "t('search.mode.content')")
	if i < 0 || !strings.Contains(src[max(0, i-400):i], "indexEnabled()") {
		t.Error("the content toggle is rendered without checking index.enabled")
	}
	// The segment must follow the index as it comes and goes between ticks.
	if !strings.Contains(src, "search.refresh()") {
		t.Error("main.js never redraws the search segments on a status tick")
	}
	// A deep link can ask for the content mode.
	if !strings.Contains(src, "link.get('mode') === 'content'") {
		t.Error("main.js does not honour ?mode=content")
	}
}

// TestContentSearchCallsIndexSearchAndKeepsItsNotes: the content mode asks
// /index/search and keeps degraded and truncated with the rows, marks a
// stale hit, and shows the heading path.
func TestContentSearchCallsIndexSearchAndKeepsItsNotes(t *testing.T) {
	src := webSource(t, "web/content_search.js")
	for _, want := range []string{
		"api.get('/index/search?q=' + encodeURIComponent(query)",
		"r.degraded", "r.truncated",
		"t('search.content.degraded')", "t('search.content.truncated')",
		"hit.stale", "t('search.content.stale')", "hit.heading",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("content_search.js lacks %s", want)
		}
	}
	// The notes are rows of the result table, not toasts.
	if strings.Contains(src, "toast(t('search.content.degraded')") || strings.Contains(src, "toast(t('search.content.truncated')") {
		t.Error("degraded/truncated must stay with the rows, not fade out of a toast")
	}
	// A row opens the extracted text at the hit.
	main := webSource(t, "web/screens/main.js")
	if !strings.Contains(main, "openExtractedText(hit.path, hit.start_off") {
		t.Error("a content hit does not open the extracted text at its offset")
	}
	if strings.Contains(main, "api.get('/index/search") {
		t.Error("main.js must wire content_search.js instead of calling /index/search itself")
	}
}

// TestSnippetIsInsertedAsText: a snippet is file content. It goes through
// snippetParts into text nodes and <mark> elements, never innerHTML.
func TestSnippetIsInsertedAsText(t *testing.T) {
	for _, f := range []string{"web/content_search.js", "web/extracted_text.js", "web/snippet.js", "web/index_inspector.js"} {
		if strings.Contains(webSource(t, f), "html:") {
			t.Errorf("%s uses the html attribute on file content", f)
		}
	}
	if !strings.Contains(webSource(t, "web/content_search.js"), "snippetParts(") {
		t.Error("results do not go through snippetParts")
	}
	if !strings.Contains(webSource(t, "web/extracted_text.js"), "document.createTextNode(") {
		t.Error("the extracted text is not appended as text nodes")
	}
	// snippet.js is a pure module: no imports, no DOM, so node can run it.
	if src := webSource(t, "web/snippet.js"); strings.Contains(src, "import ") || strings.Contains(src, "document.") {
		t.Error("snippet.js must have no imports and touch no DOM")
	}
}

// TestSearchModeSurvivesUnavailableStorage: localStorage throws in a
// private window and in some embedded views; a search box that cannot
// remember the mode must still search by name.
func TestSearchModeSurvivesUnavailableStorage(t *testing.T) {
	src := webSource(t, "web/content_search.js")
	if strings.Count(src, "try {") < 2 || !strings.Contains(src, "localStorage.getItem(SEARCH_MODE_KEY)") {
		t.Error("search mode storage is not guarded")
	}
	if !strings.Contains(src, "=== 'content' ? 'content' : 'name'") {
		t.Error("an unknown stored mode must fall back to the name search")
	}
}

// TestInspectorIndexActionsReachTheirRoutes: the inspector's three index
// actions call their routes, removal is offered only for a console rule
// on its own path and carries confirm: true, and the text panel pages
// /index/text with "load more".
func TestInspectorIndexActionsReachTheirRoutes(t *testing.T) {
	ins := webSource(t, "web/index_inspector.js")
	for _, want := range []string{
		"api.get('/index/status?path=' + encodeURIComponent(entry.path)",
		"api.post('/index/add', { path: entry.path })",
		"api.post('/index/remove', { path: entry.path, confirm: true })",
		"st.rule_source === 'ui'", "st.covered === entry.path",
		"confirmDelete({",
		"openExtractedText(entry.path, 0)",
		"t('inspector.index.ok'", "t('inspector.index.pending')", "t('inspector.index.failed'", "t('inspector.index.uncovered')",
	} {
		if !strings.Contains(ins, want) {
			t.Errorf("index_inspector.js lacks %s", want)
		}
	}
	txt := webSource(t, "web/extracted_text.js")
	for _, want := range []string{
		"api.get('/index/text?path=' + encodeURIComponent(path)",
		"next_offset", "r.eof", "t('index.text.more')", "showPanel(", "scrollIntoView(",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("extracted_text.js lacks %s", want)
		}
	}
	main := webSource(t, "web/screens/main.js")
	if !strings.Contains(main, "renderIndexInfo(") {
		t.Error("the inspector never shows index state")
	}
	i := strings.Index(main, "renderIndexInfo(")
	if i < 0 || !strings.Contains(main[max(0, i-200):i], "indexEnabled()") {
		t.Error("the inspector asks /index/status without checking index.enabled")
	}
}

// TestContentSearchRoutesAreRegistered: everything the page calls is a
// route the daemon serves, so a renamed handler cannot leave a dead
// button behind.
func TestContentSearchRoutesAreRegistered(t *testing.T) {
	s := &Server{}
	registered := map[string]bool{}
	for _, r := range s.routes() {
		registered[r.pattern] = true
	}
	for _, want := range []string{"/index/search", "/index/text", "/index/status", "/index/add", "/index/remove"} {
		if !registered[want] {
			t.Errorf("the page calls %s, which routes() does not register", want)
		}
	}
}
