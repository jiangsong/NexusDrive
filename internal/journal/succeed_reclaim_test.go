package journal

import (
	"context"
	"os"
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
