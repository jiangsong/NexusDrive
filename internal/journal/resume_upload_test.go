package journal

import (
	"context"
	"errors"
	"os"
	"testing"
)

func cancelledResumeFixture(t *testing.T) (*Journal, Upload) {
	t.Helper()
	j, _, _ := openTest(t)
	u := stage(t, j, "resume", []byte("retained contents"))
	u.Ino = 42
	ctx := context.Background()
	if err := j.Commit(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RequestCancel(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	return j, u
}

func TestUploadResumeIsFencedByEveryLaterCancellation(t *testing.T) {
	j, u := cancelledResumeFixture(t)
	ctx := context.Background()
	p, err := j.PrepareUploadResume(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.RequestCancel(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(ctx); !errors.Is(err, ErrResumeChanged) {
		t.Fatalf("older resume overwrote new stop: %v", err)
	}
	if got, err := j.Get(ctx, u.ID); err != nil || got.State != StateCancelled {
		t.Fatalf("state: %+v %v", got, err)
	}
}

func TestUploadResumeStartsFreshAndPreservesPrivateSessionHistory(t *testing.T) {
	j, u := cancelledResumeFixture(t)
	ctx := context.Background()
	if err := j.SetSession(ctx, u.ID, map[string]string{"secret": "retained-session"}); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordPart(ctx, u.ID, Part{Index: 1, ETag: "etag", State: "done"}); err != nil {
		t.Fatal(err)
	}
	p, err := j.PrepareUploadResume(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := j.Get(ctx, u.ID)
	if err != nil || got.State != StatePending || len(got.Session) != 0 {
		t.Fatalf("resume: %+v %v", got, err)
	}
	parts, err := j.Parts(ctx, u.ID)
	if err != nil || len(parts) != 0 {
		t.Fatalf("reused uncertain parts: %+v %v", parts, err)
	}
	var saved int
	if err := j.db.QueryRow(`SELECT count(*) FROM upload_resume_history WHERE upload_id=? AND snapshot LIKE '%retained-session%' AND snapshot LIKE '%etag%'`, u.ID).Scan(&saved); err != nil || saved != 1 {
		t.Fatalf("lost history: %d %v", saved, err)
	}
	if err := p.Commit(ctx); !errors.Is(err, ErrResumeChanged) {
		t.Fatalf("same resume replayed: %v", err)
	}
}

func TestUploadResumeRejectsCorruptContentEvenInPowerMode(t *testing.T) {
	j, u := cancelledResumeFixture(t)
	if err := os.WriteFile(u.BlobPath, []byte("corrupt! contents"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.PrepareUploadResume(context.Background(), u.ID); !errors.Is(err, ErrResumeContent) {
		t.Fatalf("accepted corruption: %v", err)
	}
}

func TestUploadResumeRollbackRetainsCancellationAndSession(t *testing.T) {
	j, u := cancelledResumeFixture(t)
	ctx := context.Background()
	if err := j.SetSession(ctx, u.ID, map[string]string{"secret": "old"}); err != nil {
		t.Fatal(err)
	}
	p, err := j.PrepareUploadResume(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.db.Exec(`CREATE TRIGGER reject_resume BEFORE UPDATE OF state ON uploads WHEN new.state='pending' BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(ctx); err == nil {
		t.Fatal("resume should fail")
	}
	got, err := j.Get(ctx, u.ID)
	if err != nil || got.State != StateCancelled || got.Session["secret"] != "old" {
		t.Fatalf("rollback lost data: %+v %v", got, err)
	}
	var count int
	if err := j.db.QueryRow(`SELECT count(*) FROM upload_resume_history`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("history survived rollback: %d %v", count, err)
	}
	if _, err := j.db.Exec(`DROP TRIGGER reject_resume`); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUploadResumeCannotOvertakeNewerCommit(t *testing.T) {
	j, u := cancelledResumeFixture(t)
	ctx := context.Background()
	p, err := j.PrepareUploadResume(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := stage(t, j, u.Name, []byte("newer"))
	next.Ino = u.Ino
	if err := j.Commit(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(ctx); !errors.Is(err, ErrResumeChanged) {
		t.Fatalf("overtook newer write: %v", err)
	}
}

func TestUploadResumeMigratesLegacyCancellationWithoutRevision(t *testing.T) {
	j, u := cancelledResumeFixture(t)
	ctx := context.Background()
	if _, err := j.db.Exec(`DROP TABLE upload_resume_history; DROP TABLE upload_cancellation; PRAGMA user_version=8`); err != nil {
		t.Fatal(err)
	}
	dir := j.dir
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	p, err := reopened.PrepareUploadResume(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := reopened.db.QueryRow(`SELECT revision FROM upload_resume_history WHERE upload_id=?`, u.ID).Scan(&revision); err != nil || revision != 0 {
		t.Fatalf("legacy snapshot: %d %v", revision, err)
	}
}
