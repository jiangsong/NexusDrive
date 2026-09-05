package meta

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"cloudfs/internal/provider"
)

func TestCopyCleanupFenceRequiresAbsenceAndCorrectDatabase(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "meta.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	identity, err := s.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.Insert(ctx, Node{ParentIno: RootIno, Name: "file", Kind: provider.KindFile, Remote: "ali", RemoteID: "copy-id"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FenceCopyCleanup(ctx, identity, "ali", "copy-id"); !errors.Is(err, ErrCopyReferenced) {
		t.Fatalf("fenced referenced payload: %v", err)
	}
	if err := s.Remove(ctx, n.Ino); err != nil {
		t.Fatal(err)
	}
	if err := s.FenceCopyCleanup(ctx, "another-database", "ali", "copy-id"); !errors.Is(err, ErrCopyTargetChanged) {
		t.Fatalf("accepted different database: %v", err)
	}
	var value int
	if err := s.db.QueryRow(`SELECT value FROM copy_cleanup_fence`).Scan(&value); err != nil || value != 0 {
		t.Fatalf("rejected fence mutated: %d %v", value, err)
	}
	// A trigger forces even a no-op/read-only replacement to fail this test.
	if _, err := s.db.Exec(`CREATE TRIGGER verify_fence BEFORE UPDATE ON copy_cleanup_fence WHEN (SELECT synchronous FROM pragma_synchronous)!=2 BEGIN SELECT RAISE(ABORT,'fence is not FULL'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.FenceCopyCleanup(ctx, identity, "ali", "copy-id"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT value FROM copy_cleanup_fence`).Scan(&value); err != nil || value != 1 {
		t.Fatalf("no real fence write: %d %v", value, err)
	}
}
