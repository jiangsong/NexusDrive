package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func newDeliveryStore(t *testing.T, now *time.Time) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.now = func() time.Time { return *now }
	return st
}

// TestEnqueueMergesIntoThePendingRow: the partial unique index on
// (rule, path) WHERE state='pending' is the debounce. A second event for
// the same path while the first is still waiting returns the first row's
// id and reports the merge; nothing is inserted.
func TestEnqueueMergesIntoThePendingRow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := newDeliveryStore(t, &now)
	ctx := context.Background()
	q := st.Deliveries()
	ev, stop := st.Watch()
	defer stop()

	id, merged, err := q.Enqueue(ctx, "inbox", "/work/a.txt", "write", "kernel", now.Add(2*time.Second))
	if err != nil || merged || id == 0 {
		t.Fatalf("first enqueue: id=%d merged=%v err=%v", id, merged, err)
	}
	again, merged, err := q.Enqueue(ctx, "inbox", "/work/a.txt", "create", "api", now.Add(3*time.Second))
	if err != nil || !merged || again != id {
		t.Fatalf("second enqueue: id=%d merged=%v err=%v (want id %d merged)", again, merged, err, id)
	}
	other, merged, err := q.Enqueue(ctx, "inbox", "/work/b.txt", "write", "kernel", now.Add(2*time.Second))
	if err != nil || merged || other == id {
		t.Fatalf("different path: id=%d merged=%v err=%v", other, merged, err)
	}
	d, err := q.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != "write" || d.Origin != "kernel" || !d.DueAt.Equal(now.Add(2*time.Second)) || d.State != "pending" || d.Attempts != 0 {
		t.Fatalf("merged row kept the first event's values? %+v", d)
	}
	if !d.FirstSeen.Equal(now) {
		t.Fatalf("first_seen = %v, want %v", d.FirstSeen, now)
	}
	// Only the two inserts were published; a merge is not an event.
	got := 0
	for i := 0; i < 2; i++ {
		select {
		case e := <-ev:
			if e.Kind != "trigger" || e.Delivery == nil || e.Delivery.State != "pending" {
				t.Fatalf("event %+v", e)
			}
			got++
		case <-time.After(time.Second):
			t.Fatalf("only %d events arrived", got)
		}
	}
	select {
	case e := <-ev:
		t.Fatalf("unexpected extra event %+v", e)
	default:
	}
}

