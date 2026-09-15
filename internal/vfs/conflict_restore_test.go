package vfs

import (
	"context"
	"strings"
	"testing"
)

// TestConflictLoserIsRestoredByTheNextListing: when an upload lands as a
// conflict copy, the node that held the local write keeps a local-only
// identity whose cache entry the conflict branch released. The next listing
// must restore that node to the remote's version (the other side's content)
// rather than keep protecting it as a pending write: otherwise the file
// stays unreadable for the life of the process, and the conflict copy is
// the only readable one.
func TestConflictLoserIsRestoredByTheNextListing(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("shared.md", []byte("version from the server"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/shared.md", []byte("my local edit"), false); err != nil {
		t.Fatal(err)
	}
	// Another client changes the file before our upload runs.
	e.fake.Seed("shared.md", []byte("someone else edited this first"))
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err := e.fs.ReadDirPath(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range entries {
		names = append(names, a.Name)
		if a.Name == "shared.md" && a.LocalOnly {
			t.Fatalf("the losing node is still local-only after the listing: %+v", a)
		}
	}
	if len(names) != 2 || !strings.HasPrefix(names[0]+names[1], "shared") {
		t.Fatalf("listing after the conflict: %v", names)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/shared.md", 0, 0)
	if err != nil {
		t.Fatalf("the original is unreadable after a conflict: %v", err)
	}
	if string(got) != "someone else edited this first" {
		t.Fatalf("the original must show the other side's content, got %q", got)
	}
	for _, n := range names {
		if n == "shared.md" {
			continue
		}
		copyData, err := e.fs.ReadFileRange(ctx, "/ali/"+n, 0, 0)
		if err != nil || string(copyData) != "my local edit" {
			t.Fatalf("conflict copy %s: %q %v", n, copyData, err)
		}
	}
}
