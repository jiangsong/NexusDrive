package meta

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"cloudfs/internal/provider"
)

func replaceListingTest(t *testing.T, s *Store, staged bool, n Node, protect func(Node) bool) DirChange {
	t.Helper()
	ctx := context.Background()
	if !staged {
		c, err := s.PutDirChanged(ctx, RootIno, []Node{n}, time.Minute, protect)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	l := beginListingTest(t, s, RootIno)
	if err := l.Append(ctx, []Node{n}); err != nil {
		t.Fatal(err)
	}
	if err := l.Commit(ctx, time.Minute, protect); err != nil {
		t.Fatal(err)
	}
	var out DirChange
	if err := l.Changes(ctx, func(c DirChange) {
		out.Added = append(out.Added, c.Added...)
		out.Removed = append(out.Removed, c.Removed...)
		out.Updated = append(out.Updated, c.Updated...)
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDirectoryReplacementDropsOldTreeAndUsesFreshInode(t *testing.T) {
	for _, staged := range []bool{false, true} {
		for _, mode := range []string{"to-file", "directory-id", "remote", "from-file"} {
			t.Run(fmt.Sprintf("staged=%v/%s", staged, mode), func(t *testing.T) {
				s, _ := openTest(t)
				ctx := context.Background()
				n := dir(RootIno, "entry")
				n.Remote = "r"
				n.RemoteID = "old"
				if mode == "from-file" {
					n.Kind = provider.KindFile
				}
				old, err := s.Upsert(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				if old.IsDir() {
					if err := s.PutDir(ctx, old.Ino, []Node{file(0, "stale-child", 1)}, time.Hour, nil); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.FlushIndex(ctx); err != nil {
					t.Fatal(err)
				}
				next := n
				switch mode {
				case "to-file":
					next.Kind = provider.KindFile
					next.Size = 9
				case "directory-id":
					next.RemoteID = "new"
				case "remote":
					next.Remote = "other"
				case "from-file":
					next.Kind = provider.KindDir
				}
				change := replaceListingTest(t, s, staged, next, nil)
				current, err := s.Lookup(ctx, RootIno, "entry")
				if err != nil || current.Ino == old.Ino || current.Kind != next.Kind || current.RemoteID != next.RemoteID || current.Remote != next.Remote {
					t.Fatalf("replacement reused incompatible inode: old=%+v current=%+v err=%v", old, current, err)
				}
				if _, err := s.Get(ctx, old.Ino); !errors.Is(err, ErrNotFound) {
					t.Fatalf("old inode survived: %v", err)
				}
				if children, err := s.Children(ctx, current.Ino); err != nil || len(children) != 0 {
					t.Fatalf("new directory inherited stale children: %+v %v", children, err)
				}
				if children, err := s.Children(ctx, old.Ino); err != nil || len(children) != 0 {
					t.Fatalf("orphaned old children: %+v %v", children, err)
				}
				if state, err := s.DirState(ctx, current.Ino); err != nil || state.Complete {
					t.Fatalf("replacement inherited freshness: %+v %v", state, err)
				}
				for _, q := range []string{"stale", "st"} {
					if hits, err := s.Search(ctx, q, 10); err != nil || len(hits) != 0 {
						t.Fatalf("old search entries survived: %+v %v", hits, err)
					}
				}
				if len(change.Removed) != 1 || change.Removed[0] != "entry" {
					t.Fatalf("replacement did not invalidate name/subtree: %+v", change)
				}
			})
		}
	}
}

func TestDirectoryReplacementPreservesProtectedDescendants(t *testing.T) {
	for _, staged := range []bool{false, true} {
		for _, mode := range []string{"to-file", "directory-id"} {
			t.Run(fmt.Sprintf("staged=%v/%s", staged, mode), func(t *testing.T) {
				s, _ := openTest(t)
				ctx := context.Background()
				n := dir(RootIno, "entry")
				n.Remote = "r"
				n.RemoteID = "old"
				old, err := s.Upsert(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				child := file(old.Ino, "local", 5)
				child.Dirty = true
				child, err = s.Upsert(ctx, child)
				if err != nil {
					t.Fatal(err)
				}
				next := n
				next.RemoteID = "replacement"
				if mode == "to-file" {
					next.Kind = provider.KindFile
				}
				c := replaceListingTest(t, s, staged, next, func(n Node) bool { return n.Dirty })
				current, err := s.Lookup(ctx, RootIno, "entry")
				if err != nil || current.Ino != old.Ino || !current.IsDir() || current.RemoteID != "old" {
					t.Fatalf("local descendants rebound to replacement: %+v %v", current, err)
				}
				if got, err := s.Get(ctx, child.Ino); err != nil || !got.Dirty || got.ParentIno != old.Ino {
					t.Fatalf("protected child lost: %+v %v", got, err)
				}
				if c.Any() {
					t.Fatalf("rejected replacement emitted changes: %+v", c)
				}
			})
		}
	}
}

func TestDirectoryReplacementRollbackAndChildListingFence(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(fmt.Sprint(staged), func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			old, err := s.Upsert(ctx, dir(RootIno, "entry"))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.PutDir(ctx, old.Ino, []Node{file(0, "child", 5)}, time.Hour, nil); err != nil {
				t.Fatal(err)
			}
			if err := s.FlushIndex(ctx); err != nil {
				t.Fatal(err)
			}
			before, err := s.DirState(ctx, old.Ino)
			if err != nil {
				t.Fatal(err)
			}
			next := old
			next.RemoteID = "new object"
			if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_replace BEFORE INSERT ON nodes WHEN NEW.name='entry' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
				t.Fatal(err)
			}
			if staged {
				l := beginListingTest(t, s, RootIno)
				if err := l.Append(ctx, []Node{next}); err != nil {
					t.Fatal(err)
				}
				err = l.Commit(ctx, time.Minute, nil)
			} else {
				err = s.PutDir(ctx, RootIno, []Node{next}, time.Minute, nil)
			}
			if err == nil {
				t.Fatal("replacement insertion failure ignored")
			}
			if current, err := s.Lookup(ctx, RootIno, "entry"); err != nil || current.Ino != old.Ino || current.RemoteID != old.RemoteID {
				t.Fatalf("failed replacement lost original: %+v %v", current, err)
			}
			if state, err := s.DirState(ctx, old.Ino); err != nil || state != before {
				t.Fatalf("failed replacement lost freshness: %+v %v", state, err)
			}
			for _, q := range []string{"child", "ch"} {
				if hits, err := s.Search(ctx, q, 10); err != nil || len(hits) != 1 {
					t.Fatalf("failed replacement lost index: %+v %v", hits, err)
				}
			}
			if _, err := s.db.ExecContext(ctx, `DROP TRIGGER fail_replace`); err != nil {
				t.Fatal(err)
			}
			childListing := beginListingTest(t, s, old.Ino)
			replaceListingTest(t, s, staged, next, nil)
			if err := childListing.Commit(ctx, time.Minute, nil); !errors.Is(err, ErrListingChanged) {
				t.Fatalf("old child listing published below replaced directory: %v", err)
			}
		})
	}
}

func TestDirectoryVersionChangeKeepsChildrenAndInode(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(fmt.Sprint(staged), func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			old, err := s.Upsert(ctx, dir(RootIno, "entry"))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.PutDir(ctx, old.Ino, []Node{file(0, "child", 5)}, time.Hour, nil); err != nil {
				t.Fatal(err)
			}
			next := old
			next.Version = "v2"
			replaceListingTest(t, s, staged, next, nil)
			if got, err := s.Lookup(ctx, RootIno, "entry"); err != nil || got.Ino != old.Ino || got.Version != "v2" {
				t.Fatalf("version change replaced directory: %+v %v", got, err)
			}
			if children, err := s.Children(ctx, old.Ino); err != nil || len(children) != 1 {
				t.Fatalf("same object lost children: %+v %v", children, err)
			}
			if state, err := s.DirState(ctx, old.Ino); err != nil || !state.Complete {
				t.Fatalf("same object lost listing: %+v %v", state, err)
			}
		})
	}
}

func TestDirectoryReplacementChangeBatchesStayBounded(t *testing.T) {
	for _, count := range []int{42, 43, 130} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			nodes := make([]Node, count)
			for i := range nodes {
				nodes[i] = dir(RootIno, fmt.Sprintf("entry-%03d", i))
			}
			if err := s.PutDir(ctx, RootIno, nodes, time.Minute, nil); err != nil {
				t.Fatal(err)
			}
			l := beginListingTest(t, s, RootIno)
			for i := range nodes {
				nodes[i].Kind = provider.KindFile
			}
			if err := l.Append(ctx, nodes); err != nil {
				t.Fatal(err)
			}
			if err := l.Commit(ctx, time.Minute, nil); err != nil {
				t.Fatal(err)
			}
			if n, removed, err := l.Summary(ctx); err != nil || n != min(129, count*3) || !removed {
				t.Fatalf("incorrect replacement weight: count=%d removed=%v err=%v", n, removed, err)
			}
			var added, removed, updated int
			if err := l.Changes(ctx, func(c DirChange) {
				if len(c.Added)+len(c.Removed)+len(c.Updated) > 128 {
					t.Fatal("oversized replacement notification")
				}
				added += len(c.Added)
				removed += len(c.Removed)
				updated += len(c.Updated)
			}); err != nil {
				t.Fatal(err)
			}
			if added != count || removed != count || updated != count {
				t.Fatalf("replacement notifications lost: %d %d %d", added, removed, updated)
			}
		})
	}
}
