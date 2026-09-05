package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCopyCancelGatesOpenPreparationAndRetainsCheckpoint(t *testing.T) {
	j, _, dir := openTest(t)
	ctx := context.Background()
	c, err := j.BeginCopy(ctx, copySpec(6), nil)
	if err != nil {
		t.Fatal(err)
	}
	id := c.Job().ID
	if _, err := c.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("bad")); err != nil {
		t.Fatal(err)
	}
	if err := j.CancelCopy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); !errors.Is(err, ErrCopyState) {
		t.Fatalf("cancelled checkpoint advanced: %v", err)
	}
	c.Close()
	if err := j.FailCopy(ctx, id, "foreground unwound"); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	job, err := j.GetCopy(ctx, id)
	if err != nil || job.State != CopyCancelled || job.Checkpoint != 3 {
		t.Fatalf("lost cancellation: %+v %v", job, err)
	}
	p := filepath.Join(dir, "copies", id+".part")
	if b, err := os.ReadFile(p); err != nil || string(b) != "abcbad" {
		t.Fatalf("cancel removed bytes: %q %v", b, err)
	}
	if _, err := j.ResumeCopy(ctx, id); !errors.Is(err, ErrCopyCancelled) {
		t.Fatalf("auto-resumed cancelled: %v", err)
	}
	if err := j.RetryCopy(ctx, id); err != nil {
		t.Fatal(err)
	}
	c, err = j.ResumeCopy(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if b, err := os.ReadFile(p); err != nil || string(b) != "abc" {
		t.Fatalf("retry did not trim unconfirmed tail: %q %v", b, err)
	}
	if _, err := c.Write([]byte("def")); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Submit(ctx, Upload{Remote: "target", RemoteParentID: "root", Name: "file"}); err != nil {
		t.Fatal(err)
	}
	if err := j.CancelCopy(ctx, id); !errors.Is(err, ErrCopyState) {
		t.Fatalf("cancelled upload handoff: %v", err)
	}
	c.Close()
	if err := j.RetryCopy(ctx, id); !errors.Is(err, ErrCopyState) {
		t.Fatalf("retried submitted job: %v", err)
	}
}

