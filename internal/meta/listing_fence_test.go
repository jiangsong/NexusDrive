package meta

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDirListingCannotUndoDeleteRenameOrInvalidation(t *testing.T) {
	for _, action := range []string{"delete", "rename", "invalidate", "invalidate-all", "replacement-listing", "delete-other", "rename-other"} {
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
			case "invalidate":
				err = s.Invalidate(ctx, RootIno)
			case "invalidate-all":
				err = s.InvalidateAll(ctx)
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
