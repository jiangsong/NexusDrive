package perf

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

// TestRemoveTreeCostsNoForegroundCalls: rm -rf of a tree on a writeback
// mount is a local commit per entry. The backend hears about it from the
// queue: one delete per file, and per directory one listing to confirm it
// is empty plus one delete. Nothing on the foreground path, whatever the
// age of the directory listings — rm -rf of a large tree outlives any TTL.
func TestRemoveTreeCostsNoForegroundCalls(t *testing.T) {
	h := newHarness(t, 64<<10, 0, 0)
	ctx := context.Background()
	const dirs, files = 5, 10
	seedTree(h.fake, dirs, files)
	for d := 0; d < dirs; d++ {
		if _, err := h.fs.ReadDirPath(ctx, fmt.Sprintf("/proj/dir%03d", d)); err != nil {
			t.Fatal(err)
		}
	}
	lists := h.fake.Calls("List")
	// Well past the directory TTL, the way the last directories of a long
	// rm -rf are.
	h.advance(48 * time.Hour)
	for d := 0; d < dirs; d++ {
		dir, err := h.fs.StatPath(ctx, fmt.Sprintf("/proj/dir%03d", d))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < files; i++ {
			if err := h.fs.Remove(ctx, dir.Ino, fmt.Sprintf("file%03d.txt", i), false); err != nil {
				t.Fatal(err)
			}
		}
		proj, err := h.fs.StatPath(ctx, "/proj")
		if err != nil {
			t.Fatal(err)
		}
		if err := h.fs.Remove(ctx, proj.Ino, fmt.Sprintf("dir%03d", d), false); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.fake.Calls("List") - lists; got != 0 || h.fake.Calls("Delete") != 0 {
		t.Fatalf("foreground rm -rf: lists=%d deletes=%d, want 0 and 0", got, h.fake.Calls("Delete"))
	}
	if _, err := h.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	wantDeletes := dirs*files + dirs
	if got := h.fake.Calls("Delete"); got != wantDeletes {
		t.Fatalf("background deletes = %d, want %d", got, wantDeletes)
	}
	if got := h.fake.Calls("List") - lists; got != dirs {
		t.Fatalf("background listings = %d, want one per directory (%d)", got, dirs)
	}
	if kids, err := h.fs.ReadDirPath(ctx, "/proj"); err != nil || len(kids) != 0 {
		t.Fatalf("/proj after rm -rf: %v %v", kids, err)
	}
}

// TestRewritingADirectoryListsItOnce: cp -r over an existing tree rewrites
// every file. Each rewrite's conflict check needs the parent's listing;
// the files of one directory share it instead of each fetching their own,
// so the check costs one listing per directory rather than one per file.
func TestRewritingADirectoryListsItOnce(t *testing.T) {
	h := newHarness(t, 64<<10, 0, 0)
	ctx := context.Background()
	const dirs, files = 3, 8
	seedTree(h.fake, dirs, files)
	for d := 0; d < dirs; d++ {
		if _, err := h.fs.ReadDirPath(ctx, fmt.Sprintf("/proj/dir%03d", d)); err != nil {
			t.Fatal(err)
		}
	}
	lists := h.fake.Calls("List")
	for d := 0; d < dirs; d++ {
		for i := 0; i < files; i++ {
			if _, err := h.fs.WriteFile(ctx, fmt.Sprintf("/proj/dir%03d/file%03d.txt", d, i), []byte("rewritten"), false); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := h.fake.Calls("List") - lists; got != 0 {
		t.Fatalf("foreground rewrite listed %d times", got)
	}
	if _, err := h.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	pages := (files + fakeprovider.ListPageSize - 1) / fakeprovider.ListPageSize
	if got := h.fake.Calls("List") - lists; got != dirs*pages {
		t.Fatalf("conflict checks listed %d pages for %d directories, want %d (one listing each)", got, dirs, dirs*pages)
	}
	for d := 0; d < dirs; d++ {
		for i := 0; i < files; i++ {
			p := fmt.Sprintf("proj/dir%03d/file%03d.txt", d, i)
			if got, ok := h.fake.Content(p); !ok || string(got) != "rewritten" {
				t.Fatalf("%s on the backend: %q %v", p, got, ok)
			}
		}
	}
	if kids, err := h.fs.ReadDirPath(ctx, "/proj/dir000"); err != nil || len(kids) != files {
		t.Fatalf("dir000 after the rewrite landed: %d entries %v (a conflict copy would add one)", len(kids), err)
	}
}

// TestDeltaFeedKeepsListingsWarmPastTTL: a directory listed once stays
// local for as long as the change feed keeps confirming nothing happened,
// however long ago the listing was. The TTL is the bound for a backend
// without a feed, or a feed that stopped answering; with a live feed, a
// tree traversed once costs no listings again — rm -rf and cp -r of a tree
// copied half an hour earlier re-listed every directory before this.
func TestDeltaFeedKeepsListingsWarmPastTTL(t *testing.T) {
	h := newHarness(t, 64<<10, 0, 0)
	ctx := context.Background()
	const dirs, files = 4, 6
	seedTree(h.fake, dirs, files)
	r := vfs.NewRefresher(h.fs, time.Minute)
	m := h.fs.Mounts()[0]
	if _, err := r.PollOnce(ctx, m); err != nil {
		t.Fatal(err)
	}
	for d := 0; d < dirs; d++ {
		if _, err := h.fs.ReadDirPath(ctx, fmt.Sprintf("/proj/dir%03d", d)); err != nil {
			t.Fatal(err)
		}
	}
	lists := h.fake.Calls("List")
	// Long past the directory TTL, but the feed has been polled since and
	// had nothing to say.
	h.advance(3 * time.Hour)
	if _, err := r.PollOnce(ctx, m); err != nil {
		t.Fatal(err)
	}
	for d := 0; d < dirs; d++ {
		if _, err := h.fs.ReadDirPath(ctx, fmt.Sprintf("/proj/dir%03d", d)); err != nil {
			t.Fatal(err)
		}
		if _, err := h.fs.StatPath(ctx, fmt.Sprintf("/proj/dir%03d/file000.txt", d)); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.fake.Calls("List") - lists; got != 0 {
		t.Fatalf("traversal with a live feed listed %d times past the TTL, want 0", got)
	}
	// A change on the backend still arrives through the feed and the
	// directory is listed again — once.
	h.fake.Seed("proj/dir001/new.txt", []byte("x"))
	if _, err := r.PollOnce(ctx, m); err != nil {
		t.Fatal(err)
	}
	kids, err := h.fs.ReadDirPath(ctx, "/proj/dir001")
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != files+1 {
		t.Fatalf("dir001 after a remote create: %d entries, want %d", len(kids), files+1)
	}
	// Without the feed answering, the TTL is back in force.
	lists = h.fake.Calls("List")
	h.advance(3 * time.Hour)
	h.fake.SetFaults(func(f *fakeprovider.Faults) { f.FailNext = 1 })
	if _, err := r.PollOnce(ctx, m); err == nil {
		t.Fatal("poll succeeded through the injected fault")
	}
	if _, err := h.fs.ReadDirPath(ctx, "/proj/dir000"); err != nil {
		t.Fatal(err)
	}
	if got := h.fake.Calls("List") - lists; got == 0 {
		t.Fatal("a directory past its TTL was served locally after the feed failed")
	}
}
