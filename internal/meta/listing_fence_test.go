package meta

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDirListingCannotUndoDeleteOrRename covers the hard fence: a name was
// removed or moved while an older snapshot was in flight, so publishing that
// snapshot would bring the name back. Such a listing must be refused. The soft
// fence — "this directory is stale" — is a different case, and
// TestDirListingPublishesThroughAStaleMarkButStaysIncomplete covers it.
func TestDirListingCannotUndoDeleteOrRename(t *testing.T) {
	for _, action := range []string{"delete", "rename", "replacement-listing", "delete-other", "rename-other"} {
		t.Run(action, func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			original, err := s.Upsert(ctx, file(RootIno, "original", 1))
			if err != nil {
				t.Fatal(err)
			}
			l := beginListingTest(t, s, RootIno)
			if err := l.Append(ctx, []Node{original}); err != nil {
				t.Fatal(err)
			}
			writer := s
			if strings.HasSuffix(action, "-other") {
				writer, err = Open(s.path, Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Close()
			}
			operation := strings.TrimSuffix(action, "-other")
			switch operation {
			case "delete":
				err = writer.Remove(ctx, original.Ino)
			case "rename":
				err = writer.Rename(ctx, original.Ino, RootIno, "renamed")
			case "replacement-listing":
				err = s.PutDir(ctx, RootIno, []Node{file(0, "replacement", 2)}, time.Minute, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := l.Commit(ctx, time.Minute, nil); !errors.Is(err, ErrListingChanged) {
				t.Fatalf("old listing undid %s: %v", action, err)
			}
			if operation == "delete" || operation == "rename" || operation == "replacement-listing" {
				if _, err := s.Lookup(ctx, RootIno, "original"); !errors.Is(err, ErrNotFound) {
					t.Fatal("old name was resurrected")
				}
			}
			if operation == "rename" {
				if n, err := s.Lookup(ctx, RootIno, "renamed"); err != nil || n.Ino != original.Ino {
					t.Fatalf("renamed inode lost: %+v %v", n, err)
				}
			}
		})
	}
}

// TestDirListingPublishesThroughAStaleMarkButStaysIncomplete is the other half
// of the fence. Invalidate says only "this directory is out of date" — a change
// event named an entry we have never listed, or an upload landed without
// describing its result. Nothing was removed, so the snapshot in flight is
// still a truthful listing and refusing it costs the reader its answer for no
// gain. It is published, and the directory is left incomplete so the next
// reader goes back to the backend for whatever the mark was about.
func TestDirListingPublishesThroughAStaleMarkButStaysIncomplete(t *testing.T) {
	for _, action := range []string{"invalidate", "invalidate-all"} {
		t.Run(action, func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			original, err := s.Upsert(ctx, file(RootIno, "original", 1))
			if err != nil {
				t.Fatal(err)
			}
			l := beginListingTest(t, s, RootIno)
			if err := l.Append(ctx, []Node{original, file(0, "listed-later", 2)}); err != nil {
				t.Fatal(err)
			}
			if action == "invalidate" {
				err = s.Invalidate(ctx, RootIno)
			} else {
				err = s.InvalidateAll(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := l.Commit(ctx, time.Minute, nil); err != nil {
				t.Fatalf("a stale mark refused a listing that removed nothing: %v", err)
			}
			if !l.PublishedStale() {
				t.Fatal("the listing published through a stale mark without reporting it")
			}
			// The entries are usable: this is the caller's answer.
			for _, name := range []string{"original", "listed-later"} {
				if _, err := s.Lookup(ctx, RootIno, name); err != nil {
					t.Fatalf("published listing lost %q: %v", name, err)
				}
			}
			// And the directory is not cacheable as a whole, so whatever the
			// mark was about is still fetched next time.
			st, err := s.DirState(ctx, RootIno)
			if err != nil {
				t.Fatal(err)
			}
			if st.Complete {
				t.Fatal("a listing published through a stale mark claimed the directory is complete")
			}
		})
	}
}

func TestDirListingFailedMutationRollsBackItsFence(t *testing.T) {
	for _, action := range []string{"delete", "rename", "invalidate", "invalidate-all", "replacement-listing"} {
		t.Run(action, func(t *testing.T) {
			s, _ := openTest(t)
			ctx := context.Background()
			n, err := s.Upsert(ctx, file(RootIno, "original", 1))
			if err != nil {
				t.Fatal(err)
			}
			l := beginListingTest(t, s, RootIno)
			if err := l.Append(ctx, []Node{n}); err != nil {
				t.Fatal(err)
			}
			var trigger string
			switch action {
			case "delete":
				trigger = `CREATE TRIGGER fail_mutation BEFORE DELETE ON nodes WHEN old.name='original' BEGIN SELECT RAISE(ABORT,'mutation failed'); END`
			case "rename":
				trigger = `CREATE TRIGGER fail_mutation BEFORE UPDATE OF name ON nodes WHEN new.name='renamed' BEGIN SELECT RAISE(ABORT,'mutation failed'); END`
			case "invalidate", "invalidate-all":
				trigger = `CREATE TRIGGER fail_mutation BEFORE UPDATE ON dir_state WHEN new.complete=0 BEGIN SELECT RAISE(ABORT,'mutation failed'); END`
			case "replacement-listing":
				trigger = `CREATE TRIGGER fail_mutation BEFORE INSERT ON nodes WHEN new.name='replacement' BEGIN SELECT RAISE(ABORT,'mutation failed'); END`
			}
			if _, err := s.DB().Exec(trigger); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "delete":
				err = s.Remove(ctx, n.Ino)
			case "rename":
				err = s.Rename(ctx, n.Ino, RootIno, "renamed")
			case "invalidate":
				err = s.Invalidate(ctx, RootIno)
			case "invalidate-all":
				err = s.InvalidateAll(ctx)
			case "replacement-listing":
				err = s.PutDir(ctx, RootIno, []Node{file(0, "replacement", 2)}, time.Minute, nil)
			}
			if err == nil {
				t.Fatal("mutation failure was not injected")
			}
			if err := l.Commit(ctx, time.Minute, nil); err != nil {
				t.Fatalf("failed mutation left a phantom fence: %v", err)
			}
			if l.PublishedStale() {
				t.Fatal("failed mutation left a phantom stale mark")
			}
			if got, err := s.Lookup(ctx, RootIno, "original"); err != nil || got.Ino != n.Ino {
				t.Fatalf("failed mutation changed inode: %+v %v", got, err)
			}
		})
	}
}

func TestDirListingFenceDoesNotAffectUnrelatedDirectoriesOrNoopRename(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	left, err := s.Upsert(ctx, dir(RootIno, "left"))
	if err != nil {
		t.Fatal(err)
	}
	right, err := s.Upsert(ctx, dir(RootIno, "right"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Upsert(ctx, file(left.Ino, "a", 1))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Upsert(ctx, file(right.Ino, "b", 1))
	if err != nil {
		t.Fatal(err)
	}
	l := beginListingTest(t, s, left.Ino)
	if err := l.Append(ctx, []Node{a}); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, a.Ino, left.Ino, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, b.Ino, right.Ino, "c"); err != nil {
		t.Fatal(err)
	}
	if err := s.Invalidate(ctx, right.Ino); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, b.Ino); err != nil {
		t.Fatal(err)
	}
	if err := l.Commit(ctx, time.Minute, nil); err != nil {
		t.Fatalf("unrelated change or no-op rename fenced left: %v", err)
	}
	var generations int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM directory_refresh_generation`).Scan(&generations); err != nil || generations != 1 {
		t.Fatalf("mutations created unnecessary generation rows: %d %v", generations, err)
	}
}

func TestDirListingMoveFencesBothParentsAndPreservesMovedInode(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	left, err := s.Upsert(ctx, dir(RootIno, "left"))
	if err != nil {
		t.Fatal(err)
	}
	right, err := s.Upsert(ctx, dir(RootIno, "right"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := s.Upsert(ctx, file(left.Ino, "source", 11))
	if err != nil {
		t.Fatal(err)
	}
	victim, err := s.Upsert(ctx, file(right.Ino, "target", 22))
	if err != nil {
		t.Fatal(err)
	}
	a := beginListingTest(t, s, left.Ino)
	b := beginListingTest(t, s, right.Ino)
	if err := a.Append(ctx, []Node{source}); err != nil {
		t.Fatal(err)
	}
	if err := b.Append(ctx, []Node{victim}); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, source.Ino, right.Ino, "target"); err != nil {
		t.Fatal(err)
	}
	for _, l := range []*DirListing{a, b} {
		if err := l.Commit(ctx, time.Minute, nil); !errors.Is(err, ErrListingChanged) {
			t.Fatalf("old parent snapshot committed: %v", err)
		}
	}
	if _, err := s.Lookup(ctx, left.Ino, "source"); !errors.Is(err, ErrNotFound) {
		t.Fatal("moved source resurrected")
	}
	if n, err := s.Lookup(ctx, right.Ino, "target"); err != nil || n.Ino != source.Ino || n.Size != 11 {
		t.Fatalf("target reverted to victim: %+v %v", n, err)
	}
}

func TestInvalidateAllFenceStorageFailureRollsBackFreshness(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.PutDir(ctx, RootIno, []Node{file(0, "original", 1)}, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	before, err := s.DirState(ctx, RootIno)
	if err != nil || !before.Complete {
		t.Fatalf("fixture must be fresh: %+v %v", before, err)
	}
	l := beginListingTest(t, s, RootIno)
	if err := l.Append(ctx, []Node{file(0, "original", 1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_generation BEFORE UPDATE ON directory_refresh_generation BEGIN SELECT RAISE(ABORT,'generation write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateAll(ctx); err == nil {
		t.Fatal("fence storage failure ignored")
	}
	if after, err := s.DirState(ctx, RootIno); err != nil || after != before {
		t.Fatalf("fence failure left partial invalidation: %+v %v", after, err)
	}
	if err := l.Commit(ctx, time.Minute, nil); err != nil {
		t.Fatalf("failed invalidate left a phantom fence: %v", err)
	}
}

// TestStaleFenceMigratesAnExistingDatabase: the soft fence arrived as a new
// column on a table that shipped in v9, so an installation upgrading from it
// has to gain the column without losing what the table already held. A user
// who upgrades and finds their metadata gone would have no way back.
func TestStaleFenceMigratesAnExistingDatabase(t *testing.T) {
	p := filepath.Join(t.TempDir(), "v9.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:9] {
		if _, err := db.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version=9;
INSERT INTO nodes(ino,parent_ino,name,kind) VALUES(1,1,'',1),(2,1,'kept',1);
INSERT INTO directory_refresh_generation(ino,generation) VALUES(2,7);`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var generation, stale int64
	if err := s.DB().QueryRow(
		`SELECT generation, stale_generation FROM directory_refresh_generation WHERE ino=2`).Scan(&generation, &stale); err != nil {
		t.Fatal(err)
	}
	// The hard counter keeps counting from where the old database left it: an
	// in-flight listing from before the upgrade must still be fenced.
	if generation != 7 || stale != 0 {
		t.Fatalf("migrated row is generation=%d stale=%d, want 7 and 0", generation, stale)
	}
	// And the new counter works on the migrated row.
	ctx := context.Background()
	if err := s.Invalidate(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(
		`SELECT generation, stale_generation FROM directory_refresh_generation WHERE ino=2`).Scan(&generation, &stale); err != nil {
		t.Fatal(err)
	}
	if generation != 7 || stale != 1 {
		t.Fatalf("after Invalidate: generation=%d stale=%d, want 7 and 1", generation, stale)
	}
}
