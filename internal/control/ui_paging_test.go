package control

import (
	"regexp"
	"strings"
	"testing"
)

// Every listing route already pages: /fs/list and /uploads return a
// next_cursor and the daemon keeps the cursor encoding stable for exactly
// this reason. The page discarded both, so a directory of 900 files showed
// 500 with no sign that the rest existed — the worst kind of missing data,
// the kind that looks like data.
func TestListingScreensFollowTheCursor(t *testing.T) {
	if ui, err := webFS.ReadFile("web/ui.js"); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(ui), "export function moreRow") {
		t.Fatal("the shared next-page control is gone; each screen will grow its own again")
	}
	for _, c := range []struct{ file, cursorParam string }{
		{"web/screens/main.js", "&cursor="},
		{"web/screens/transfers.js", "cursor="},
	} {
		b, err := webFS.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if !strings.Contains(src, "next_cursor") {
			t.Errorf("%s ignores next_cursor", c.file)
		}
		if !strings.Contains(src, c.cursorParam) {
			t.Errorf("%s never asks for the next page (%s)", c.file, c.cursorParam)
		}
		if !strings.Contains(src, "moreRow(") {
			t.Errorf("%s has no control to load the next page", c.file)
		}
	}
}

// A truncated search is a fact about the result, so it belongs beside the
// results and not in a toast that fades after three seconds.
func TestTruncatedSearchSaysSoInTheResults(t *testing.T) {
	if !strings.Contains(webSource(t, "web/screens/main.js"), "t('search.truncated'") {
		t.Fatal("a truncated search result set is not marked in the table")
	}
}

// bareLoaderWiring matches a paged loader handed straight to an event
// handler: `onclick: load` followed by whatever closes the property, rather
// than by the `(` of a call. The two literal spellings this used to compare
// against — "onclick: load," and "onclick: load }" — meant the same bug
// written as `{onclick: load}` passed the test, which is the shape a
// formatter or a hurried edit produces first.
var bareLoaderWiring = regexp.MustCompile(`onclick\s*:\s*load\s*[,}\)\]]`)

// A paged loader takes a cursor. Wiring it straight to an event handler —
// onclick: load — hands it the click event as that cursor, and the daemon
// answers 400 for a cursor that is an event object. pageCursor() now refuses
// anything that is not a string, so the wiring no longer breaks the table;
// the reference still has to be a call, because a loader and the button that
// runs it disagreeing about their one argument is a trap laid for the next
// person to add a parameter.
func TestPagedLoadersAreNotWiredDirectlyToEvents(t *testing.T) {
	// The pattern is the whole test, so it is held to the spellings it has to
	// catch and the ones it must leave alone.
	for _, bad := range []string{"{onclick: load}", "{onclick:load,}", "{ onclick : load }", "el('button', {onclick: load}, x)", "{\n    onclick: load\n  }"} {
		if !bareLoaderWiring.MatchString(bad) {
			t.Errorf("the wiring pattern does not catch %q", bad)
		}
	}
	for _, good := range []string{"onclick: () => load(next),", "onclick: loadAccounts,", "onclick: () => load(),", "onclick: load(next),"} {
		if bareLoaderWiring.MatchString(good) {
			t.Errorf("the wiring pattern falsely accuses %q", good)
		}
	}
	for _, name := range []string{"web/screens/main.js", "web/screens/transfers.js", "web/screens/copies.js"} {
		b, err := webFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if bareLoaderWiring.MatchString(string(b)) {
			t.Errorf("%s passes the click event to a paged loader as its cursor", name)
		}
	}
}
