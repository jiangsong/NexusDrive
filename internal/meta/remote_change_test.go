package meta

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"cloudfs/internal/provider"
)

func TestRemoteNodeSnapshotCannotOverwriteConcurrentChanges(t *testing.T) {
	for _, action := range []string{"delete", "update"} {
		for _, concurrent := range []string{"dirty", "version", "rename", "deleted", "new-inode"} {
			t.Run(action+"/"+concurrent, func(t *testing.T) {
				s, _ := openTest(t)
				ctx := context.Background()
				n := file(RootIno, "entry", 1)
				n.Remote, n.RemoteID, n.Version = "r", "id", "v1"
				created, err := s.Upsert(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				old, err := s.Get(ctx, created.Ino)
				if err != nil {
					t.Fatal(err)
				}
				current := old
				switch concurrent {
				case "dirty":
					current.Dirty, current.Version = true, "local"
					_, err = s.Upsert(ctx, current)
				case "version":
					current.Version = "newer"
					_, err = s.Upsert(ctx, current)
				case "rename":
					err = s.Rename(ctx, current.Ino, RootIno, "moved")
				case "deleted", "new-inode":
					err = s.Remove(ctx, current.Ino)
					if err == nil && concurrent == "new-inode" {
						current.Version = "new occupant"
						_, err = s.Insert(ctx, current)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				expected, err := s.Children(ctx, RootIno)
				if err != nil {
					t.Fatal(err)
				}
				var next *Node
				if action == "update" {
					n = old
					n.Version = "stale remote event"
					next = &n
				}
				_, applied, err := s.ApplyRemoteNode(ctx, old, next, func(n Node) bool { return n.Dirty })
				wantChanged := concurrent == "version" || concurrent == "rename"
				if applied || (wantChanged && !errors.Is(err, ErrNodeChanged)) || (!wantChanged && err != nil) {
					t.Fatalf("stale event: applied=%v err=%v", applied, err)
				}
				got, err := s.Children(ctx, RootIno)
				if err != nil || !slices.Equal(got, expected) {
					t.Fatalf("concurrent tree overwritten: got=%+v want=%+v err=%v", got, expected, err)
				}
			})
		}
	}
}

// Only the changes that take a name away refuse an older snapshot: a delete,
// and a directory replaced by something that is not a directory, which takes
// away everything inside it. A plain attribute update removes nothing, so it
// uses the soft fence instead — see
// TestARemoteAttributeUpdatePublishesThroughAConcurrentListing.
func TestRemoteNodeChangeFencesParentAndDirectoryListings(t *testing.T) {
	for _, action := range []string{"delete", "replace"} {
		t.Run(action, func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			n, err := s.Upsert(ctx, dir(RootIno, "entry"))
			if err != nil {
				t.Fatal(err)
			}
			old, err := s.Get(ctx, n.Ino)
			if err != nil {
				t.Fatal(err)
			}
			parentListing := beginListingTest(t, s, RootIno)
			if err := parentListing.Append(ctx, []Node{old}); err != nil {
				t.Fatal(err)
			}
			childListing := beginListingTest(t, s, old.Ino)
			var next *Node
			if action != "delete" {
				n = old
				n.Version = "v2"
				if action == "replace" {
					n.Kind = provider.KindFile
				}
				next = &n
			}
			if _, applied, err := s.ApplyRemoteNode(ctx, old, next, nil); err != nil || !applied {
				t.Fatalf("change: applied=%v err=%v", applied, err)
			}
			for _, l := range []*DirListing{parentListing, childListing} {
				if err := l.Commit(ctx, time.Minute, nil); !errors.Is(err, ErrListingChanged) {
					t.Fatalf("stale listing undid remote %s: %v", action, err)
				}
			}
		})
	}
}

func TestRemoteNodeChangeRollsBackTreeIndicesAndFence(t *testing.T) {
	for _, action := range []string{"delete", "replace", "update"} {
		t.Run(action, func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			n, err := s.Upsert(ctx, dir(RootIno, "entry"))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.PutDir(ctx, n.Ino, []Node{file(0, "child", 5)}, time.Hour, nil); err != nil {
				t.Fatal(err)
			}
			if err := s.FlushIndex(ctx); err != nil {
				t.Fatal(err)
			}
			old, err := s.Get(ctx, n.Ino)
			if err != nil {
				t.Fatal(err)
			}
			state, err := s.DirState(ctx, n.Ino)
			if err != nil {
				t.Fatal(err)
			}
			listing := beginListingTest(t, s, RootIno)
			if err := listing.Append(ctx, []Node{old}); err != nil {
				t.Fatal(err)
			}
			trigger := `CREATE TRIGGER fail_remote BEFORE DELETE ON nodes WHEN OLD.name='entry' BEGIN SELECT RAISE(ABORT,'injected'); END`
			var next *Node
			if action != "delete" {
				n = old
				n.Version = "new"
				trigger = `CREATE TRIGGER fail_remote BEFORE UPDATE ON nodes WHEN OLD.name='entry' BEGIN SELECT RAISE(ABORT,'injected'); END`
				if action == "replace" {
					n.Kind = provider.KindFile
					trigger = `CREATE TRIGGER fail_remote BEFORE INSERT ON nodes WHEN NEW.name='entry' BEGIN SELECT RAISE(ABORT,'injected'); END`
				}
				next = &n
			}
			if _, err := s.db.ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			if _, applied, err := s.ApplyRemoteNode(ctx, old, next, nil); err == nil || applied {
				t.Fatalf("failed mutation confirmed: applied=%v err=%v", applied, err)
			}
			if got, err := s.Get(ctx, old.Ino); err != nil || got != old {
				t.Fatalf("old object lost: %+v %v", got, err)
			}
			if got, err := s.DirState(ctx, old.Ino); err != nil || got != state {
				t.Fatalf("freshness changed: %+v %v", got, err)
			}
			for _, q := range []string{"child", "ch"} {
				if hits, err := s.Search(ctx, q, 10); err != nil || len(hits) != 1 {
					t.Fatalf("index lost: %+v %v", hits, err)
				}
			}
			if _, err := s.db.ExecContext(ctx, `DROP TRIGGER fail_remote`); err != nil {
				t.Fatal(err)
			}
			if err := listing.Commit(ctx, time.Minute, nil); err != nil {
				t.Fatalf("failed mutation leaked fence: %v", err)
			}
		})
	}
}
