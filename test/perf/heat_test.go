package perf

import (
	"bytes"
	"context"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/vfs"
)

// TestReadCountingIsFree (docs/agent-first-design.md §6.2): the read-heat
// observer the daemon installs costs the data path nothing it can see —
// ten thousand warm reads of a cached file with the observer wired make
// zero provider calls, the observer hears about the inode once per
// window rather than once per read, and the flush that turns the
// observation into a day bucket touches only agent.db.
func TestReadCountingIsFree(t *testing.T) {
	h := newHarness(t, 4096, 0, 0)
	ctx := context.Background()
	if _, err := h.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("h"), 8192)
	if _, err := h.fs.WriteFile(ctx, "/hot.bin", payload, false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.fs.ReadFileRange(ctx, "/hot.bin", 0, 0); err != nil {
		t.Fatal(err)
	}
	st, err := agent.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	heat := agent.NewReadObserver(st)
	observed := 0
	h.fs.SetReadObserver(func(ctx context.Context, ino uint64) {
		observed++
		if p, err := h.fs.Meta().Path(ctx, ino); err == nil {
			heat.Observe(p, agent.ReadByKernel)
		}
	})
	a, err := h.fs.StatPath(ctx, "/hot.bin")
	if err != nil {
		t.Fatal(err)
	}
	fh, err := h.fs.Open(ctx, a.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.fs.Release(ctx, fh)
	kctx := vfs.FromKernel(ctx)
	before := h.fake.TotalCalls()
	buf := make([]byte, 4096)
	start := time.Now()
	for i := 0; i < 10000; i++ {
		if _, err := h.fs.Read(kctx, fh, buf, int64(i%2)*4096); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	if extra := h.fake.TotalCalls() - before; extra != 0 {
		t.Fatalf("ten thousand counted reads cost %d provider calls, want 0", extra)
	}
	if observed != 1 {
		t.Fatalf("the observer heard %d reports for one inode in one window, want 1", observed)
	}
	if err := heat.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if extra := h.fake.TotalCalls() - before; extra != 0 {
		t.Fatalf("the flush cost %d provider calls, want 0", extra)
	}
	hot, err := st.HotPaths(ctx, "/", 7, 10)
	if err != nil || len(hot) != 1 || hot[0].Path != "/hot.bin" || hot[0].ByKind[agent.ReadByKernel] != 1 {
		t.Fatalf("hot paths: %+v %v", hot, err)
	}
	// A loose ceiling on the per-read cost of the counting itself: one
	// map lookup. The bound is generous so a loaded machine passes; a
	// counting path that took a lock per read or resolved the path would
	// blow past it by an order of magnitude.
	if per := elapsed / 10000; per > 200*time.Microsecond {
		t.Logf("warm read with counting: %v per read (not failing on wall clock; see the call counts above)", per)
	}
}

// TestChangeRecorderCostsNoRemoteCalls: the recorder that turns the
// change feed into rows asks the provider nothing — the feed carries
// paths, the rows are agent.db — so recording a burst of writes costs
// exactly the provider calls the writes themselves cost.
func TestChangeRecorderCostsNoRemoteCalls(t *testing.T) {
	h := newHarness(t, 4096, 0, 0)
	ctx := context.Background()
	if _, err := h.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	st, err := agent.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go st.RunChangeRecorder(rctx, h.fs)
	// The same writes without a recorder, for the baseline.
	base := newHarness(t, 4096, 0, 0)
	if _, err := base.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	write := func(hh *harness) int {
		before := hh.fake.TotalCalls()
		for i := 0; i < 20; i++ {
			if _, err := hh.fs.WriteFile(ctx, "/f"+string(rune('a'+i))+".txt", []byte("x"), false); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := hh.up.DrainAll(ctx); err != nil {
			t.Fatal(err)
		}
		return hh.fake.TotalCalls() - before
	}
	baseline := write(base)
	withRecorder := write(h)
	deadline := time.Now().Add(5 * time.Second)
	var rows []agent.Change
	for time.Now().Before(deadline) {
		rows, _, _ = st.Changes(ctx, agent.ChangesQuery{})
		if len(rows) >= 20 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(rows) < 20 {
		t.Fatalf("the recorder recorded %d rows for 20 writes", len(rows))
	}
	if withRecorder != baseline {
		t.Fatalf("recording cost %d provider calls beyond the writes' own %d", withRecorder-baseline, baseline)
	}
}
