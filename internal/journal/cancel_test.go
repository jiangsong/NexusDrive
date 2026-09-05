package journal

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestCancellationSurvivesStaleWorkerTransitionsAndRestart(t *testing.T) {
	ctx := context.Background()
	p := t.TempDir()
	j, err := Open(Options{Dir: p})
	if err != nil {
		t.Fatal(err)
	}
	u := stage(t, j, "cancel", []byte("retained"))
	if err := j.Commit(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Claim(ctx, u.Remote, 1); err != nil {
		t.Fatal(err)
	}
	if state, err := j.RequestCancel(ctx, u.ID); err != nil || state != StateCancelling {
		t.Fatalf("request: %s %v", state, err)
	}
	for name, fn := range map[string]func() error{
		"retry":     func() error { return j.Retry(ctx, u.ID, errors.New("late failure"), time.Second) },
		"defer":     func() error { return j.Defer(ctx, u.ID, 0, "late cancellation") },
		"succeed":   func() error { return j.Succeed(ctx, u.ID) },
		"fail":      func() error { return j.Fail(ctx, u.ID, errors.New("late failure")) },
		"commit":    func() error { return j.Commit(ctx, u) },
		"retarget":  func() error { return j.Retarget(ctx, u.ID, "p", "other") },
		"tombstone": func() error { return j.Tombstone(ctx, u.ID) },
		"drop":      func() error { return j.Drop(ctx, u.ID) },
	} {
		if err := fn(); !errors.Is(err, ErrCancelled) {
			t.Fatalf("%s reset stop: %v", name, err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = Open(Options{Dir: p})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err := j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := j.Get(ctx, u.ID)
	if err != nil || row.State != StateCancelled {
		t.Fatalf("restart: %+v %v", row, err)
	}
	if data, err := os.ReadFile(row.BlobPath); err != nil || string(data) != "retained" {
		t.Fatalf("content: %q %v", data, err)
	}
	if rows, err := j.Claim(ctx, u.Remote, 10); err != nil || len(rows) != 0 {
		t.Fatalf("reclaimed: %+v %v", rows, err)
	}
	if err := j.Requeue(ctx, u.ID); !errors.Is(err, ErrCancelled) {
		t.Fatalf("ordinary retry bypassed stop: %v", err)
	}
	if st, err := j.Stats(ctx); err != nil || st.Cancelled != 1 || st.RetainedBytes != u.Size {
		t.Fatalf("stats: %+v %v", st, err)
	}
	if rows, _, err := j.ListActive(ctx, "", 10); err != nil || len(rows) != 1 {
		t.Fatalf("hidden cancelled row: %+v %v", rows, err)
	}
}

func TestCancellationRefusesCompletedCompensationAndNonOwner(t *testing.T) {
	j, _, p := openTest(t)
	ctx := context.Background()
	for _, kind := range []string{"completed", "compensation"} {
		u := stage(t, j, kind, []byte(kind))
		if err := j.Commit(ctx, u); err != nil {
			t.Fatal(err)
		}
		if kind == "completed" {
			if err := j.Succeed(ctx, u.ID); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := j.Tombstone(ctx, u.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := j.RequestCancel(ctx, u.ID); !errors.Is(err, ErrCannotCancel) {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	ro, err := OpenReadOnly(p)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.RequestCancel(ctx, "unknown"); err == nil {
		t.Fatal("non-owner accepted mutation")
	}
}

func TestCancellingOlderUploadBlocksNewerVersionUntilAcknowledged(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	old := stage(t, j, "old", []byte("old"))
	old.Ino = 42
	if err := j.Commit(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Claim(ctx, old.Remote, 1); err != nil {
		t.Fatal(err)
	}
	next := stage(t, j, "new", []byte("new"))
	next.Ino = 42
	if err := j.Commit(ctx, next); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RequestCancel(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	if busy, err := j.OlderInFlight(ctx, next); err != nil || !busy {
		t.Fatalf("older transfer was not fenced: %v %v", busy, err)
	}
	if err := j.FinishCancel(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	if busy, err := j.OlderInFlight(ctx, next); err != nil || busy {
		t.Fatalf("acknowledged stop still blocks: %v %v", busy, err)
	}
}
