package memory

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFrontmatterRoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 15, 10, 30, 0, 0, time.UTC)
	expires := at.Add(24 * time.Hour)
	m := Meta{Name: "style", Description: "coding: style, with punctuation \"quoted\"", Type: "preference",
		Scope: "cloudfs", SourcePaths: []string{"/work/design.md", "/work/todo.md"}, SourceSession: "session-1", ExpiresAt: &expires, UpdatedAt: at}
	file := render(m, "body line\n")
	if !strings.HasPrefix(file, "---\nname: style\n") || !strings.HasSuffix(file, "\n---\nbody line\n") {
		t.Fatalf("%q", file)
	}
	got, body, ok := parseFrontmatter([]byte(file))
	if !ok || body != "body line\n" || !reflect.DeepEqual(got, m) {
		t.Fatalf("ok=%v body=%q meta=%+v", ok, body, got)
	}
}

func TestFrontmatterOnlyReadsTheHead(t *testing.T) {
	// No frontmatter: the whole file is the body.
	if m, body, ok := parseFrontmatter([]byte("just text\n")); ok || body != "just text\n" || !reflect.DeepEqual(m, Meta{}) {
		t.Fatalf("plain file: ok=%v body=%q meta=%+v", ok, body, m)
	}
	// An unterminated block is not frontmatter either.
	if _, body, ok := parseFrontmatter([]byte("---\nname: x\nno end\n")); ok || body != "---\nname: x\nno end\n" {
		t.Fatalf("unterminated: ok=%v body=%q", ok, body)
	}
	// A closing fence past the first 4 KiB is not looked for.
	big := "---\nname: x\n" + strings.Repeat("k: v\n", 1000) + "---\nbody\n"
	if _, _, ok := parseFrontmatter([]byte(big)); ok {
		t.Fatal("a fence beyond 4 KiB must not be parsed")
	}
	// Unknown keys are ignored; a malformed block is treated as body.
	m, body, ok := parseFrontmatter([]byte("---\nname: y\nextra: 1\n---\nrest\n"))
	if !ok || m.Name != "y" || body != "rest\n" {
		t.Fatalf("unknown keys: ok=%v body=%q meta=%+v", ok, body, m)
	}
	if _, _, ok := parseFrontmatter([]byte("---\n: : [\n---\nrest\n")); ok {
		t.Fatal("malformed yaml must not parse")
	}
}
