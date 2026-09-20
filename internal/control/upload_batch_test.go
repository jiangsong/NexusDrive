package control

import (
	"testing"
	"time"
)

// TestUploadBatchFollowsOneBurstOfWork: the bar starts when the queue
// stops being empty, grows as more is committed into the burst, counts
// what finished since the start, and stays on screen — complete — once
// the queue is empty, until the next burst begins.
func TestUploadBatchFollowsOneBurstOfWork(t *testing.T) {
	var tr uploadBatchTracker
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	// Idle, with a history of 10 files behind it: nothing to show.
	if b := tr.observe(t0, 0, 0, 10, 1000); b.Active || b.FilesTotal != 0 {
		t.Fatalf("idle: %+v", b)
	}
	// Three files queued: the batch begins at zero done.
	b := tr.observe(t0.Add(time.Second), 3, 300, 10, 1000)
	if !b.Active || b.FilesTotal != 3 || b.FilesDone != 0 || b.BytesTotal != 300 || b.BytesDone != 0 || b.StartedAt == "" {
		t.Fatalf("start: %+v", b)
	}
	// One landed, two more were committed meanwhile: the total grows, the
	// done part is what landed since the start.
	b = tr.observe(t0.Add(2*time.Second), 4, 450, 11, 1100)
	if b.FilesTotal != 5 || b.FilesDone != 1 || b.BytesTotal != 550 || b.BytesDone != 100 {
		t.Fatalf("mid: %+v", b)
	}
	// Everything landed: complete, no longer active, and the figures stay.
	b = tr.observe(t0.Add(9*time.Second), 0, 0, 15, 1550)
	if b.Active || b.FilesTotal != 5 || b.FilesDone != 5 || b.BytesDone != 550 || b.FinishedAt == "" {
		t.Fatalf("end: %+v", b)
	}
	if again := tr.observe(t0.Add(30*time.Second), 0, 0, 15, 1550); again != b {
		t.Fatalf("idle after the batch shows %+v, want the finished batch %+v", again, b)
	}
	// A new burst starts a new bar from zero.
	b = tr.observe(t0.Add(time.Minute), 2, 20, 15, 1550)
	if !b.Active || b.FilesTotal != 2 || b.FilesDone != 0 || b.BytesDone != 0 {
		t.Fatalf("second batch: %+v", b)
	}
}