// TestClaimTakesTheOldestDueRowOnly: a row is claimable once its due_at
// has passed; claiming moves it to running and counts the attempt; the
// next claim on the same rule skips it and takes the next due row.
func TestClaimTakesTheOldestDueRowOnly(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := newDeliveryStore(t, &now)
	ctx := context.Background()
	q := st.Deliveries()
	late, _, err := q.Enqueue(ctx, "r", "/late", "write", "kernel", now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	early, _, err := q.Enqueue(ctx, "r", "/early", "write", "kernel", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := q.Claim(ctx, "r", now); err != nil || ok {
		t.Fatalf("nothing is due yet: ok=%v err=%v", ok, err)
	}
	if _, ok, err := q.Claim(ctx, "other", now.Add(time.Hour)); err != nil || ok {
		t.Fatalf("another rule's rows are not mine: ok=%v err=%v", ok, err)
	}
	d, ok, err := q.Claim(ctx, "r", now.Add(time.Second))
	if err != nil || !ok || d.ID != early || d.State != "running" || d.Attempts != 1 {
		t.Fatalf("claim = %+v ok=%v err=%v", d, ok, err)
	}
	if _, ok, _ := q.Claim(ctx, "r", now.Add(time.Second)); ok {
		t.Fatal("the running row was claimed twice")
	}
	d, ok, err = q.Claim(ctx, "r", now.Add(time.Minute))
	if err != nil || !ok || d.ID != late {
		t.Fatalf("second claim = %+v ok=%v err=%v", d, ok, err)
	}
	pending, dead, err := q.Counts(ctx)
	if err != nil || pending != 0 || dead != 0 {
		t.Fatalf("counts = %d %d %v", pending, dead, err)
	}
}

// TestFailReschedulesAndDeadEnds: Fail puts the row back to pending at the
// given due time and keeps the attempt count; Dead parks it for a human;
// Retry reopens a dead row with its attempts intact. Every transition is
// published.
func TestFailReschedulesAndDeadEnds(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := newDeliveryStore(t, &now)
	ctx := context.Background()
	q := st.Deliveries()
	id, _, err := q.Enqueue(ctx, "r", "/a", "write", "kernel", now)
	if err != nil {
		t.Fatal(err)
	}
	ev, stop := st.Watch()
	defer stop()
	states := func(n int) []string {
		var out []string
		for i := 0; i < n; i++ {
			select {
			case e := <-ev:
				out = append(out, e.Delivery.State)
			case <-time.After(time.Second):
				t.Fatalf("after %v events nothing more arrived", out)
			}
		}
		return out
	}

	if _, ok, _ := q.Claim(ctx, "r", now); !ok {
		t.Fatal("claim")
	}
	if err := q.Fail(ctx, id, "exit status 1", "stderr text", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	d, _ := q.Get(ctx, id)
	if d.State != "pending" || d.Attempts != 1 || d.LastError != "exit status 1" || d.Output != "stderr text" || !d.DueAt.Equal(now.Add(time.Second)) {
		t.Fatalf("after fail: %+v", d)
	}
	if _, ok, _ := q.Claim(ctx, "r", now); ok {
		t.Fatal("claimed before the backoff expired")
	}
	if d, ok, _ := q.Claim(ctx, "r", now.Add(time.Second)); !ok || d.Attempts != 2 {
		t.Fatalf("claim after backoff: %+v ok=%v", d, ok)
	}
	if err := q.Dead(ctx, id, "gave up", "out"); err != nil {
		t.Fatal(err)
	}
	d, _ = q.Get(ctx, id)
	if d.State != "dead" || d.Attempts != 2 || d.LastError != "gave up" || !d.DoneAt.Equal(now) {
		t.Fatalf("after dead: %+v", d)
	}
	if _, dead, _ := q.Counts(ctx); dead != 1 {
		t.Fatalf("dead count = %d", dead)
	}
	if err := q.Retry(ctx, id); err != nil {
		t.Fatal(err)
	}
	d, _ = q.Get(ctx, id)
	if d.State != "pending" || d.Attempts != 2 || !d.DueAt.Equal(now) || !d.DoneAt.IsZero() {
		t.Fatalf("after retry: %+v", d)
	}
	if err := q.Retry(ctx, id); !errors.Is(err, ErrDeliveryNotDead) {
		t.Fatalf("retrying a pending row: %v", err)
	}
	if err := q.Retry(ctx, 999); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("retrying a missing row: %v", err)
	}
	if _, ok, _ := q.Claim(ctx, "r", now); !ok {
		t.Fatal("claim after retry")
	}
	if err := q.Done(ctx, id, "ok"); err != nil {
		t.Fatal(err)
	}
	d, _ = q.Get(ctx, id)
	if d.State != "done" || d.Output != "ok" || d.LastError != "" || !d.DoneAt.Equal(now) {
		t.Fatalf("after done: %+v", d)
	}
	want := []string{"running", "pending", "running", "dead", "pending", "running", "done"}
	if got := states(len(want)); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("published states %v, want %v", got, want)
	}
}

// TestResetRunningRepends: what was running when the process died is
// pending again on the next start, attempts kept. A pending duplicate that
// arrived meanwhile folds into the older row so the unique index holds.
func TestResetRunningRepends(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := newDeliveryStore(t, &now)
	ctx := context.Background()
	q := st.Deliveries()
	id, _, _ := q.Enqueue(ctx, "r", "/a", "write", "kernel", now)
	if _, ok, _ := q.Claim(ctx, "r", now); !ok {
		t.Fatal("claim")
	}
	dup, merged, err := q.Enqueue(ctx, "r", "/a", "write", "kernel", now.Add(time.Second))
	if err != nil || merged || dup == id {
		t.Fatalf("a running row must not block a new pending one: %d %v %v", dup, merged, err)
	}
	n, err := q.ResetRunning(ctx)
	if err != nil || n != 1 {
		t.Fatalf("reset = %d %v", n, err)
	}
	d, err := q.Get(ctx, id)
	if err != nil || d.State != "pending" || d.Attempts != 1 {
		t.Fatalf("reset row: %+v %v", d, err)
	}
	if _, err := q.Get(ctx, dup); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("duplicate should be folded away, got %v", err)
	}
	d, ok, _ := q.Claim(ctx, "r", now)
	if !ok || d.ID != id || d.Attempts != 2 {
		t.Fatalf("claim after reset: %+v ok=%v", d, ok)
	}
	// Fail while a newer pending row exists folds the same way.
	dup2, _, _ := q.Enqueue(ctx, "r", "/a", "write", "kernel", now)
	if err := q.Fail(ctx, id, "boom", "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Get(ctx, dup2); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("duplicate after fail should be folded away, got %v", err)
	}
	if d, _ := q.Get(ctx, id); d.State != "pending" || d.Attempts != 2 {
		t.Fatalf("failed row: %+v", d)
	}
}

