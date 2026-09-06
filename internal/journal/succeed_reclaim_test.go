package journal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSucceedKeepsContentAnotherRowStillNeeds. Two writes of the same bytes
// deduplicate onto one object, so releasing the queue's link when one of them
// finishes must not take the data the other one still has to send. This is the
// same shape as the bug that made Drop delete a blob a second row was queued
// on, and it has to stay fixed on this path too.
func TestSucceedKeepsContentAnotherRowStillNeeds(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	content := []byte("identical bytes")
	first, second := stage(t, j, "first", content), stage(t, j, "second", content)
	if first.BlobPath != second.BlobPath {
		t.Fatalf("content addressing did not deduplicate: %q vs %q", first.BlobPath, second.BlobPath)
	}
	for _, u := range []Upload{first, second} {
		if err := j.Commit(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Succeed(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second.BlobPath); err != nil {
		t.Fatalf("finishing one upload removed the content another one is still queued on: %v", err)
	}
	// And once the second finishes too, nothing names it any more.
	if err := j.Succeed(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second.BlobPath); !os.IsNotExist(err) {
		t.Fatalf("the last reference finished and the object survived: %v", err)
	}
}

// TestSucceedDoesNotTouchADeadLettersPayload: a dead letter keeps its data
// precisely so `cloudfs uploads retry` has something to send.
func TestSucceedDoesNotTouchADeadLettersPayload(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	content := []byte("identical bytes")
	failed, sent := stage(t, j, "failed", content), stage(t, j, "sent", content)
	for _, u := range []Upload{failed, sent} {
		if err := j.Commit(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Fail(ctx, failed.ID, os.ErrPermission); err != nil {
		t.Fatal(err)
	}
	if err := j.Succeed(ctx, sent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(failed.BlobPath); err != nil {
		t.Fatalf("a dead letter lost the payload retry would resend: %v", err)
	}
	dead, err := j.Dead(ctx)
	if err != nil || len(dead) != 1 || dead[0].BlobPath == "" {
		t.Fatalf("dead letter lost its blob reference: %+v %v", dead, err)
	}
}

// TestCompletedHistoryIsBoundedButOutlivesAWaiter. Completed rows are kept so
// a strict-mode writer can watch its upload finish and so recent history is
// readable, but they must not accumulate for the life of the installation.
// Both bounds have to be past before a row goes: an age floor means a waiter
// would have to be stalled for minutes before its row could vanish under it.
func TestCompletedHistoryIsBoundedButOutlivesAWaiter(t *testing.T) {
	j, c, _ := openTest(t)
	ctx := context.Background()
	finish := func(name string) string {
		u := stage(t, j, name, []byte(name))
		if err := j.Commit(ctx, u); err != nil {
			t.Fatal(err)
		}
		if err := j.Succeed(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	oldest := finish("oldest")
	// Everything after it completes later, so "the newest N" is unambiguous.
	c.advance(time.Second)
	// Still inside the age floor: nothing is reclaimed however many follow.
	for i := 0; i < doneHistory+20; i++ {
		finish("recent")
	}
	if _, err := j.Get(ctx, oldest); err != nil {
		t.Fatalf("a row completed minutes ago was reclaimed while a waiter could still be looking at it: %v", err)
	}

	// Past the floor, and past the history bound, it goes.
	c.advance(2 * doneRetention)
	finish("trigger")
	if _, err := j.Get(ctx, oldest); err != ErrNotFound {
		t.Fatalf("completed rows accumulate without bound: %v", err)
	}
	all, err := j.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) > doneHistory+1 {
		t.Fatalf("%d completed rows retained, want at most %d", len(all), doneHistory+1)
	}
	// A row completed just now is never a candidate.
	newest := finish("newest")
	if _, err := j.Get(ctx, newest); err != nil {
		t.Fatalf("the row just completed is gone: %v", err)
	}
}

// TestOrphanObjectsSeesWhatNoRowNames. The disk leak this file is about was
// invisible: the bytes are outside cache.max_size and outside the queue's
// reported total, so no report would ever have mentioned them. Doctor asks
// this question now, and it has to answer honestly in both directions —
// silent when the queue is consistent, and specific when it is not.
func TestOrphanObjectsSeesWhatNoRowNames(t *testing.T) {
	j, _, dir := openTest(t)
	ctx := context.Background()
	queued := stage(t, j, "queued", []byte("still to send"))
	if err := j.Commit(ctx, queued); err != nil {
		t.Fatal(err)
	}
	if n, bytes, err := j.OrphanObjects(ctx); err != nil || n != 0 || bytes != 0 {
		t.Fatalf("a queued upload's own payload was reported as an orphan: %d objects, %d bytes, %v", n, bytes, err)
	}
	if err := j.Succeed(ctx, queued.ID); err != nil {
		t.Fatal(err)
	}
	if n, _, err := j.OrphanObjects(ctx); err != nil || n != 0 {
		t.Fatalf("after a clean finish the store is not empty: %d objects, %v", n, err)
	}

	// Something no row names — what a crash between the row delete and the
	// unlink leaves behind.
	stray := filepath.Join(dir, "objects", "sha1-0000000000000000000000000000000000000000")
	if err := os.WriteFile(stray, []byte("leaked payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, bytes, err := j.OrphanObjects(ctx)
	if err != nil || n != 1 || bytes != int64(len("leaked payload")) {
		t.Fatalf("orphaned payload not reported: %d objects, %d bytes, %v", n, bytes, err)
	}
}

// TestReclaimingACompletedUploadTakesItsSideRowsWithIt. An upload that was
// dead-lettered and retried, or cancelled and resumed, leaves rows in the side
// tables keyed by its ID. Reclaiming only the upload row would move the
// unbounded growth into those instead of stopping it.
func TestReclaimingACompletedUploadTakesItsSideRowsWithIt(t *testing.T) {
	j, c, _ := openTest(t)
	ctx := context.Background()

	// An upload that was cancelled and explicitly resumed, then landed: the
	// resume keeps the old session in private history, and the cancellation
	// revision stays behind it.
	u := stage(t, j, "eventually", []byte("eventually sent"))
	if err := j.Commit(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RequestCancel(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	prepared, err := j.PrepareUploadResume(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"upload_cancellation", "upload_resume_history"} {
		var n int
		if err := j.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE upload_id = ?`, u.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatalf("%s holds nothing for a resumed upload; this test is not exercising what it claims", table)
		}
	}
	if err := j.Succeed(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	c.advance(2 * doneRetention)
	// Push it past the history bound as well.
	for i := 0; i <= doneHistory; i++ {
		other := stage(t, j, fmt.Sprintf("filler-%d", i), []byte(fmt.Sprintf("filler %d", i)))
		if err := j.Commit(ctx, other); err != nil {
			t.Fatal(err)
		}
		if err := j.Succeed(ctx, other.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := j.Get(ctx, u.ID); err != ErrNotFound {
		t.Fatalf("the completed upload was not reclaimed: %v", err)
	}
	for _, table := range []string{"upload_parts", "dead_letter", "upload_cancellation", "upload_resume_history"} {
		var n int
		if err := j.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE upload_id = ?`, u.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s still holds %d rows for a reclaimed upload", table, n)
		}
	}
}
