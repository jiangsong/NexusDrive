package control

import (
	"strings"
	"testing"
)

// The main window's search used to be a listener inside main.js that always
// sent path=<cwd>: a search from /work could not find a file in /archive,
// the rows had empty size and date cells, and nothing on screen said how
// much of the tree the index had seen. These tests read the shipped assets,
// so the wiring that fixed each of those cannot quietly regress.

// TestNameSearchDefaultsToTheWholeDrive: the request names no path unless
// the person chose "this folder", the choice survives a reload, and main.js
// no longer talks to /search itself.
func TestNameSearchDefaultsToTheWholeDrive(t *testing.T) {
	src := webSource(t, "web/name_search.js")
	for _, want := range []string{
		"searchURL(",
		"=== 'cwd' ? 'cwd' : 'all'",
		"localStorage.getItem(SEARCH_SCOPE_KEY)",
		"localStorage.setItem(SEARCH_SCOPE_KEY, scope)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("name_search.js lacks %s", want)
		}
	}
	// localStorage throws in a private window and in some embedded views;
	// a search box that cannot remember a preference must still search.
	if strings.Count(src, "try {") < 2 {
		t.Error("scope storage is not guarded")
	}
	main := webSource(t, "web/screens/main.js")
	if strings.Contains(main, "api.get('/search?") || !strings.Contains(main, "mountNameSearch(") {
		t.Error("main.js must wire name_search.js instead of calling /search itself")
	}
}

// TestIndexWholeTreeAsksForConfirmation: listing the whole tree is one
// provider call per directory, and on an unofficial API that can flag the
// account. The button goes through the typed confirmation and sends the
// confirm flag the daemon insists on for depth -1.
func TestIndexWholeTreeAsksForConfirmation(t *testing.T) {
	src := webSource(t, "web/name_search.js")
	for _, want := range []string{
		"confirmToken: 'warm'",
		"api.post('/cache/warm', { path: '/', depth: -1, all: true, confirm: true })",
		"r.coverage.listed",
		"r.coverage.known",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("name_search.js lacks %s", want)
		}
	}
}

// TestSortHeadersChangeTheRequest: the daemon sorts the collected set; the
// page asks it to rather than sorting a page of 100 on its own.
func TestSortHeadersChangeTheRequest(t *testing.T) {
	src := webSource(t, "web/name_search.js")
	for _, want := range []string{"'data-sort': key", "onclick: () => setSort(key)", "'aria-sort'"} {
		if !strings.Contains(src, want) {
			t.Errorf("name_search.js lacks %s", want)
		}
	}
	if !strings.Contains(webSource(t, "web/search_query.js"), "'&sort=' + ") {
		t.Error("searchURL never sends sort=")
	}
}

// TestHighlightIsInsertedAsText: a file name is remote data. The hit runs
// become <mark> elements around text nodes; nothing about a name reaches
// innerHTML.
func TestHighlightIsInsertedAsText(t *testing.T) {
	for _, f := range []string{"web/name_search.js", "web/search_query.js"} {
		if strings.Contains(webSource(t, f), "html:") {
			t.Errorf("%s uses the html attribute on a file name", f)
		}
	}
	if !strings.Contains(webSource(t, "web/name_search.js"), "highlightParts(hit.name, parsed)") {
		t.Error("result names do not go through highlightParts")
	}
}

// TestCoverageLineComesFromI18n: the line above the results is the one
// place the page says how much of the tree a search could see, so it and
// the confirmation around "index the whole tree" exist in both languages.
func TestCoverageLineComesFromI18n(t *testing.T) {
	src := webSource(t, "web/name_search.js")
	for _, want := range []string{
		"t('search.count'",
		"t('search.coverage'",
		"t('search.coverage.index')",
		"t('search.scope.all')",
		"t('search.scope.cwd')",
		"t('search.truncated')",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("name_search.js lacks %s", want)
		}
	}
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, k := range []string{
		"search.count", "search.coverage", "search.coverage.crawling", "search.coverage.index",
		"search.scope", "search.scope.all", "search.scope.cwd", "search.sort",
		"confirm.warm.title", "confirm.warm.body", "toast.warm.started",
	} {
		if !zh[k] {
			t.Errorf("missing %s", k)
		}
	}
}

// TestSearchShortcutsAreWired: Ctrl/⌘+K reaches the box from anywhere on
// the screen, Escape clears it, and a directory row carries its path so a
// result opened by double-click can be selected after its folder loads.
func TestSearchShortcutsAreWired(t *testing.T) {
	src := webSource(t, "web/name_search.js")
	for _, want := range []string{"ev.metaKey", "ev.ctrlKey", "=== 'k'", "'Escape'", "searchBox.focus()"} {
		if !strings.Contains(src, want) {
			t.Errorf("name_search.js lacks %s", want)
		}
	}
	if !strings.Contains(webSource(t, "web/screens/main.js"), "'data-path': e.path") {
		t.Error("directory rows carry no data-path, so a result cannot be selected after opening its folder")
	}
}
