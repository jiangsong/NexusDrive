package journal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDropSupersededPreservesAClaimAfterItsSnapshot(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	old := stage(t, j, "old", []byte("old contents"))
	old.Ino = 42
	if err := j.Commit(ctx, old); err != nil {
		t.Fatal(err)
	}
	newer := stage(t, j, "new", []byte("new contents"))
	newer.Ino = 42
	if err := j.Commit(ctx, newer); err != nil {
		t.Fatal(err)
	}
	j.dropSnapshotFault = func() {
		claimed, err := j.Claim(ctx, old.Remote, 1)
		if err != nil || len(claimed) != 1 || claimed[0].ID != old.ID {
			t.Fatalf("claim: %+v %v", claimed, err)
		}
	}
	n, err := j.DropSuperseded(ctx, old.Remote, old.Ino, newer.ID)
	if err != nil || n != 0 {
		t.Fatalf("dropped claimed old version: %d %v", n, err)
	}
	got, err := j.Get(ctx, old.ID)
	if err != nil || got.State != StateUploading {
		t.Fatalf("claimed row lost: %+v %v", got, err)
	}
	if _, err := os.Stat(old.BlobPath); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Get(ctx, newer.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDropPendingRacesWithDeadLetterRetryAndClaim(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		u := stage(t, j, "retry", []byte("retained until drop wins"))
		u.State = StateDead
		if err := j.Commit(ctx, u); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		type result struct {
			claimed []Upload
			err     error
		}
		retried := make(chan result, 1)
		dropped := make(chan error, 1)
		go func() {
			<-start
			if err := j.Requeue(ctx, u.ID); err != nil {
				retried <- result{err: err}
				return
			}
			rows, err := j.Claim(ctx, u.Remote, 1)
			retried <- result{rows, err}
		}()
		go func() { <-start; dropped <- j.DropPending(ctx, u.ID) }()
		close(start)
		r, dropErr := <-retried, <-dropped
		if r.err != nil && !errors.Is(r.err, ErrNotFound) {
			t.Fatal(r.err)
		}
		if len(r.claimed) > 0 {
			if !errors.Is(dropErr, ErrInFlight) {
				t.Fatalf("claimed task discarded at iteration %d: %v", i, dropErr)
			}
			if _, err := os.Stat(u.BlobPath); err != nil {
				t.Fatal(err)
			}
			if err := j.Drop(ctx, u.ID); err != nil {
				t.Fatal(err)
			} // worker is no longer running
		} else if dropErr != nil {
			t.Fatalf("unclaimed task: %v", dropErr)
		}
	}
}

func TestDropProtectsAnIdenticalStagedObjectBeforeItsRowCommits(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	old := stage(t, j, "old", []byte("shared contents"))
	if err := j.Commit(ctx, old); err != nil {
		t.Fatal(err)
	}
	second := stage(t, j, "second", []byte("shared contents"))
	third := stage(t, j, "third", []byte("shared contents"))
	if old.BlobPath != second.BlobPath || second.BlobPath != third.BlobPath {
		t.Fatal("fixture must share content-addressed path")
	}
	if err := j.DropPending(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second.BlobPath); err != nil {
		t.Fatalf("uncommitted staged object removed by old upload drop: %v", err)
	}
	if err := j.Commit(ctx, second); err != nil {
		t.Fatal(err)
	}
	// An idempotent commit cannot release another staging owner's reservation.
	if err := j.Commit(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := j.DropPending(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(third.BlobPath); err != nil {
		t.Fatalf("third staging reservation lost: %v", err)
	}
	if err := j.Commit(ctx, third); err != nil {
		t.Fatal(err)
	}
	if err := j.DropPending(ctx, third.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(third.BlobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("last reference not collected: %v", err)
	}
}

func TestFailedCommitKeepsStagingProtectionForRetry(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	old := stage(t, j, "old", []byte("retryable contents"))
	if err := j.Commit(ctx, old); err != nil {
		t.Fatal(err)
	}
	next := stage(t, j, "fail", []byte("retryable contents"))
	if _, err := j.db.Exec(`CREATE TRIGGER fail_commit BEFORE INSERT ON uploads WHEN new.name='fail' BEGIN SELECT RAISE(ABORT,'injected commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := j.Commit(ctx, next); err == nil {
		t.Fatal("commit should fail")
	}
	if err := j.DropPending(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(next.BlobPath); err != nil {
		t.Fatalf("retry payload lost: %v", err)
	}
	if _, err := j.db.Exec(`DROP TRIGGER fail_commit`); err != nil {
		t.Fatal(err)
	}
	if err := j.Commit(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := j.DropPending(ctx, next.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(next.BlobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reservation was not released: %v", err)
	}
}

func TestIdleDropRollsBackAllBookkeepingOnFailure(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	u := stage(t, j, "retained", []byte("retained contents"))
	if err := j.Commit(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordPart(ctx, u.ID, Part{Index: 1, ETag: "part", State: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Fail(ctx, u.ID, errors.New("failed")); err != nil {
		t.Fatal(err)
	}
	if _, err := j.db.Exec(`CREATE TRIGGER fail_drop BEFORE DELETE ON upload_parts BEGIN SELECT RAISE(ABORT,'injected delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := j.DropPending(ctx, u.ID); err == nil {
		t.Fatal("drop should fail")
	}
	if got, err := j.Get(ctx, u.ID); err != nil || got.State != StateDead {
		t.Fatalf("row lost: %+v %v", got, err)
	}
	if parts, err := j.Parts(ctx, u.ID); err != nil || len(parts) != 1 {
		t.Fatalf("parts lost: %+v %v", parts, err)
	}
	if _, err := os.Stat(u.BlobPath); err != nil {
		t.Fatal(err)
	}
	if _, err := j.db.Exec(`DROP TRIGGER fail_drop`); err != nil {
		t.Fatal(err)
	}
	if err := j.DropPending(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDropDoesNotUnlinkAnExternalBlobPath(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	outside := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(outside, []byte("not owned by journal"), 0600); err != nil {
		t.Fatal(err)
	}
	u := Upload{ID: NewID(), Remote: "r", BlobPath: outside, Size: 20}
	if err := j.Commit(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := j.DropPending(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside object deleted: %v", err)
	}
}

func TestLegacyDropProtectsPathAliasesInEveryOwnerState(t *testing.T) {
	for _, state := range []State{StatePending, StateUploading, StateDone, StateDead, StateCancelling, StateCancelled, StatePurging} {
		for _, relativeOwner := range []bool{false, true} {
			for _, idle := range []bool{false, true} {
				name := fmt.Sprintf("%s/relative-owner=%t/idle=%t", state, relativeOwner, idle)
				t.Run(name, func(t *testing.T) {
					j, _, _ := openTest(t)
					ctx := t.Context()
					owner := stage(t, j, "owner", []byte("shared alias payload"))
					dropped := stage(t, j, "dropped", []byte("shared alias payload"))
					absolute := owner.BlobPath
					wd, err := os.Getwd()
					if err != nil {
						t.Fatal(err)
					}
					relative, err := filepath.Rel(wd, absolute)
					if err != nil {
						t.Fatal(err)
					}
					if relativeOwner {
						owner.BlobPath = relative
					} else {
						dropped.BlobPath = relative
					}
					owner.State = state
					if state == StatePurging {
						owner.State = StateCancelled
					}
					for _, u := range []Upload{owner, dropped} {
						if err := j.Commit(ctx, u); err != nil {
							t.Fatal(err)
						}
					}
					if state == StatePurging {
						beginCleanupTest(t, j, owner)
					}
					if idle {
						err = j.DropPending(ctx, dropped.ID)
					} else {
						err = j.Drop(ctx, dropped.ID)
					}
					if err != nil {
						t.Fatal(err)
					}
					if _, err := j.Get(ctx, dropped.ID); !errors.Is(err, ErrNotFound) {
						t.Fatalf("drop did not remove its row: %v", err)
					}
					if row, err := j.Get(ctx, owner.ID); err != nil || row.State != state {
						t.Fatalf("changed other owner: %+v %v", row, err)
					}
					if data, err := os.ReadFile(owner.BlobPath); err != nil || string(data) != "shared alias payload" {
						t.Fatalf("deleted shared alias: %q %v", data, err)
					}
				})
			}
		}
	}
}

func TestLegacyDropProtectsInodeAliasAndRetainsOnReferenceError(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprintf("unreadable-reference=%t", broken), func(t *testing.T) {
			j, _, _ := openTest(t)
			ctx := t.Context()
			u := stage(t, j, "drop", []byte("keep payload"))
			if err := j.Commit(ctx, u); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(t.TempDir(), "alias")
			target := j.ObjectsDir()
			if broken {
				target = alias // ELOOP must retain data, not mean no reference.
			}
			if err := os.Symlink(target, alias); err != nil {
				t.Fatal(err)
			}
			owner := u
			owner.ID = NewID()
			owner.BlobPath = filepath.Join(alias, filepath.Base(u.BlobPath))
			if err := j.Commit(ctx, owner); err != nil {
				t.Fatal(err)
			}
			if err := j.DropPending(ctx, u.ID); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(u.BlobPath); err != nil || string(data) != "keep payload" {
				t.Fatalf("unsafe collection: %q %v", data, err)
			}
		})
	}
}
