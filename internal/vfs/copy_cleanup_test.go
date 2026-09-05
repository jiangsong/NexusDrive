package vfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
)

func TestCopyCleanupRefusesLocalReferencesAndPreservesOpenLease(t *testing.T) {
	dir := killCopyPreparation(t, "bind")
	e, job := recoverCopyPreparation(t, dir)
	ctx := context.Background()
	if err := e.fs.CancelCopy(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.ForgetCopy(ctx, job.ID); !errors.Is(err, meta.ErrCopyReferenced) {
		t.Fatalf("discarded referenced local content: %v", err)
	}
	key := cache.FileKey{Remote: "ali", RemoteID: localRemoteID(job.ID), Version: localVersion(job.ID)}
	lease, err := e.cache.OpenWhole(key)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	n, err := e.store.Resolve(ctx, "/ali/dest")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Remove(ctx, n.ParentIno, n.Name, false); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.ForgetCopy(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.j.GetCopy(ctx, job.ID); !errors.Is(err, journal.ErrNotFound) {
		t.Fatalf("history remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "journal", "copies", job.ID+".part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("payload remains: %v", err)
	}
	if e.cache.Stats().LeasedBytes != job.Spec.Size {
		t.Fatalf("forgot charged lease: %+v", e.cache.Stats())
	}
	b := make([]byte, job.Spec.Size)
	if _, err := lease.ReadAt(b, 0); err != nil || !bytes.Equal(b, copyRecoveryBody()) {
		t.Fatalf("cleanup invalidated open lease: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if e.cache.Stats().LeasedBytes != 0 {
		t.Fatal("closed lease remains charged")
	}
	if e.fake.TotalCalls() != 0 {
		t.Fatal("cleanup contacted remote provider")
	}
}

func TestCopyCleanupResumesAfterProcessKill(t *testing.T) {
	dir := killCopyPreparation(t, "cleanup")
	e, job := recoverCopyPreparation(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if job.State != journal.CopyPurging || job.Checkpoint != 1<<20 {
		t.Fatalf("cleanup intent lost: %+v", job)
	}
	p := filepath.Join(dir, "journal", "copies", job.ID+".part")
	if b, err := os.ReadFile(p); err != nil || !bytes.Equal(b, copyRecoveryBody()[:job.Checkpoint]) {
		t.Fatalf("startup GC removed pending cleanup: size=%d %v", len(b), err)
	}
	e.fs.StartCopies(ctx, time.Hour)
	defer e.fs.StopCopies()
	for {
		_, err := e.j.GetCopy(ctx, job.ID)
		if errors.Is(err, journal.ErrNotFound) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ctx.Err() != nil {
			t.Fatalf("cleanup stalled: %s", e.fs.CopyWarning())
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup did not remove payload: %v", err)
	}
	if e.fake.TotalCalls() != 0 {
		t.Fatal("cleanup resumed download/upload")
	}
}

func TestCopyCleanupFailureStaysVisibleAndCanRetry(t *testing.T) {
	dir := killCopyPreparation(t, "download")
	e, job := recoverCopyPreparation(t, dir)
	ctx := context.Background()
	if err := e.fs.CancelCopy(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	e.fs.copyCleanupFault = func() error { return errors.New("cleanup storage unavailable") }
	if err := e.fs.ForgetCopy(ctx, job.ID); err == nil {
		t.Fatal("ignored cleanup fault")
	}
	current, err := e.j.GetCopy(ctx, job.ID)
	if err != nil || current.State != journal.CopyPurging {
		t.Fatalf("cleanup not inspectable: %+v %v", current, err)
	}
	if err := e.fs.RetryCopy(ctx, job.ID); !errors.Is(err, journal.ErrCopyState) {
		t.Fatalf("restarted retiring copy: %v", err)
	}
	e.fs.copyCleanupFault = nil
	if err := e.fs.ForgetCopy(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCopyCleanupCacheFailureDoesNotDiscardJournalPayload(t *testing.T) {
	dir := killCopyPreparation(t, "download")
	e, job := recoverCopyPreparation(t, dir)
	ctx := context.Background()
	if err := e.fs.CancelCopy(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(dir, "journal", "copies", job.ID+".part")
	key := cache.FileKey{Remote: "ali", RemoteID: localRemoteID(job.ID), Version: localVersion(job.ID)}
	// Simulate an unreferenced private cache alias and a failed unlink.
	if err := e.cache.LinkPinnedFile(key, payload, job.Checkpoint); err != nil {
		t.Fatal(err)
	}
	p, ok := e.cache.HydratedPath(key)
	if !ok {
		t.Fatal("cache alias not installed")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0700); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.ForgetCopy(ctx, job.ID); err == nil {
		t.Fatal("cache failure was ignored")
	}
	current, err := e.j.GetCopy(ctx, job.ID)
	if err != nil || current.State != journal.CopyPurging {
		t.Fatalf("cache failure lost cleanup intent: %+v %v", current, err)
	}
	if b, err := os.ReadFile(payload); err != nil || !bytes.Equal(b, copyRecoveryBody()[:job.Checkpoint]) {
		t.Fatalf("cache failure discarded payload: %v", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(payload, p); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.ForgetCopy(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(payload); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry left payload: %v", err)
	}
	if e.fake.TotalCalls() != 0 {
		t.Fatal("cleanup used provider")
	}
}
