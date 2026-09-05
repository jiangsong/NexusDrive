package vfs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/meta"
	"cloudfs/test/fakeprovider"
)

func TestDirectoryRefreshSharesLazyFullListAmongWaiters(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	for i := range 20 {
		e.fake.Seed(fmt.Sprintf("file-%02d", i), []byte("data"))
	}
	if _, err := e.fs.ReadDirPagePath(ctx, "/ali", DirectoryPageOptions{Limit: 1}); err != nil {
		t.Fatal(err)
	}
	root, err := e.fs.StatPath(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	before := e.store.ChildrenScans()
	var refreshed directoryRefresh
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nodes, err := refreshed.children(ctx, e.store, root.Ino)
			if err != nil || len(nodes) != 20 {
				t.Errorf("shared directory result: %d %v", len(nodes), err)
			}
		}()
	}
	wg.Wait()
	if e.store.ChildrenScans()-before != 1 {
		t.Fatal("refresh waiters each loaded a full directory")
	}
}

func TestDirectoryPageAvoidsFullCachedReadsAndPreservesRefreshPolicy(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	for i := range 35 {
		e.fake.Seed(fmt.Sprintf("dir/file-%02d", i), []byte("data"))
	}
	before := e.store.ChildrenScans()
	first, err := e.fs.ReadDirPagePath(ctx, "/ali/dir", DirectoryPageOptions{Limit: 10, Count: true})
	if err != nil || len(first.Entries) != 10 || !first.HasMore || first.Total != 35 {
		t.Fatalf("cold page: %+v %v", first, err)
	}
	if e.store.ChildrenScans() != before {
		t.Fatal("cold refresh unnecessarily reloaded full cached children")
	}
	calls := e.fake.TotalCalls()
	page := first
	seen := 10
	for page.HasMore {
		page, err = e.fs.ReadDirPagePath(ctx, "/ali/dir", DirectoryPageOptions{After: page.Entries[len(page.Entries)-1].Name, Limit: 10})
		if err != nil || len(page.Entries) == 0 {
			t.Fatalf("continuation: %+v %v", page, err)
		}
		seen += len(page.Entries)
	}
	if seen != 35 || e.fake.TotalCalls() != calls || e.store.ChildrenScans() != before {
		t.Fatalf("warm page cost: seen=%d calls=%d scans=%d", seen, e.fake.TotalCalls()-calls, e.store.ChildrenScans()-before)
	}
	// A failed expired refresh must still expose the previous full listing.
	e.clk.advance(2 * time.Minute)
	e.fake.SetFaults(func(f *fakeprovider.Faults) { f.FailNext = 100 })
	page, err = e.fs.ReadDirPagePath(ctx, "/ali/dir", DirectoryPageOptions{After: "file-30", Limit: 10})
	if err != nil || len(page.Entries) != 4 || page.HasMore {
		t.Fatalf("stale page: %+v %v", page, err)
	}
	if e.store.ChildrenScans() != before {
		t.Fatal("stale fallback loaded whole children")
	}
	if _, err := e.fs.ReadDirPagePath(ctx, "/ali/dir/file-00", DirectoryPageOptions{Limit: 1}); !errors.Is(err, ErrNotDir) {
		t.Fatalf("file listing: %v", err)
	}
}

func TestDirectoryPageNameCursorSurvivesDeletionAndBoundsBeforeIO(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	for _, opt := range []DirectoryPageOptions{{Limit: 0}, {Limit: MaxDirectoryPageSize + 1}, {Limit: 1, Offset: -1}, {Limit: 1, Offset: 1, After: "a"}} {
		if _, err := e.fs.ReadDirPagePath(ctx, "/ali/missing", opt); err == nil {
			t.Fatal("invalid page accepted")
		}
	}
	if e.fake.TotalCalls() != 0 {
		t.Fatal("invalid pagination contacted provider")
	}
	for _, name := range []string{"a", "b", "c", "d"} {
		e.fake.Seed(name, []byte("data"))
	}
	page, err := e.fs.ReadDirPagePath(ctx, "/ali", DirectoryPageOptions{Limit: 2})
	if err != nil || page.Entries[1].Name != "b" {
		t.Fatalf("first page: %+v %v", page, err)
	}
	for _, attr := range page.Entries {
		if err := e.store.Remove(ctx, attr.Ino); err != nil {
			t.Fatal(err)
		}
	}
	root, _ := e.fs.StatPath(ctx, "/ali")
	if _, err := e.store.Upsert(ctx, meta.Node{ParentIno: root.Ino, Name: "aa"}); err != nil {
		t.Fatal(err)
	}
	page, err = e.fs.ReadDirPagePath(ctx, "/ali", DirectoryPageOptions{After: "b", Limit: 2})
	if err != nil || len(page.Entries) != 2 || page.Entries[0].Name != "c" || page.Entries[1].Name != "d" || page.HasMore {
		t.Fatalf("deletion shifted continuation: %+v %v", page, err)
	}
	// Synthetic root directories use the same indexed path without a provider.
	page, err = e.fs.ReadDirPagePath(ctx, "/", DirectoryPageOptions{Limit: 1})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Name != "ali" || page.HasMore {
		t.Fatalf("synthetic page: %+v %v", page, err)
	}
}
