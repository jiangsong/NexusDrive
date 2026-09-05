package vfs

import (
	"context"
	"errors"
	"testing"

	"cloudfs/internal/cache"
	"cloudfs/internal/journal"
)

func TestPendingUploadCleanupCannotBeRenamedOrDeletedByOrdinaryNamespaceCalls(t *testing.T) {
	e, u := stoppedUpload(t)
	ctx := t.Context()
	identity, err := e.store.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.j.BeginUploadCleanup(ctx, u.ID, identity); err != nil {
		t.Fatal(err)
	}
	n, err := e.store.Get(ctx, u.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Remove(ctx, n.ParentIno, n.Name, false); !errors.Is(err, journal.ErrUploadPurging) {
		t.Fatalf("ordinary remove bypassed cleanup: %v", err)
	}
	if err := e.fs.Rename(ctx, n.ParentIno, n.Name, n.ParentIno, "other"); !errors.Is(err, journal.ErrUploadPurging) {
		t.Fatalf("ordinary rename bypassed cleanup: %v", err)
	}
	if b, err := e.fs.ReadFileRange(ctx, "/ali/resume.txt", 0, 100); err != nil || string(b) != "retained" {
		t.Fatalf("uncoordinated namespace cleanup lost local version: %q %v", b, err)
	}
}

func TestCancelledUploadRetainsReadabilityAndRestoresLostCacheLink(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.WriteFile(ctx, "/ali/cancel.txt", []byte("keep local"), false); err != nil {
		t.Fatal(err)
	}
	rows, err := e.j.Pending(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending: %+v %v", rows, err)
	}
	if _, err := e.up.Cancel(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	n, err := e.store.Resolve(ctx, "/ali/cancel.txt")
	if err != nil {
		t.Fatal(err)
	}
	k := cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
	e.cache.Forget(k)
	if _, err := e.j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
		t.Fatal(err)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/cancel.txt", 0, 10)
	if err != nil || string(got) != "keep local" {
		t.Fatalf("read: %q %v", got, err)
	}
	if row, err := e.j.Get(ctx, rows[0].ID); err != nil || row.State != journal.StateCancelled {
		t.Fatalf("restarted: %+v %v", row, err)
	}
	if err := e.fs.Remove(ctx, n.ParentIno, n.Name, false); !errors.Is(err, journal.ErrCancelled) {
		t.Fatalf("removed unreconciled cancelled version: %v", err)
	}
	if err := e.fs.Rename(ctx, n.ParentIno, n.Name, n.ParentIno, "renamed"); !errors.Is(err, journal.ErrCancelled) {
		t.Fatalf("retargeted cancelled version: %v", err)
	}
}
