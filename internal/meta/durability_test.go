package meta

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"cloudfs/internal/provider"
)

func TestDurableTransactionUsesFullAndRestoresConnection(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "meta.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.db.SetMaxOpenConns(1)
	ctx := context.Background()
	fault := errors.New("abort transaction")
	for _, failure := range []error{nil, fault} {
		err := s.durableTx(ctx, func(tx *sql.Tx) error {
			var mode int
			if err := tx.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode); err != nil {
				return err
			}
			if mode != 2 {
				t.Fatalf("publication synchronous=%d, want FULL", mode)
			}
			return failure
		})
		if !errors.Is(err, failure) {
			t.Fatalf("transaction=%v want=%v", err, failure)
		}
		var mode int
		if err := s.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode); err != nil || mode != 1 {
			t.Fatalf("connection policy=%d %v", mode, err)
		}
	}
}

func TestPublishByInoIsDurableAndKeepsIdentity(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "meta.db")
	s, err := Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	n, err := s.Insert(ctx, Node{ParentIno: RootIno, Name: "file", Remote: "ali", Kind: provider.KindFile})
	if err != nil {
		t.Fatal(err)
	}
	n.Size, n.RemoteID, n.Version, n.Dirty = 10, "cloudfs-local:upload", "local-version", true
	s.db.SetMaxOpenConns(1)
	if err := s.PublishByIno(ctx, n); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Resolve(ctx, "/file")
	if err != nil || got.Ino != n.Ino || got.Size != 10 || got.RemoteID != n.RemoteID || !got.Dirty {
		t.Fatalf("publication=%+v %v", got, err)
	}
}
