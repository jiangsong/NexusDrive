package vfs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

type listingTestProvider struct {
	provider.Provider
	list func(context.Context, string, string) ([]provider.Entry, string, error)
}

func (p *listingTestProvider) List(ctx context.Context, dir, cursor string) ([]provider.Entry, string, error) {
	return p.list(ctx, dir, cursor)
}

func TestStreamingDirectoryFailureKeepsWholeOldListing(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("original", []byte("data"))
	if _, err := e.fs.ReadDirPagePath(ctx, "/ali", DirectoryPageOptions{Limit: 1}); err != nil {
		t.Fatal(err)
	}
	// Use an existing local file so the later write does not legitimately
	// trigger a separate directory refresh while resolving a missing name.
	if _, err := e.fs.WriteFile(ctx, "/ali/local", []byte("initial"), false); err != nil {
		t.Fatal(err)
	}
	root, _ := e.fs.StatPath(ctx, "/ali")
	dirNode, _ := e.store.Get(ctx, root.Ino)
	oldState, _ := e.store.DirState(ctx, root.Ino)
	e.clk.advance(2 * time.Minute)
	calls := 0
	m := e.mount()
	m.Provider = &listingTestProvider{Provider: e.fake, list: func(ctx context.Context, dir, cursor string) ([]provider.Entry, string, error) {
		calls++
		if cursor != "" {
			if _, err := e.store.Lookup(ctx, root.Ino, "new-000"); !errors.Is(err, meta.ErrNotFound) {
				t.Fatal("first provider page published early")
			}
			// A local create can commit while the collector is between pages.
			if _, err := e.fs.WriteFile(ctx, "/ali/local", []byte("retained"), false); err != nil {
				t.Fatal(err)
			}
			return nil, "", provider.ErrTransient
		}
		entries := make([]provider.Entry, meta.DirListingBatch)
		for i := range entries {
			entries[i] = provider.Entry{ID: fmt.Sprint(i), Name: fmt.Sprintf("new-%03d", i), Kind: provider.KindFile, Size: 1, Version: "v1"}
		}
		return entries, "second", nil
	}}
	if err := e.fs.fetchDir(ctx, m, root.Ino, dirNode, true); !errors.Is(err, provider.ErrTransient) {
		t.Fatalf("incomplete listing: %v", err)
	}
	if calls != 2 {
		t.Fatalf("provider pages: %d", calls)
	}
	if state, _ := e.store.DirState(ctx, root.Ino); state.ListedAt != oldState.ListedAt {
		t.Fatal("partial provider result advanced freshness")
	}
	if _, err := e.store.Lookup(ctx, root.Ino, "original"); err != nil {
		t.Fatal("partial listing deleted original")
	}
	if _, err := e.store.Lookup(ctx, root.Ino, "new-000"); !errors.Is(err, meta.ErrNotFound) {
		t.Fatal("partial listing survived failure")
	}
	if got, err := e.fs.ReadFileRange(ctx, "/ali/local", 0, 100); err != nil || string(got) != "retained" {
		t.Fatalf("local write lost: %q %v", got, err)
	}
}

func TestStreamingDirectoryRejectsEmptyCursorCycle(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	root, _ := e.fs.StatPath(ctx, "/ali")
	dirNode, _ := e.store.Get(ctx, root.Ino)
	m := e.mount()
	calls := 0
	m.Provider = &listingTestProvider{Provider: e.fake, list: func(context.Context, string, string) ([]provider.Entry, string, error) {
		calls++
		switch calls {
		case 1:
			return nil, "a", nil
		case 2:
			return nil, "b", nil
		case 3:
			return nil, "a", nil
		default:
			return nil, "", errors.New("cycle was not detected")
		}
	}}
	if err := e.fs.fetchDir(ctx, m, root.Ino, dirNode, false); err == nil || calls != 3 {
		t.Fatalf("cursor cycle: calls=%d err=%v", calls, err)
	}
	if state, _ := e.store.DirState(ctx, root.Ino); state.Complete {
		t.Fatal("cursor cycle published an empty complete directory")
	}
}

