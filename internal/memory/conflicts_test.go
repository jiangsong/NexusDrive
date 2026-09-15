package memory

import (
	"reflect"
	"testing"
)

func TestConflictSiblingsArePrefixMatchesThatAreNotFacts(t *testing.T) {
	names := []string{
		"style.md",                       // the fact itself
		"style (conflict 2026-09-15).md", // a copy, whatever the provider calls it
		"style.md.sb-1a2b",               // another naming scheme
		"style-guide.md",                 // a different, well-formed fact
		"style2.md",                      // likewise
		"stylus.md",                      // shares four letters, not the name
		"other (conflict).md",            // someone else's copy
	}
	got := conflictsOf(names, "style")
	want := []string{"style (conflict 2026-09-15).md", "style.md.sb-1a2b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := conflictsOf(names, "other"); !reflect.DeepEqual(got, []string{"other (conflict).md"}) {
		t.Fatalf("%v", got)
	}
	if got := conflictsOf(names, "nothing"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// Every non-fact entry counts once for the agent summary.
	if n := countConflicts(names); n != 3 {
		t.Fatalf("countConflicts = %d", n)
	}
}

func TestIndexLineEditing(t *testing.T) {
	line := indexLine("style", "coding style")
	if line != "- [style](facts/style.md) — coding style" {
		t.Fatalf("%q", line)
	}
	if got := indexLine("style", ""); got != "- [style](facts/style.md)" {
		t.Fatalf("no description: %q", got)
	}
	// Empty index: append.
	if got := upsertIndexLine("", "style", "d"); got != "- [style](facts/style.md) — d\n" {
		t.Fatalf("%q", got)
	}
	// Unique match: replaced in place, other lines untouched, no trailing
	// newline is added where one was missing.
	in := "# Memory\n- [a](facts/a.md) — A\n- [style](facts/style.md) — old\n- [b](facts/b.md) — B"
	want := "# Memory\n- [a](facts/a.md) — A\n- [style](facts/style.md) — new\n- [b](facts/b.md) — B\n"
	if got := upsertIndexLine(in, "style", "new"); got != want {
		t.Fatalf("%q", got)
	}
	// Two matches: neither is guessed at, the line is appended.
	in = "- [style](facts/style.md) — one\n- [style](facts/style.md) — two\n"
	if got := upsertIndexLine(in, "style", "three"); got != in+"- [style](facts/style.md) — three\n" {
		t.Fatalf("%q", got)
	}
	// A different fact whose name shares a prefix is not a match.
	in = "- [style-guide](facts/style-guide.md) — g\n"
	if got := upsertIndexLine(in, "style", "s"); got != in+"- [style](facts/style.md) — s\n" {
		t.Fatalf("%q", got)
	}
	// Delete drops every line pointing at the fact.
	in = "# H\n- [style](facts/style.md) — one\n- [a](facts/a.md)\n- [style](facts/style.md) — two\n"
	if got := removeIndexLines(in, "style"); got != "# H\n- [a](facts/a.md)\n" {
		t.Fatalf("%q", got)
	}
}
