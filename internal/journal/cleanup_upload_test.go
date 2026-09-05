package journal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const cleanupTestIdentity = "metadata-original-instance"

func beginCleanupTest(t *testing.T, j *Journal, u Upload) {
	t.Helper()
	if _, err := j.BeginUploadCleanup(t.Context(), u.ID, cleanupTestIdentity); err != nil {
		t.Fatal(err)
	}
}

func TestUploadCleanupFencesAllLateTransitionsAndRemainsVisible(t *testing.T) {
	j, u := cancelledResumeFixture(t)
	ctx := t.Context()
	if err := j.SetSession(ctx, u.ID, map[string]string{"private": "old-session"}); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordPart(ctx, u.ID, Part{Index: 1, ETag: "old-part"}); err != nil {
		t.Fatal(err)
	}
	resume, err := j.PrepareUploadResume(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	beginCleanupTest(t, j, u)
	for name, mutate := range map[string]func() error{
		"commit":       func() error { return j.Commit(ctx, u) },
		"session":      func() error { return j.SetSession(ctx, u.ID, map[string]string{"late": "secret"}) },
		"part":         func() error { return j.RecordPart(ctx, u.ID, Part{Index: 2, ETag: "late"}) },
		"published":    func() error { return j.MarkPublished(ctx, u.ID) },
		"success":      func() error { return j.Succeed(ctx, u.ID) },
		"retry":        func() error { return j.Retry(ctx, u.ID, errors.New("late"), 0) },
		"defer":        func() error { return j.Defer(ctx, u.ID, 0, "late") },
		"fail":         func() error { return j.Fail(ctx, u.ID, errors.New("late")) },
		"requeue":      func() error { return j.Requeue(ctx, u.ID) },
		"retarget":     func() error { return j.Retarget(ctx, u.ID, "other", "other") },
		"tombstone":    func() error { return j.Tombstone(ctx, u.ID) },
		"drop":         func() error { return j.Drop(ctx, u.ID) },
		"drop-pending": func() error { return j.DropPending(ctx, u.ID) },
	} {
		if err := mutate(); !errors.Is(err, ErrUploadPurging) {
			t.Errorf("%s bypassed cleanup: %v", name, err)
		}
	}
	if err := resume.Commit(ctx); !errors.Is(err, ErrResumeChanged) {
		t.Fatalf("prepared resume bypassed cleanup: %v", err)
	}
	if _, err := j.RequestCancel(ctx, u.ID); !errors.Is(err, ErrCannotCancel) {
		t.Fatal(err)
	}
	if err := j.FinishCancel(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	row, err := j.Get(ctx, u.ID)
	if err != nil || row.State != StatePurging || row.Session["private"] != "old-session" {
		t.Fatalf("cleanup lost frozen upload: %+v %v", row, err)
	}
	parts, err := j.Parts(ctx, u.ID)
	if err != nil || len(parts) != 1 || parts[0].ETag != "old-part" {
		t.Fatalf("late part mutation: %+v %v", parts, err)
	}
	rows, next, err := j.ListActive(ctx, "", 1)
	if err != nil || len(rows) != 1 || rows[0].State != StatePurging || next != "" {
		t.Fatalf("hidden cleanup: %+v %q %v", rows, next, err)
	}
	if rows, err := j.ByIno(ctx, u.Ino); err != nil || len(rows) != 1 || rows[0].State != StatePurging {
		t.Fatalf("namespace lost cleanup fence: %+v %v", rows, err)
	}
	if rows, err := j.Claim(ctx, u.Remote, 10); err != nil || len(rows) != 0 {
		t.Fatalf("cleanup claimed: %+v %v", rows, err)
	}
	if stats, err := j.Stats(ctx); err != nil || stats.Purging != 1 || stats.RetainedBytes != u.Size {
		t.Fatalf("cleanup hidden from stats: %+v %v", stats, err)
	}
	if _, err := j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(u.BlobPath); err != nil || string(b) != "retained contents" {
		t.Fatalf("recovery collected unfinished cleanup: %q %v", b, err)
	}
}

func TestUploadCleanupRetainsIntentAcrossUnlinkAndFinalCommitFailure(t *testing.T) {
	for _, fault := range []string{"unlink", "sync-window", "sql"} {
		t.Run(fault, func(t *testing.T) {
			j, u := cancelledResumeFixture(t)
			ctx := t.Context()
			if err := j.SetSession(ctx, u.ID, map[string]string{"private": "keep-until-finish"}); err != nil {
				t.Fatal(err)
			}
			beginCleanupTest(t, j, u)
			switch fault {
			case "unlink":
				if err := os.Remove(u.BlobPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(u.BlobPath, 0700); err != nil {
					t.Fatal(err)
				}
			case "sync-window":
				j.uploadCleanupFault = func(phase string) error {
					if phase == "unlinked" {
						return errors.New("injected directory sync failure")
					}
					return nil
				}
			case "sql":
				if _, err := j.db.Exec(`CREATE TRIGGER fail_upload_cleanup BEFORE DELETE ON upload_cleanup BEGIN SELECT RAISE(ABORT,'injected final commit failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if err := j.FinishUploadCleanup(ctx, u.ID, cleanupTestIdentity); err == nil {
				t.Fatal("ignored cleanup failure")
			}
			if row, err := j.Get(ctx, u.ID); err != nil || row.State != StatePurging || row.Session["private"] != "keep-until-finish" {
				t.Fatalf("lost intent after failure: %+v %v", row, err)
			}
			var receipts int
			if err := j.db.QueryRow(`SELECT COUNT(*) FROM upload_discarded`).Scan(&receipts); err != nil || receipts != 0 {
				t.Fatalf("receipt survived rollback: %d %v", receipts, err)
			}
			if fault == "unlink" {
				if err := os.Remove(u.BlobPath); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "sql" {
				if _, err := j.db.Exec(`DROP TRIGGER fail_upload_cleanup`); err != nil {
					t.Fatal(err)
				}
			}
			dir := j.dir
			j.Close()
			reopened, err := Open(Options{Dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if _, err := reopened.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if err := reopened.FinishUploadCleanup(ctx, u.ID, cleanupTestIdentity); err != nil {
				t.Fatal(err)
			}
			if err := reopened.FinishUploadCleanup(ctx, u.ID, cleanupTestIdentity); err != nil {
				t.Fatalf("non-idempotent completion: %v", err)
			}
			if _, err := reopened.Get(ctx, u.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("upload not removed: %v", err)
			}
			if _, err := os.Stat(u.BlobPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("private name still exists: %v", err)
			}
		})
	}
}

func TestUploadCleanupKeepsSharedAndUncommittedPayloadOwners(t *testing.T) {
	for _, owner := range []string{"committed", "staged", "relative-alias"} {
		t.Run(owner, func(t *testing.T) {
			j, u := cancelledResumeFixture(t)
			ctx := t.Context()
			next := stage(t, j, "other", []byte("retained contents"))
			if next.BlobPath != u.BlobPath {
				t.Fatal("fixture must share pathname")
			}
			if owner == "relative-alias" {
				wd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				next.BlobPath, err = filepath.Rel(wd, next.BlobPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			if owner != "staged" {
				if err := j.Commit(ctx, next); err != nil {
					t.Fatal(err)
				}
			}
			beginCleanupTest(t, j, u)
			if err := j.FinishUploadCleanup(ctx, u.ID, cleanupTestIdentity); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(next.BlobPath); err != nil || string(b) != "retained contents" {
				t.Fatalf("deleted other owner's bytes: %q %v", b, err)
			}
			if owner == "staged" {
				if err := j.Commit(ctx, next); err != nil {
					t.Fatalf("staged owner lost its payload: %v", err)
				}
			}
			if _, err := j.Get(ctx, next.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := j.RequestCancel(ctx, next.ID); err != nil {
				t.Fatal(err)
			}
			beginCleanupTest(t, j, next)
			if err := j.FinishUploadCleanup(ctx, next.ID, cleanupTestIdentity); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(u.BlobPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("last owner did not remove object: %v", err)
			}
		})
	}
}

func TestUploadCleanupDiscardsPrivateHistoryButPermanentlyFencesID(t *testing.T) {
	j, u := cancelledResumeFixture(t)
	ctx := t.Context()
	if err := j.SetSession(ctx, u.ID, map[string]string{"secret": "signed-url"}); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordPart(ctx, u.ID, Part{Index: 1, ETag: "secret-etag"}); err != nil {
		t.Fatal(err)
	}
	p, err := j.PrepareUploadResume(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := j.Fail(ctx, u.ID, errors.New("private remote failure")); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RequestCancel(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	beginCleanupTest(t, j, u)
	if err := j.FinishUploadCleanup(ctx, u.ID, cleanupTestIdentity); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"uploads", "upload_parts", "dead_letter", "upload_cancellation", "upload_resume_history", "upload_cleanup"} {
		var count int
		if err := j.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("private history remained in %s: %d %v", table, count, err)
		}
	}
	for name, write := range map[string]func() error{
		"commit":    func() error { return j.Commit(ctx, u) },
		"part":      func() error { return j.RecordPart(ctx, u.ID, Part{Index: 99}) },
		"session":   func() error { return j.SetSession(ctx, u.ID, nil) },
		"success":   func() error { return j.Succeed(ctx, u.ID) },
		"failure":   func() error { return j.Fail(ctx, u.ID, errors.New("late")) },
		"published": func() error { return j.MarkPublished(ctx, u.ID) },
	} {
		if err := write(); !errors.Is(err, ErrUploadPurging) {
			t.Errorf("%s revived discarded identity: %v", name, err)
		}
	}
	stale := u
	stale.ID = NewID()
	stale.StagingID = ""
	if err := j.Commit(ctx, stale); !errors.Is(err, ErrUploadPurging) {
		t.Fatalf("adopted discarded missing payload with new ID: %v", err)
	}
	// Content addressing permits a new independent write of identical bytes.
	next := stage(t, j, "new-write", []byte("retained contents"))
	if err := j.Commit(ctx, next); err != nil {
		t.Fatalf("receipt blocked genuinely restaged content: %v", err)
	}
	if b, err := os.ReadFile(next.BlobPath); err != nil || string(b) != "retained contents" {
		t.Fatalf("new payload: %q %v", b, err)
	}
}

func TestUploadCleanupRejectsChangedIdentityStateAndForeignPaths(t *testing.T) {
	for _, state := range []State{StatePending, StateUploading, StateDone, StateDead, StateCancelling} {
		t.Run(string(state), func(t *testing.T) {
			j, _, _ := openTest(t)
			u := stage(t, j, "state", []byte("data"))
			u.State = state
			if err := j.Commit(t.Context(), u); err != nil {
				t.Fatal(err)
			}
			if _, err := j.BeginUploadCleanup(t.Context(), u.ID, cleanupTestIdentity); !errors.Is(err, ErrUploadCleanupState) {
				t.Fatal(err)
			}
		})
	}
	j, u := cancelledResumeFixture(t)
	ctx := t.Context()
	if _, err := j.BeginUploadCleanup(ctx, u.ID, ""); !errors.Is(err, ErrUploadCleanupIdentity) {
		t.Fatal(err)
	}
	fabricated := stage(t, j, "no-intent", []byte("never committed"))
	fabricated.State = StatePurging
	if err := j.Commit(ctx, fabricated); !errors.Is(err, ErrUploadCleanupState) {
		t.Fatalf("created purging row without intent: %v", err)
	}
	if _, err := j.Get(ctx, fabricated.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fabricated cleanup was persisted: %v", err)
	}
	if _, err := j.db.Exec(`UPDATE uploads SET tombstone=1 WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := j.BeginUploadCleanup(ctx, u.ID, cleanupTestIdentity); !errors.Is(err, ErrUploadCleanupState) {
		t.Fatal(err)
	}
	if _, err := j.db.Exec(`UPDATE uploads SET tombstone=0 WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	beginCleanupTest(t, j, u)
	if _, err := j.BeginUploadCleanup(ctx, u.ID, "replacement-metadata"); !errors.Is(err, ErrUploadCleanupIdentity) {
		t.Fatal(err)
	}
	if err := j.FinishUploadCleanup(ctx, u.ID, "replacement-metadata"); !errors.Is(err, ErrUploadCleanupIdentity) {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(foreign, []byte("outside journal"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.db.Exec(`UPDATE uploads SET blob_path=? WHERE id=?`, foreign, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := j.FinishUploadCleanup(ctx, u.ID, cleanupTestIdentity); !errors.Is(err, ErrUploadCleanupContent) {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(foreign); err != nil || string(b) != "outside journal" {
		t.Fatalf("foreign path changed: %q %v", b, err)
	}
}

func TestUploadCleanupMigratesReadOnlyLegacyAndRollsBackIntent(t *testing.T) {
	j, u := cancelledResumeFixture(t)
	ctx := t.Context()
	if _, err := j.db.Exec(`DROP TABLE upload_cleanup; DROP TABLE upload_discarded; PRAGMA user_version=9`); err != nil {
		t.Fatal(err)
	}
	dir := j.dir
	j.Close()
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.UploadCleanupIdentity(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := ro.BeginUploadCleanup(ctx, u.ID, cleanupTestIdentity); err == nil {
		t.Fatal("read-only cleanup succeeded")
	}
	var version int
	if err := ro.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 9 {
		t.Fatalf("read-only migrated: %d %v", version, err)
	}
	ro.Close()
	j, err = Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err := j.db.Exec(`CREATE TRIGGER fail_upload_intent BEFORE UPDATE OF state ON uploads WHEN new.state='purging' BEGIN SELECT RAISE(ABORT,'intent failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := j.BeginUploadCleanup(ctx, u.ID, cleanupTestIdentity); err == nil {
		t.Fatal("intent failure ignored")
	}
	if row, err := j.Get(ctx, u.ID); err != nil || row.State != StateCancelled {
		t.Fatalf("failed intent changed state: %+v %v", row, err)
	}
	if _, err := j.UploadCleanupIdentity(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("failed intent survived rollback")
	}
	if _, err := j.db.Exec(`DROP TRIGGER fail_upload_intent`); err != nil {
		t.Fatal(err)
	}
	beginCleanupTest(t, j, u)
	nonOwner, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer nonOwner.Close()
	if _, err := nonOwner.BeginUploadCleanup(ctx, u.ID, cleanupTestIdentity); err == nil {
		t.Fatal("non-owner began cleanup")
	}
	if err := nonOwner.FinishUploadCleanup(ctx, u.ID, cleanupTestIdentity); err == nil {
		t.Fatal("non-owner finished cleanup")
	}
	if identity, err := nonOwner.UploadCleanupIdentity(ctx, u.ID); err != nil || identity != cleanupTestIdentity {
		t.Fatalf("read-only intent inspection: %q %v", identity, err)
	}
}

func TestUploadCleanupRejectsSymlinkPayloadAndParent(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(fmt.Sprint(parent), func(t *testing.T) {
			j, u := cancelledResumeFixture(t)
			beginCleanupTest(t, j, u)
			foreign := t.TempDir()
			if parent {
				if err := os.Rename(j.ObjectsDir(), filepath.Join(foreign, "objects")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(foreign, "objects"), j.ObjectsDir()); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(u.BlobPath, filepath.Join(foreign, "keep")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(foreign, "keep"), u.BlobPath); err != nil {
					t.Fatal(err)
				}
			}
			if err := j.FinishUploadCleanup(context.Background(), u.ID, cleanupTestIdentity); !errors.Is(err, ErrUploadCleanupContent) {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(u.BlobPath); err != nil || string(b) != "retained contents" {
				t.Fatalf("symlink target changed: %q %v", b, err)
			}
		})
	}
}

func TestUploadCleanupAndResumeHaveOneAtomicWinner(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := t.Context()
	for range 30 {
		u := stage(t, j, "race", []byte("immutable retained bytes"))
		if err := j.Commit(ctx, u); err != nil {
			t.Fatal(err)
		}
		if _, err := j.RequestCancel(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
		resume, err := j.PrepareUploadResume(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		resumed, purged := make(chan error, 1), make(chan error, 1)
		go func() { <-start; resumed <- resume.Commit(ctx) }()
		go func() { <-start; _, err := j.BeginUploadCleanup(ctx, u.ID, cleanupTestIdentity); purged <- err }()
		close(start)
		r, p := <-resumed, <-purged
		if r == nil {
			if !errors.Is(p, ErrUploadCleanupState) {
				t.Fatalf("resume/cleanup both won: %v %v", r, p)
			}
			if _, err := j.RequestCancel(ctx, u.ID); err != nil {
				t.Fatal(err)
			}
			beginCleanupTest(t, j, u)
		} else if !errors.Is(r, ErrResumeChanged) || p != nil {
			t.Fatalf("cleanup/resume result: %v %v", p, r)
		}
		if err := j.FinishUploadCleanup(ctx, u.ID, cleanupTestIdentity); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUploadCleanupOfSubmittedCopyKeepsSourceAndPreparationHistory(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := t.Context()
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
	u, err := c.Submit(ctx, Upload{Remote: "target", RemoteParentID: "root", Name: "file"})
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.RequestCancel(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	beginCleanupTest(t, j, u)
	if err := j.FinishUploadCleanup(ctx, u.ID, cleanupTestIdentity); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(u.BlobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copy private payload remains: %v", err)
	}
	if b, err := os.ReadFile(source); err != nil || string(b) != "abc" {
		t.Fatalf("shared source destroyed: %q %v", b, err)
	}
	if job, err := j.GetCopy(ctx, u.ID); err != nil || job.State != CopySubmitted {
		t.Fatalf("upload cleanup erased/reclassified preparation: %+v %v", job, err)
	}
	// The preparation remains independently forgettable, without reviving its
	// upload ID or needing an already removed private payload to exist.
	if err := j.BeginCopyCleanup(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := j.FinishCopyCleanup(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := j.Commit(ctx, u); !errors.Is(err, ErrUploadPurging) {
		t.Fatalf("copy cleanup erased discard fence: %v", err)
	}
}

func TestUploadCleanupProtectedFromLegacyDropUsingPathAlias(t *testing.T) {
	j, u := cancelledResumeFixture(t)
	ctx := t.Context()
	other := stage(t, j, "other", []byte("retained contents"))
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	other.BlobPath, err = filepath.Rel(wd, other.BlobPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Commit(ctx, other); err != nil {
		t.Fatal(err)
	}
	beginCleanupTest(t, j, u)
	if err := j.DropPending(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(u.BlobPath); err != nil || string(b) != "retained contents" {
		t.Fatalf("legacy drop of an alias deleted purging owner's payload: %q %v", b, err)
	}
	if err := j.FinishUploadCleanup(ctx, u.ID, cleanupTestIdentity); err != nil {
		t.Fatal(err)
	}
}
