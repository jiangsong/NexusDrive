package memory

import (
	"strings"
	"testing"
)

// TestMerge3TakesOneSidedChangesAndMarksTheRest: against an ancestor, a
// line only one side changed is taken as that side has it; a line both
// changed the same way is taken once; a line both changed differently is
// a conflict block; an insertion by one side lands where it was made.
func TestMerge3TakesOneSidedChangesAndMarksTheRest(t *testing.T) {
	base := "a\nb\nc\nd\n"
	ours := "a\nB\nc\nd\ne\n"   // changed b, appended e
	theirs := "a\nb\nC\nd\n"    // changed c
	got, n := merge3(base, ours, theirs, true)
	if n != 0 || got != "a\nB\nC\nd\ne" {
		t.Fatalf("clean three-way: %q (%d)", got, n)
	}
	got, n = merge3(base, "a\nB\nc\nd\n", "a\nX\nc\nd\n", true)
	if n != 1 || !strings.Contains(got, "<<<<<<< this device\nB\n=======\nX\n>>>>>>> conflict copy") {
		t.Fatalf("both changed b: %q (%d)", got, n)
	}
	got, n = merge3(base, "a\nB\nc\nd\n", "a\nB\nc\nd\n", true)
	if n != 0 || got != "a\nB\nc\nd" {
		t.Fatalf("same change on both sides: %q (%d)", got, n)
	}
	// One side deleted a line the other left alone.
	got, n = merge3(base, "a\nc\nd\n", "a\nb\nc\nD\n", true)
	if n != 0 || got != "a\nc\nD" {
		t.Fatalf("delete vs change elsewhere: %q (%d)", got, n)
	}
}

// TestMerge2MarksEveryDifference: without an ancestor the common lines
// are kept and every differing run is a conflict block.
func TestMerge2MarksEveryDifference(t *testing.T) {
	got, n := merge3("", "a\nb\nc\n", "a\nx\nc\n", false)
	if n != 1 || got != "a\n<<<<<<< this device\nb\n=======\nx\n>>>>>>> conflict copy\nc" {
		t.Fatalf("%q (%d)", got, n)
	}
	got, n = merge3("", "same\n", "same\n", false)
	if n != 0 || got != "same" {
		t.Fatalf("identical: %q (%d)", got, n)
	}
	got, n = merge3("", "", "only theirs\n", false)
	if n != 1 || !strings.Contains(got, "only theirs") {
		t.Fatalf("one side empty: %q (%d)", got, n)
	}
}
