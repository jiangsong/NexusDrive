package meta

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/provider"
)

func beginListingTest(t *testing.T, s *Store, ino uint64) *DirListing {
	t.Helper()
	n, err := s.Get(context.Background(), ino)
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.BeginDirListing(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestDirListingAtomicMergeProtectionAndChangeBatches(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	if err := s.PutDir(ctx, RootIno, []Node{file(0, "keep", 1), file(0, "gone", 2)}, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	keep, _ := s.Lookup(ctx, RootIno, "keep")
	before, _ := s.DirState(ctx, RootIno)
	c.advance(time.Minute)
	l := beginListingTest(t, s, RootIno)
	for batch := range 3 {
		nodes := make([]Node, DirListingBatch)
		for i := range nodes {
			nodes[i] = file(0, fmt.Sprintf("new-%03d", batch*DirListingBatch+i), 3)
		}
		if err := l.Append(ctx, nodes); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Append(ctx, []Node{file(0, "keep", 9), file(0, "local", 1)}); err != nil {
		t.Fatal(err)
	}
	// Existing tree and freshness remain unchanged until all pages commit.
	if n, err := s.Lookup(ctx, RootIno, "keep"); err != nil || n.Size != 1 {
		t.Fatalf("partial update: %+v %v", n, err)
	}
	if _, err := s.Lookup(ctx, RootIno, "new-000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial insert: %v", err)
	}
	if state, _ := s.DirState(ctx, RootIno); state != before {
		t.Fatal("partial listing published freshness")
	}
	local := file(RootIno, "local", 99)
	local.Dirty = true
	if _, err := s.Upsert(ctx, local); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAbsent(ctx, RootIno, "new-000", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := l.Commit(ctx, time.Minute, func(n Node) bool { return n.Dirty }); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Lookup(ctx, RootIno, "keep"); err != nil || n.Ino != keep.Ino || n.Size != 9 {
		t.Fatalf("keep: %+v %v", n, err)
	}
	if n, err := s.Lookup(ctx, RootIno, "local"); err != nil || n.Size != 99 || !n.Dirty {
		t.Fatalf("local overwritten: %+v %v", n, err)
	}
	if _, err := s.Lookup(ctx, RootIno, "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing remote child survived")
	}
	if absent, _ := s.IsAbsent(ctx, RootIno, "new-000"); absent {
		t.Fatal("new child still negatively cached")
	}
	if count, removed, err := l.Summary(ctx); err != nil || count != 129 || !removed {
		t.Fatalf("summary: %d %v %v", count, removed, err)
	}
	added, removed, updated := 0, 0, 0
	if err := l.Changes(ctx, func(c DirChange) {
		if len(c.Added)+len(c.Removed)+len(c.Updated) > 128 {
			t.Fatal("unbounded change batch")
		}
		added += len(c.Added)
		removed += len(c.Removed)
		updated += len(c.Updated)
	}); err != nil {
		t.Fatal(err)
	}
	if added != 600 || removed != 1 || updated != 1 {
		t.Fatalf("changes: %d/%d/%d", added, removed, updated)
	}
}

func TestDirListingPoisonedCollectionsCannotPublish(t *testing.T) {
	for _, mode := range []string{"duplicate", "cycle", "bad-name", "oversize", "cancel", "storage"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			l := beginListingTest(t, s, RootIno)
			if err := l.Append(ctx, []Node{file(0, "staged", 1)}); err != nil {
				t.Fatal(err)
			}
			var err error
			switch mode {
			case "duplicate":
				err = l.Append(ctx, []Node{file(0, "staged", 2)})
			case "cycle":
				for _, cursor := range []string{"", "a", "b"} {
					if e := l.RecordCursor(ctx, cursor); e != nil {
						t.Fatal(e)
					}
				}
				err = l.RecordCursor(ctx, "a")
			case "bad-name":
				err = l.Append(ctx, []Node{file(0, "../bad", 2)})
			case "oversize":
				err = l.Append(ctx, make([]Node, DirListingBatch+1))
			case "cancel":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				err = l.Append(cancelled, []Node{file(0, "cancelled", 2)})
			case "storage":
				if _, e := l.db.Exec(`CREATE TEMP TRIGGER fail_append BEFORE INSERT ON listing_nodes BEGIN SELECT RAISE(ABORT,'injected storage failure'); END`); e != nil {
					t.Fatal(e)
				}
				err = l.Append(ctx, []Node{file(0, "failure", 2)})
			}
			if err == nil {
				t.Fatal("failure not detected")
			}
			if err := l.Commit(ctx, time.Minute, nil); err == nil {
				t.Fatal("poisoned listing committed")
			}
			if _, err := s.Lookup(ctx, RootIno, "staged"); !errors.Is(err, ErrNotFound) {
				t.Fatal("partial listing escaped")
			}
			if st, _ := s.DirState(ctx, RootIno); st.Complete {
				t.Fatal("failed listing became complete")
			}
		})
	}
}

func TestDirListingCommitFailureRollsBackTreeIndicesAndFreshness(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.PutDir(ctx, RootIno, []Node{file(0, "original", 1)}, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
	before, _ := s.DirState(ctx, RootIno)
	l := beginListingTest(t, s, RootIno)
	if err := l.Append(ctx, []Node{file(0, "replacement", 9)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_listing_state BEFORE UPDATE ON dir_state BEGIN SELECT RAISE(ABORT,'injected publication failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := l.Commit(ctx, time.Hour, nil); err == nil {
		t.Fatal("failed publication accepted")
	}
	if state, _ := s.DirState(ctx, RootIno); state != before {
		t.Fatal("failed publication changed TTL")
	}
	if _, err := s.Lookup(ctx, RootIno, "original"); err != nil {
		t.Fatal("rollback lost original child")
	}
	if _, err := s.Lookup(ctx, RootIno, "replacement"); !errors.Is(err, ErrNotFound) {
		t.Fatal("rollback published replacement")
	}
	for q, want := range map[string]int{"ori": 1, "or": 1, "repl": 0, "re": 0} {
		got, err := s.Search(ctx, q, 10)
		if err != nil || len(got) != want {
			t.Fatalf("index rollback %q: %+v %v", q, got, err)
		}
	}
	if err := l.Changes(ctx, func(DirChange) { t.Fatal("failed transaction produced notifications") }); err == nil {
		t.Fatal("failed changes accepted")
	}
}

func TestDirListingFencesNewerRefreshAndChangedParent(t *testing.T) {
	for _, mode := range []string{"newer-process", "parent-id", "parent-removed", "before-begin", "database-identity"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			parent, err := s.Upsert(ctx, dir(RootIno, "dir"))
			if err != nil {
				t.Fatal(err)
			}
			l := beginListingTest(t, s, parent.Ino)
			if err := l.Append(ctx, []Node{file(0, "stale", 1)}); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "database-identity":
				if _, err := s.DB().Exec(`UPDATE store_identity SET id='another-database'`); err != nil {
					t.Fatal(err)
				}
			case "newer-process":
				other, err := Open(s.path, Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
				newer := beginListingTest(t, other, parent.Ino)
				if err := newer.Append(ctx, []Node{file(0, "current", 2)}); err != nil {
					t.Fatal(err)
				}
				if err := newer.Commit(ctx, time.Minute, nil); err != nil {
					t.Fatal(err)
				}
			case "parent-id", "before-begin":
				changed := parent
				changed.RemoteID = "another-directory"
				if err := s.UpdateByIno(ctx, changed); err != nil {
					t.Fatal(err)
				}
				if mode == "before-begin" {
					if extra, err := s.BeginDirListing(ctx, parent); !errors.Is(err, ErrListingChanged) {
						if extra != nil {
							extra.Close()
						}
						t.Fatalf("stale target accepted: %v", err)
					}
				}
			case "parent-removed":
				if err := s.Remove(ctx, parent.Ino); err != nil {
					t.Fatal(err)
				}
			}
			if err := l.Commit(ctx, time.Minute, nil); !errors.Is(err, ErrListingChanged) {
				t.Fatalf("old listing committed: %v", err)
			}
			if _, err := s.Lookup(ctx, parent.Ino, "stale"); !errors.Is(err, ErrNotFound) {
				t.Fatal("old listing resurrected a child")
			}
		})
	}
}

func TestDirListingTempStorageSlotsAndClose(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	var active []*DirListing
	for range 8 {
		active = append(active, beginListingTest(t, s, RootIno))
	}
	root, _ := s.Get(ctx, RootIno)
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if l, err := s.BeginDirListing(short, root); !errors.Is(err, context.DeadlineExceeded) {
		if l != nil {
			l.Close()
		}
		t.Fatalf("unbounded listing connections: %v", err)
	}
	// Holding all staging slots must not starve the ordinary metadata pool.
	if _, err := s.Upsert(ctx, file(RootIno, "concurrent-write", 1)); err != nil {
		t.Fatal(err)
	}
	if err := active[0].Close(); err != nil {
		t.Fatal(err)
	}
	l := beginListingTest(t, s, RootIno)
	var storage, cachePages int
	if err := l.db.QueryRow(`PRAGMA temp_store`).Scan(&storage); err != nil || storage != 1 {
		t.Fatalf("temporary storage is not file-backed: %d %v", storage, err)
	}
	if err := l.db.QueryRow(`PRAGMA temp.cache_size`).Scan(&cachePages); err != nil || cachePages != -2048 {
		t.Fatalf("unbounded temp page cache: %d %v", cachePages, err)
	}
	for batch := range 20 {
		nodes := make([]Node, DirListingBatch)
		for i := range nodes {
			nodes[i] = file(0, fmt.Sprintf("item-%05d", batch*DirListingBatch+i), 1)
			nodes[i].RemoteID = strings.Repeat("r", 1024)
		}
		if err := l.Append(ctx, nodes); err != nil {
			t.Fatal(err)
		}
	}
	var pages, pageSize int
	if err := l.db.QueryRow(`PRAGMA temp.page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := l.db.QueryRow(`PRAGMA temp.page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if pages*pageSize <= 2048*1024 {
		t.Fatal("fixture never exceeded the TEMP page cache")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(ctx, []Node{file(0, "after-close", 1)}); err == nil {
		t.Fatal("closed store retained active staging")
	}
	if got, err := s.BeginDirListing(ctx, root); err == nil {
		got.Close()
		t.Fatal("closed store accepted staging")
	}
	other, err := Open(filepath.Clean(s.path), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Lookup(ctx, RootIno, "item-00000"); !errors.Is(err, ErrNotFound) {
		t.Fatal("uncommitted temp contents persisted")
	}
	var count int
	if err := other.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'listing_%'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("temporary tables leaked to main schema: %d %v", count, err)
	}
}

func TestDirListingPreservesLocalDescendantsAndRemovesCleanSubtrees(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(fmt.Sprint(staged), func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			preserved, _ := s.Upsert(ctx, dir(RootIno, "preserved"))
			local := file(preserved.Ino, "unuploaded", 1)
			local.Dirty = true
			local, _ = s.Upsert(ctx, local)
			gone, _ := s.Upsert(ctx, dir(RootIno, "gone"))
			for i := range 250 {
				if _, err := s.Upsert(ctx, file(gone.Ino, fmt.Sprintf("child-%03d", i), 1)); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.FlushIndex(ctx); err != nil {
				t.Fatal(err)
			}
			protect := func(n Node) bool { return n.Dirty }
			if staged {
				l := beginListingTest(t, s, RootIno)
				if err := l.Commit(ctx, time.Minute, protect); err != nil {
					t.Fatal(err)
				}
			} else if err := s.PutDir(ctx, RootIno, nil, time.Minute, protect); err != nil {
				t.Fatal(err)
			}
			if n, err := s.Get(ctx, local.Ino); err != nil || n.ParentIno != preserved.Ino {
				t.Fatalf("local descendant lost: %+v %v", n, err)
			}
			if _, err := s.Get(ctx, gone.Ino); !errors.Is(err, ErrNotFound) {
				t.Fatal("clean subtree survived removal")
			}
			for _, q := range []string{"child", "ch"} {
				if rows, err := s.Search(ctx, q, 10); err != nil || len(rows) != 0 {
					t.Fatalf("removed subtree retained search results: %+v %v", rows, err)
				}
			}
			if n, err := s.Get(ctx, RootIno); err != nil || n.Kind != provider.KindDir {
				t.Fatal("subtree removal touched root")
			}
		})
	}
}
