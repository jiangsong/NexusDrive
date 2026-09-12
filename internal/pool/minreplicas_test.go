package pool

import (
	"context"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/test/fakeprovider"
)

// TestBelowMinReplicasIsAnAlertNotAWriteBarrier: relaxed mode is the
// default, and min_replicas is what it claims to be there — a threshold
// that raises repair priority and shows up in status, never something a
// write waits for or fails on.
func TestBelowMinReplicasIsAnAlertNotAWriteBarrier(t *testing.T) {
	ctx := context.Background()
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 3, MinReplicas: 2}, a, b, c)
	docs, _ := p.Mkdir(ctx, rootID, "docs")

	// One replica exists the moment the upload lands: close() returned
	// without waiting for the other two.
	upload(t, ctx, p, docs.ID, "r.txt", []byte("one copy so far"))
	if got := liveCount(t, p, "/docs/r.txt"); got != 1 {
		t.Fatalf("live right after the write = %d, want 1", got)
	}
	av, err := p.Availability(ctx, "/docs/r.txt")
	if err != nil {
		t.Fatal(err)
	}
	if av.State != AvailDegraded || !av.BelowMin {
		t.Fatalf("availability = %+v, want degraded and below min", av)
	}
	r, err := p.StatusReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.BelowMin != 1 || r.UnderReplicated != 1 {
		t.Fatalf("report: below min %d, under %d, want 1 and 1", r.BelowMin, r.UnderReplicated)
	}

	// The scan puts a file below min_replicas ahead of one that is merely
	// short of its target.
	upload(t, ctx, p, docs.ID, "s.txt", []byte("second file"))
	if _, err := p.ScanOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var priority int
	var reason string
	if err := p.db.QueryRowContext(ctx, `SELECT priority, reason FROM repair_queue WHERE path = ?`, "/docs/r.txt").Scan(&priority, &reason); err != nil {
		t.Fatal(err)
	}
	if priority < 2 || reason != "below min_replicas" {
		t.Fatalf("queued as priority %d, %q", priority, reason)
	}

	// Once repair has caught up, nothing is below the threshold.
	for i := 0; i < 4; i++ {
		if _, err := p.RepairOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if r, err = p.StatusReport(ctx); err != nil {
		t.Fatal(err)
	}
	if r.BelowMin != 0 {
		t.Fatalf("below min after repair = %d", r.BelowMin)
	}
	if av, err = p.Availability(ctx, "/docs/r.txt"); err != nil || av.State != AvailFull || av.BelowMin {
		t.Fatalf("availability after repair = %+v (%v)", av, err)
	}
}

// TestStrictWriteModeFillsMinReplicasBeforeCloseReturns: strict mode buys
// the caller the other copies before close() returns, without ever
// turning a member outage into a failed write.
func TestStrictWriteModeFillsMinReplicasBeforeCloseReturns(t *testing.T) {
	ctx := context.Background()
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 3, MinReplicas: 2,
		WriteMode: config.WriteModeStrict, MinReplicasTimeout: time.Minute}, a, b, c)
	docs, _ := p.Mkdir(ctx, rootID, "docs")
	upload(t, ctx, p, docs.ID, "r.txt", []byte("wait for me"))
	// At least min_replicas. One repair pass fills the whole target, so
	// more than the threshold is normal and not worth slowing down for.
	if got := liveCount(t, p, "/docs/r.txt"); got < 2 {
		t.Fatalf("live when close() returned = %d, want at least min_replicas 2", got)
	}
	if r, err := p.StatusReport(ctx); err != nil || r.BelowMin != 0 {
		t.Fatalf("below min = %d (%v)", r.BelowMin, err)
	}
}

// TestStrictWriteModeStillSucceedsWhenNoMemberCanTakeACopy: the promise
// is about when close() returns, not about whether the data is safe. A
// pool with one member left must not fail writes.
func TestStrictWriteModeStillSucceedsWhenNoMemberCanTakeACopy(t *testing.T) {
	ctx := context.Background()
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	b.SetFaults(func(f *fakeprovider.Faults) { f.Down = true })
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 2,
		WriteMode: config.WriteModeStrict, MinReplicasTimeout: 200 * time.Millisecond, ProbeInterval: time.Hour}, a, b)
	docs, _ := p.Mkdir(ctx, rootID, "docs")

	done := make(chan struct{})
	go func() {
		upload(t, ctx, p, docs.ID, "r.txt", []byte("one member only"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a strict-mode write hung instead of returning after its deadline")
	}
	if got := liveCount(t, p, "/docs/r.txt"); got != 1 {
		t.Fatalf("live = %d", got)
	}
	// The file stays queued: the data is durable on one member and in the
	// hold, and repair keeps trying.
	var queued int
	if err := p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM repair_queue WHERE path = ?`, "/docs/r.txt").Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatal("a write that could not reach min_replicas must stay in the repair queue")
	}
}
