package journal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func cleanupCopy(t *testing.T, j *Journal) (string, string) {
	t.Helper()
	ctx := context.Background()
	c, err := j.BeginCopy(ctx, copySpec(3), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	id := c.Job().ID
	c.Close()
	if err := j.CancelCopy(ctx, id); err != nil {
		t.Fatal(err)
	}
	p, err := j.copyPath(id)
	if err != nil {
		t.Fatal(err)
	}
	return id, p
}

func TestCopyCleanupRetainsIntentAcrossUnlinkAndCommitFailure(t *testing.T) {
	j, _, dir := openTest(t)
	ctx := context.Background()
	id, p := cleanupCopy(t, j)
	if err := j.BeginCopyCleanup(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "abc" {
		t.Fatalf("GC deleted pending cleanup: %q %v", b, err)
	}
	job, err := j.GetCopy(ctx, id)
	if err != nil || job.State != CopyPurging {
		t.Fatalf("pending cleanup invisible: %+v %v", job, err)
	}
	page, next, err := j.ListCopyJobs(ctx, "", 1)
	if err != nil || len(page) != 1 || page[0].State != CopyPurging || next != "" {
		t.Fatalf("cleanup page: %+v %q %v", page, next, err)
	}
	if _, err := j.db.Exec(`CREATE TRIGGER fail_cleanup BEFORE DELETE ON copy_cleanup BEGIN SELECT RAISE(ABORT,'cleanup commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := j.FinishCopyCleanup(ctx, id); err == nil {
		t.Fatal("ignored cleanup commit failure")
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("payload not unlinked at fault: %v", err)
	}
	if job, err := j.GetCopy(ctx, id); err != nil || job.State != CopyPurging {
		t.Fatalf("lost cleanup intent: %+v %v", job, err)
	}
	if _, err := j.db.Exec(`DROP TRIGGER fail_cleanup`); err != nil {
		t.Fatal(err)
	}
	j.Close()
	j, err = Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err := j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := j.FinishCopyCleanup(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := j.GetCopy(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cleanup history remains: %v", err)
	}
}

func TestCopyCleanupRefusesActiveHandlesUploadsAndLateReferences(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	c, err := j.BeginCopy(ctx, copySpec(0), nil)
	if err != nil {
		t.Fatal(err)
	}
	id := c.Job().ID
	if err := j.BeginCopyCleanup(ctx, id); !errors.Is(err, ErrCopyBusy) {
		t.Fatalf("purged open handle: %v", err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := j.BeginCopyCleanup(ctx, id); !errors.Is(err, ErrCopyState) {
		t.Fatalf("purged active state: %v", err)
	}
	c, err = j.ResumeCopy(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	u, err := c.Submit(ctx, Upload{Remote: "target", RemoteParentID: "root", Name: "file"})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := j.BeginCopyCleanup(ctx, id); !errors.Is(err, ErrCopyReferenced) {
		t.Fatalf("purged queued upload: %v", err)
	}
	if err := j.Drop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := j.BeginCopyCleanup(ctx, id); err != nil {
		t.Fatal(err)
	}
	u.ID = NewID()
	if err := j.Commit(ctx, u); !errors.Is(err, ErrCopyState) {
		t.Fatalf("new upload adopted retiring payload: %v", err)
	}
	if err := j.FinishCopyCleanup(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := j.Commit(ctx, u); !errors.Is(err, ErrCopyState) {
		t.Fatalf("new upload adopted removed payload: %v", err)
	}
}

func TestCopyCleanupPreservesDirectoriesAndHardLinkSources(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := j.BeginCopyFromWhole(ctx, copySpec(3), f, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := c.Job().ID
	p, _ := j.copyPath(id)
	c.Close()
	if err := j.CancelCopy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := j.BeginCopyCleanup(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0700); err != nil {
		t.Fatal(err)
	}
	if err := j.FinishCopyCleanup(ctx, id); !errors.Is(err, ErrCopyCorrupt) {
		t.Fatalf("removed unexpected directory: %v", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, p); err != nil {
		t.Fatal(err)
	}
	if err := j.FinishCopyCleanup(ctx, id); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(source); err != nil || string(b) != "abc" {
		t.Fatalf("deleted source: %q %v", b, err)
	}
}

func TestCopyCleanupReadOnlyLegacyAndRollback(t *testing.T) {
	dir, id := legacyCopyAdminDB(t, 5)
	ctx := context.Background()
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.GetCopy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := ro.BeginCopyCleanup(ctx, id); err == nil {
		t.Fatal("read-only cleanup succeeded")
	}
	ro.Close()
	j, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err := j.db.Exec(`CREATE TRIGGER fail_cleanup_intent BEFORE INSERT ON copy_cleanup BEGIN SELECT RAISE(ABORT,'failed intent'); END`); err != nil {
		t.Fatal(err)
	}
	if err := j.BeginCopyCleanup(ctx, id); err == nil {
		t.Fatal("ignored cleanup-intent failure")
	}
	job, err := j.GetCopy(ctx, id)
	if err != nil || job.State != CopyFailed {
		t.Fatalf("lost history on failed intent: %+v %v", job, err)
	}
}