func TestCopyCancelWinsAgainstReadySubmit(t *testing.T) {
	for range 20 {
		j, _, _ := openTest(t)
		ctx := context.Background()
		c, err := j.BeginCopy(ctx, copySpec(0), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var cancelled, submitted error
		wg.Add(2)
		go func() { defer wg.Done(); <-start; cancelled = j.CancelCopy(ctx, c.Job().ID) }()
		go func() {
			defer wg.Done()
			<-start
			_, submitted = c.Submit(ctx, Upload{Remote: "target", RemoteParentID: "root", Name: "file"})
		}()
		close(start)
		wg.Wait()
		job, err := j.GetCopy(ctx, c.Job().ID)
		if err != nil {
			t.Fatal(err)
		}
		_, uploadErr := j.Get(ctx, job.ID)
		if cancelled == nil {
			if !errors.Is(submitted, ErrCopyState) || job.State != CopyCancelled || !errors.Is(uploadErr, ErrNotFound) {
				t.Fatalf("cancel lost: %v %+v %v", submitted, job, uploadErr)
			}
		} else if !errors.Is(cancelled, ErrCopyState) || submitted != nil || job.State != CopySubmitted || uploadErr != nil {
			t.Fatalf("invalid winner: cancel=%v submit=%v %+v upload=%v", cancelled, submitted, job, uploadErr)
		}
		c.Close()
	}
}

func TestCopyRetryDoesNotOverwriteLaterCancellation(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	c, err := j.BeginCopy(ctx, copySpec(0), nil)
	if err != nil {
		t.Fatal(err)
	}
	id := c.Job().ID
	c.Close()
	if err := j.CancelCopy(ctx, id); err != nil {
		t.Fatal(err)
	}
	j.copyRetryFault = func() {
		if err := j.CancelCopy(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.RetryCopy(ctx, id); !errors.Is(err, ErrCopyState) {
		t.Fatalf("retry undid later cancellation: %v", err)
	}
	job, err := j.GetCopy(ctx, id)
	if err != nil || job.State != CopyCancelled || job.Revision != 2 {
		t.Fatalf("lost newest revision: %+v %v", job, err)
	}
}

func TestCopyRetryChecksCorruptionDestinationAndOwner(t *testing.T) {
	j, _, dir := openTest(t)
	ctx := context.Background()
	c, err := j.BeginCopy(ctx, copySpec(3), nil)
	if err != nil {
		t.Fatal(err)
	}
	id := c.Job().ID
	if _, err := c.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := j.FailCopy(ctx, id, "retry me"); err != nil {
		t.Fatal(err)
	}
	other, err := j.BeginCopy(ctx, copySpec(3), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := j.RetryCopy(ctx, id); !errors.Is(err, ErrCopyBusy) {
		t.Fatalf("stole active destination: %v", err)
	}
	if err := j.CancelCopy(ctx, other.Job().ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "copies", id+".part"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := j.RetryCopy(ctx, id); !errors.Is(err, ErrCopyCorrupt) {
		t.Fatalf("retried corrupt prefix: %v", err)
	}
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := ro.RetryCopy(ctx, id); err == nil {
		t.Fatal("read-only retry mutated")
	}
	if err := ro.CancelCopy(ctx, id); err == nil {
		t.Fatal("read-only cancel mutated")
	}
	if job, err := j.GetCopy(ctx, id); err != nil || job.State != CopyFailed {
		t.Fatalf("failed retry changed job: %+v %v", job, err)
	}
}

func legacyCopyAdminDB(t *testing.T, version int) (string, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := strings.Replace(copySchema, ",'cancelled'", "", 1)
	old = strings.Replace(old, " revision INTEGER NOT NULL DEFAULT 0,", "", 1)
	if _, err := db.Exec(journalSchema + old); err != nil {
		t.Fatal(err)
	}
	if version == 4 {
		_, err = db.Exec(`PRAGMA user_version = 4`)
	} else {
		_, err = db.Exec(`PRAGMA user_version = 5`)
	}
	if err != nil {
		t.Fatal(err)
	}
	id := NewID()
	spec, _ := json.Marshal(copySpec(3))
	if _, err := db.Exec(`INSERT INTO copy_jobs(id,spec,want,state,target_remote,target_parent,target_name,last_error) VALUES (?,?,'[]','failed','target','root','file','retained')`, id, string(spec)); err != nil {
		t.Fatal(err)
	}
	return dir, id
}

func TestCopyAdminMigrationAndNonOwnerProtection(t *testing.T) {
	for _, version := range []int{4, 5} {
		dir, id := legacyCopyAdminDB(t, version)
		ctx := context.Background()
		ro, err := OpenReadOnly(dir)
		if err != nil {
			t.Fatal(err)
		}
		job, err := ro.GetCopy(ctx, id)
		if err != nil || job.Revision != 0 || job.LastError != "retained" {
			t.Fatalf("old copy unreadable: %+v %v", job, err)
		}
		lock, owner, err := acquireOwnership(dir)
		if err != nil || !owner {
			t.Fatalf("lock: %v %v", owner, err)
		}
		other, err := Open(Options{Dir: dir})
		if err == nil {
			other.Close()
			t.Fatal("non-owner migrated legacy table")
		}
		var got int
		if err := ro.db.QueryRow(`PRAGMA user_version`).Scan(&got); err != nil || got != version {
			t.Fatalf("non-owner changed version: %d %v", got, err)
		}
		releaseOwnership(lock)
		ro.Close()
		j, err := Open(Options{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if err := j.CancelCopy(ctx, id); err != nil {
			t.Fatal(err)
		}
		job, err = j.GetCopy(ctx, id)
		if err != nil || job.State != CopyCancelled || job.Revision != 1 || job.Spec.Size != 3 {
			t.Fatalf("migration lost job: %+v %v", job, err)
		}
		j.Close()
	}
}
