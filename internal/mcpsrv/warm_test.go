package mcpsrv

import (
	"strings"
	"testing"
)

// TestWarmToolListsASubtree: the server's own guidance tells an agent that
// entries under an unlisted directory stay invisible "until warm lists
// them", and search says the same. Warm is the tool that does it, and what
// it buys is measurable: after it, listing a directory it walked costs the
// provider nothing.
func TestWarmToolListsASubtree(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("work/deep/report.md", []byte("x"))
	e.fake.Seed("work/deep/nested/note.md", []byte("y"))

	var out warmOutput
	if res := e.call(t, "warm", warmInput{Path: "/work"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	// /work, /work/deep and /work/deep/nested.
	if out.Directories != 3 || out.Path != "/work" || out.Depth != -1 {
		t.Fatalf("warm = %+v", out)
	}

	calls := e.fake.Calls("List")
	var list listOutput
	if res := e.call(t, "list_directory", listInput{Path: "/work/deep"}, &list); res.IsError {
		t.Fatal(errText(res))
	}
	if n := e.fake.Calls("List") - calls; n != 0 {
		t.Fatalf("list_directory of a warmed directory cost %d provider List calls", n)
	}
	if len(list.Entries) != 2 {
		t.Fatalf("/work/deep has %d entries after warm: %+v", len(list.Entries), list.Entries)
	}

	// A path that is not a directory says so rather than reporting a walk
	// of nothing, which is what FS.Warm does with one on its own.
	res := e.call(t, "warm", warmInput{Path: "/work/deep/report.md"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "pin") {
		t.Fatalf("warm of a file = %q", errText(res))
	}
	res = e.call(t, "warm", warmInput{Path: "/work/absent"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "does not exist") {
		t.Fatalf("warm of a missing path = %q", errText(res))
	}
}

// TestWarmDepthBoundsMatchTheCLI: depth means what `cloudfs warm <path>
// <depth>` means, down to the bounds the control plane validates, so an
// agent and a person reading the same documentation get the same walk.
func TestWarmDepthBoundsMatchTheCLI(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("work/deep/report.md", []byte("x"))
	e.fake.Seed("work/deep/nested/note.md", []byte("y"))

	// depth 0 lists the named directory and nothing below it.
	var out warmOutput
	if res := e.call(t, "warm", warmInput{Path: "/work", Depth: ptr(0)}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if out.Directories != 1 || out.Depth != 0 {
		t.Fatalf("warm depth 0 = %+v", out)
	}
	// depth 1 adds its children.
	out = warmOutput{}
	if res := e.call(t, "warm", warmInput{Path: "/work", Depth: ptr(1)}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if out.Directories != 2 {
		t.Fatalf("warm depth 1 = %+v", out)
	}

	for _, depth := range []int{-2, 1025} {
		res := e.call(t, "warm", warmInput{Path: "/work", Depth: ptr(depth)}, nil)
		if !res.IsError || !strings.Contains(errText(res), "depth") {
			t.Fatalf("warm depth %d = %q", depth, errText(res))
		}
	}
}

// TestWarmRefusesOutsideTheScope: warm walks a whole subtree, so the path
// it starts from is the only place the allowlist can hold it.
func TestWarmRefusesOutsideTheScope(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	e.fake.Seed("private/secret.txt", []byte("x"))
	e.fake.Seed("work/a.txt", []byte("y"))

	res := e.call(t, "warm", warmInput{Path: "/private"}, nil)
	if !res.IsError {
		t.Fatal("warm listed a path outside the allowlist")
	}
	if d, ok := errorDetailOf(res); !ok || d.Code != codeScopeDenied {
		t.Fatalf("refusal detail = %+v (%v)", d, ok)
	}
	if n := e.fake.TotalCalls(); n != 0 {
		t.Fatalf("a refused warm made %d provider calls", n)
	}
	if res := e.call(t, "warm", warmInput{Path: "/work"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
}