func TestStreamingLargeRefreshCoalescesChangesAndKeepsPageMemoryBounded(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	version := "v1"
	const count = 1200
	calls := 0
	e.fs.mounts[0].Provider = &listingTestProvider{Provider: e.fake, list: func(ctx context.Context, dir, cursor string) ([]provider.Entry, string, error) {
		calls++
		start := 0
		if cursor != "" {
			var err error
			start, err = strconv.Atoi(cursor)
			if err != nil {
				return nil, "", err
			}
		}
		end := min(start+73, count)
		entries := make([]provider.Entry, 0, end-start)
		for i := start; i < end; i++ {
			entries = append(entries, provider.Entry{ID: fmt.Sprint(i), Name: fmt.Sprintf("file-%04d", i), Kind: provider.KindFile, Size: 1, Version: version})
		}
		next := ""
		if end < count {
			next = strconv.Itoa(end)
		}
		return entries, next, nil
	}}
	before := e.store.ChildrenScans()
	page, err := e.fs.ReadDirPagePath(ctx, "/ali", DirectoryPageOptions{Limit: 10, Count: true})
	if err != nil || len(page.Entries) != 10 || page.Total != count || !page.HasMore {
		t.Fatalf("cold page: %+v %v", page, err)
	}
	if calls != 17 || e.store.ChildrenScans() != before {
		t.Fatalf("cold streaming cost: calls=%d scans=%d", calls, e.store.ChildrenScans()-before)
	}
	changes, cancel := e.fs.WatchChanges()
	defer cancel()
	full, individual := 0, 0
	e.fs.SetInvalidateAll(func() { full++ })
	e.fs.SetInvalidate(func(uint64) { individual++ })
	e.fs.SetInvalidateEntry(func(uint64, string) { individual++ })
	version = "v2"
	e.clk.advance(2 * time.Minute)
	page, err = e.fs.ReadDirPagePath(ctx, "/ali", DirectoryPageOptions{Limit: 10})
	if err != nil || len(page.Entries) != 10 || page.Entries[0].Version != "v2" {
		t.Fatalf("refreshed page: %+v %v", page, err)
	}
	if full != 1 || individual != 0 || e.store.ChildrenScans() != before {
		t.Fatalf("unbounded invalidation/full read: bulk=%d individual=%d scans=%d", full, individual, e.store.ChildrenScans()-before)
	}
	select {
	case c := <-changes:
		if !c.Subtree || !c.Affects("/ali/file-1199") || len(c.Paths) != 1 {
			t.Fatalf("incomplete subtree hint: %+v", c)
		}
	default:
		t.Fatal("missing subtree notification")
	}
	select {
	case c := <-changes:
		t.Fatalf("large refresh emitted additional batches: %+v", c)
	default:
	}
}

func TestDirectoryRefreshProtectsPendingFileBelowMissingRemoteDirectory(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("parent/old", []byte("old"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali/parent"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/parent/local", []byte("not uploaded"), false); err != nil {
		t.Fatal(err)
	}
	parent, _ := e.fs.StatPath(ctx, "/ali/parent")
	parentNode, _ := e.store.Get(ctx, parent.Ino)
	if err := e.fake.Delete(ctx, parentNode.RemoteID); err != nil {
		t.Fatal(err)
	}
	e.clk.advance(2 * time.Minute)
	if _, err := e.fs.ReadDirPagePath(ctx, "/ali", DirectoryPageOptions{Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.fs.ReadFileRange(ctx, "/ali/parent/local", 0, 100); err != nil || string(got) != "not uploaded" {
		t.Fatalf("ancestor refresh discarded local file: %q %v", got, err)
	}
}

func TestDirectoryCommitFailureUsesConservativeNotificationFallback(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("original", []byte("old"))
	if _, err := e.fs.ReadDirPagePath(ctx, "/ali", DirectoryPageOptions{Limit: 1}); err != nil {
		t.Fatal(err)
	}
	root, _ := e.fs.StatPath(ctx, "/ali")
	n, _ := e.store.Get(ctx, root.Ino)
	e.fake.Seed("new", []byte("new"))
	e.clk.advance(2 * time.Minute)
	changes, cancel := e.fs.WatchChanges()
	defer cancel()
	full := 0
	e.fs.SetInvalidateAll(func() { full++ })
	if _, err := e.store.DB().Exec(`CREATE TRIGGER fail_directory_publish BEFORE UPDATE ON dir_state BEGIN SELECT RAISE(ABORT,'commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.fetchDir(ctx, e.mount(), root.Ino, n, true); err == nil {
		t.Fatal("publication failure ignored")
	}
	if full != 1 {
		t.Fatalf("no conservative kernel invalidation: %d", full)
	}
	select {
	case c := <-changes:
		if !c.Rescan {
			t.Fatalf("missing uncertain-outcome hint: %+v", c)
		}
	default:
		t.Fatal("missing rescan")
	}
	if _, err := e.store.Lookup(ctx, root.Ino, "new"); !errors.Is(err, meta.ErrNotFound) {
		t.Fatal("failed transaction published new entry")
	}
}