// TestListFiltersAndPages: newest first, filtered by rule and state,
// cursor-paged like the audit trail; the JSON view derives truncated from
// the marker the runner leaves.
func TestListFiltersAndPages(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := newDeliveryStore(t, &now)
	ctx := context.Background()
	q := st.Deliveries()
	for i := 0; i < 5; i++ {
		if _, _, err := q.Enqueue(ctx, "a", "/p"+string(rune('0'+i)), "write", "kernel", now); err != nil {
			t.Fatal(err)
		}
	}
	bid, _, _ := q.Enqueue(ctx, "b", "/x", "mkdir", "api", now)
	if _, ok, _ := q.Claim(ctx, "b", now); !ok {
		t.Fatal("claim")
	}
	if err := q.Done(ctx, bid, "out"+OutputTruncatedMarker); err != nil {
		t.Fatal(err)
	}

	rows, next, err := q.List(ctx, DeliveryQuery{Limit: 4})
	if err != nil || len(rows) != 4 || next == "" || rows[0].ID != bid {
		t.Fatalf("page 1: %d rows next=%q err=%v", len(rows), next, err)
	}
	if !rows[0].Truncated || rows[1].Truncated {
		t.Fatalf("truncated flags: %+v %+v", rows[0], rows[1])
	}
	rows, next, err = q.List(ctx, DeliveryQuery{Limit: 4, Cursor: next})
	if err != nil || len(rows) != 2 || next != "" {
		t.Fatalf("page 2: %d rows next=%q err=%v", len(rows), next, err)
	}
	rows, _, err = q.List(ctx, DeliveryQuery{Rule: "a", State: "pending"})
	if err != nil || len(rows) != 5 {
		t.Fatalf("filter: %d rows err=%v", len(rows), err)
	}
	rows, _, err = q.List(ctx, DeliveryQuery{State: "done"})
	if err != nil || len(rows) != 1 || rows[0].Rule != "b" {
		t.Fatalf("state filter: %+v err=%v", rows, err)
	}
	if _, _, err := q.List(ctx, DeliveryQuery{Cursor: "junk"}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("bad cursor: %v", err)
	}
	if _, _, err := q.List(ctx, DeliveryQuery{State: "bogus"}); err == nil {
		t.Fatal("an unknown state should be rejected, not silently match nothing")
	}
}
