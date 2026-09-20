package upload

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/journal"
	"cloudfs/internal/provider"
)

// progressHarness drives a tracker without a journal: the readings are the
// thing under test, and a real queue would only make them harder to state.
type progressHarness struct {
	t       *testing.T
	now     time.Time
	stats   journal.Stats
	files   int64
	bytes   int64
	inFlt   int64
	queries int
	tr      *progressTracker
}

func newProgressHarness(t *testing.T) *progressHarness {
	t.Helper()
	h := &progressHarness{t: t, now: time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)}
	h.tr = newProgressTracker(
		func(context.Context) (journal.Stats, error) { h.queries++; return h.stats, nil },
		func() (int64, int64) { return h.files, h.bytes },
		func() int64 { return h.inFlt },
		func() time.Time { return h.now },
	)
	return h
}

// queue sets what the journal would report and takes one reading. busy is
// what the supervisor's own listing of remotes with queued work found.
func (h *progressHarness) queue(pending, uploading int, bytes int64) Progress {
	h.t.Helper()
	h.stats.Pending, h.stats.Uploading, h.stats.Bytes = pending, uploading, bytes
	return h.tr.sample(context.Background(), pending+uploading > 0)
}

// finished records rows landing: the uploader's lifetime counters move.
func (h *progressHarness) finished(files, bytes int64) {
	h.files += files
	h.bytes += bytes
}

func (h *progressHarness) tick(d time.Duration) { h.now = h.now.Add(d) }

// drain takes the two empty readings a batch needs to close. The second one
// is deliberate: see the two-reading rule in observe.
func (h *progressHarness) drain() Progress {
	h.t.Helper()
	h.queue(0, 0, 0)
	h.tick(time.Second)
	return h.queue(0, 0, 0)
}

// TestBatchFollowsTheQueueFromBusyToDrained: the bar starts when the queue
// stops being empty, grows as more is committed into the burst, counts what
// finished since the start, and stays on screen — complete — once the queue
// is empty, until the next burst begins.
func TestBatchFollowsTheQueueFromBusyToDrained(t *testing.T) {
	h := newProgressHarness(t)
	// Idle, with ten files finished earlier in this process: nothing to show.
	h.finished(10, 1000)
	if p := h.queue(0, 0, 0); p.Active || p.FilesTotal != 0 {
		t.Fatalf("idle: %+v", p)
	}
	// Three files queued: the batch begins at zero done, not at ten.
	h.tick(time.Second)
	p := h.queue(3, 0, 300)
	if !p.Active || p.Seq != 1 || p.FilesTotal != 3 || p.FilesDone != 0 || p.BytesTotal != 300 || p.BytesDone != 0 || p.StartedAt.IsZero() {
		t.Fatalf("start: %+v", p)
	}
	// One landed, two more were committed meanwhile: the total grows, the
	// done part is what landed since the start.
	h.tick(time.Second)
	h.finished(1, 100)
	if p = h.queue(4, 0, 450); p.FilesTotal != 5 || p.FilesDone != 1 || p.BytesTotal != 550 || p.BytesDone != 100 {
		t.Fatalf("mid: %+v", p)
	}
	// Everything landed: complete, no longer active, and the figures stay.
	h.tick(7 * time.Second)
	h.finished(4, 450)
	p = h.drain()
	if p.Active || p.FilesTotal != 5 || p.FilesDone != 5 || p.BytesDone != 550 || p.FinishedAt.IsZero() {
		t.Fatalf("end: %+v", p)
	}
	if p.Rate != 0 || p.ETA != 0 {
		t.Fatalf("a drained batch still reports a rate: %+v", p)
	}
	// And the snapshot a status page reads is that same finished batch.
	if got := h.tr.snapshot(); got.FilesDone != 5 || got.Active {
		t.Fatalf("snapshot after the batch: %+v", got)
	}
}

// TestABatchWaitsOneReadingBeforeDeclaringItselfDone: a worker marks its row
// done in the journal and only then adds to the lifetime counters. A reading
// taken between the two sees an empty queue and a count one short, and would
// end a copy of 3434 files on the words "3433 of 3433". The next reading has
// the real figure.
func TestABatchWaitsOneReadingBeforeDeclaringItselfDone(t *testing.T) {
	h := newProgressHarness(t)
	h.queue(2, 0, 200)
	h.tick(time.Second)
	// The queue is empty but only one row has been counted yet.
	h.finished(1, 100)
	if p := h.queue(0, 0, 0); !p.Active {
		t.Fatalf("the batch closed on the first empty reading, with %+v", p)
	}
	// The second row's counter catches up before the next reading.
	h.tick(time.Second)
	h.finished(1, 100)
	p := h.queue(0, 0, 0)
	if p.Active {
		t.Fatalf("the batch did not close on the second empty reading: %+v", p)
	}
	if p.FilesDone != 2 || p.FilesTotal != 2 {
		t.Fatalf("the finished batch is short: %+v", p)
	}
}

// TestASecondBatchGetsANewSeqAndRestartsAtZero: two copies in a row are two
// bars. A reader that kept the figures of the first must be able to tell that
// they do not continue into the second, and Seq is how.
func TestASecondBatchGetsANewSeqAndRestartsAtZero(t *testing.T) {
	h := newProgressHarness(t)
	first := h.queue(2, 0, 200)
	h.tick(time.Second)
	h.finished(2, 200)
	done := h.drain()
	if first.Seq != 1 || done.Seq != 1 || done.FilesDone != 2 {
		t.Fatalf("first batch: %+v then %+v", first, done)
	}
	h.tick(time.Minute)
	second := h.queue(3, 0, 30)
	if second.Seq != 2 {
		t.Fatalf("the second batch reused seq %d", second.Seq)
	}
	if !second.Active || second.FilesTotal != 3 || second.FilesDone != 0 || second.BytesDone != 0 {
		t.Fatalf("second batch did not restart at zero: %+v", second)
	}
	if !second.FinishedAt.IsZero() {
		t.Fatalf("second batch kept the first one's finish time: %+v", second)
	}
}

// TestAQueueFoundNonEmptyAtStartupIsResumed: a daemon restarted in the middle
// of a copy cannot know how much of it already went up — done rows are
// trimmed, so journal.Stats.Done would make the bar run backwards. It says
// the batch is resumed instead of quietly counting from the restart.
func TestAQueueFoundNonEmptyAtStartupIsResumed(t *testing.T) {
	h := newProgressHarness(t)
	if p := h.queue(400, 2, 4000); !p.Active || !p.Resumed {
		t.Fatalf("a queue found busy at startup was not marked resumed: %+v", p)
	}
	// The batch after it is a fresh one and is not resumed.
	h.tick(time.Second)
	h.finished(402, 4000)
	h.drain()
	h.tick(time.Second)
	if p := h.queue(1, 0, 10); p.Resumed {
		t.Fatalf("a batch that began while we watched was marked resumed: %+v", p)
	}
}

// TestAQueueIdleAtStartupIsNotResumed guards the other direction: the first
// reading being taken is not by itself a resume.
func TestAQueueIdleAtStartupIsNotResumed(t *testing.T) {
	h := newProgressHarness(t)
	h.queue(0, 0, 0)
	h.tick(time.Second)
	if p := h.queue(2, 0, 20); p.Resumed {
		t.Fatalf("an idle start made the next batch look resumed: %+v", p)
	}
}

// TestAnIdleQueueIsNotQueried: the queue statistics include a recursive walk
// over the queued directories. A daemon with nothing to upload polls this
// once a second forever, so it must not pay for it.
func TestAnIdleQueueIsNotQueried(t *testing.T) {
	h := newProgressHarness(t)
	for i := 0; i < 10; i++ {
		h.tick(time.Second)
		h.queue(0, 0, 0)
	}
	if h.queries != 0 {
		t.Fatalf("an idle queue was scanned %d times", h.queries)
	}
	h.tick(time.Second)
	h.queue(1, 0, 10)
	if h.queries != 1 {
		t.Fatalf("a busy queue was scanned %d times, want 1", h.queries)
	}
	// Draining it costs the one reading that closes the batch, and then
	// nothing again.
	h.tick(time.Second)
	h.finished(1, 10)
	h.drain()
	before := h.queries
	for i := 0; i < 5; i++ {
		h.tick(time.Second)
		h.queue(0, 0, 0)
	}
	if h.queries != before {
		t.Fatalf("an idle queue was scanned again after draining: %d then %d", before, h.queries)
	}
}

// TestRateAndETAAreComputedInGo: the console worked these out in JavaScript,
// so a terminal and an agent had no rate at all. Against a fake clock a
// steady 100 bytes a second has to read as one.
func TestRateAndETAAreComputedInGo(t *testing.T) {
	h := newProgressHarness(t)
	h.queue(10, 0, 1000)
	for i := 0; i < 60; i++ {
		h.tick(time.Second)
		h.finished(0, 100)
		h.queue(10, 0, 1000)
	}
	p := h.queue(10, 0, 1000)
	// A ten-second exponential average of a constant sample converges on it;
	// after a minute it is within a percent.
	if p.Rate < 99 || p.Rate > 101 {
		t.Fatalf("rate = %.2f B/s, want about 100", p.Rate)
	}
	// 1000 bytes still queued at 100 B/s is ten seconds.
	if p.ETA < 9*time.Second || p.ETA > 11*time.Second {
		t.Fatalf("eta = %s, want about 10s", p.ETA)
	}
}

// TestBlockedRowsAndInFlightBytesSurface: the journal counts rows that cannot
// run — they wait on a directory creation that is dead or gone — and nothing
// outside the flush path ever showed the number. A queue that looks busy and
// is going nowhere is exactly what an operator needs told.
func TestBlockedRowsAndInFlightBytesSurface(t *testing.T) {
	h := newProgressHarness(t)
	h.stats.Blocked = 7
	h.inFlt = 4096
	p := h.queue(9, 1, 900)
	if p.Blocked != 7 {
		t.Fatalf("blocked = %d, want 7", p.Blocked)
	}
	if p.InFlightBytes != 4096 {
		t.Fatalf("in-flight bytes = %d, want 4096", p.InFlightBytes)
	}
}

// TestLifetimeTotalsNeverDecreaseAcrossBatches: the Prometheus completion
// counters are fed from these. Feeding them the batch delta instead would
// make the counter fall to zero at every batch boundary, which a counter may
// not do.
func TestLifetimeTotalsNeverDecreaseAcrossBatches(t *testing.T) {
	h := newProgressHarness(t)
	var lastFiles, lastBytes int64
	check := func(p Progress, where string) {
		if p.FilesDoneTotal < lastFiles || p.BytesDoneTotal < lastBytes {
			t.Fatalf("%s: lifetime totals went backwards: %d/%d after %d/%d",
				where, p.FilesDoneTotal, p.BytesDoneTotal, lastFiles, lastBytes)
		}
		lastFiles, lastBytes = p.FilesDoneTotal, p.BytesDoneTotal
	}
	for batch := 0; batch < 3; batch++ {
		check(h.queue(2, 0, 200), "open")
		h.tick(time.Second)
		h.finished(2, 200)
		check(h.drain(), "drain")
		h.tick(time.Minute)
	}
	if lastFiles != 6 || lastBytes != 600 {
		t.Fatalf("after three batches the lifetime totals are %d/%d, want 6/600", lastFiles, lastBytes)
	}
}

// TestTheSupervisorSamplesTheQueueWhileItRuns wires the tracker to the real
// thing: an uploader that has never run reports an empty batch, and one that
// drains a queue reports a batch that opened and closed — without anyone
// calling sample by hand.
func TestTheSupervisorSamplesTheQueueWhileItRuns(t *testing.T) {
	f := newFixture(t)
	f.queue(t, "a.txt", []byte("one"), "")
	f.queue(t, "b.txt", []byte("two"), "")
	if p := f.up.Progress(); p.Seq != 0 || p.Active {
		t.Fatalf("an uploader that never ran reports %+v", p)
	}

	// A second uploader over the same journal, ticking fast enough for a
	// test and with no hooks to race on.
	u, err := New(Options{
		Journal:      f.j,
		Providers:    func(string) (provider.Provider, bool) { return f.fake, true },
		PollInterval: 5 * time.Millisecond,
		Now:          f.clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u.Start(ctx)
	defer u.Stop()

	// Both rows go up and the batch closes. How they fall into batches is
	// the workers' timing, not this test's business — what it pins is that
	// the supervisor samples at all, that everything is accounted for, and
	// that the bar ends closed rather than stuck open.
	deadline := time.Now().Add(10 * time.Second)
	for {
		p := u.Progress()
		if p.FilesDoneTotal == 2 && !p.Active {
			if p.Seq == 0 || p.FinishedAt.IsZero() {
				t.Fatalf("drained batch: %+v", p)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the queue never drained into a finished batch: %+v", p)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestABatchAnnouncesItselfInTheLog: internal/upload had no log statement at
// all, so nothing ever said a copy had finished uploading. The two lines are
// the completion signal a person tailing the daemon is waiting for.
func TestABatchAnnouncesItselfInTheLog(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(previous)

	h := newProgressHarness(t)
	h.queue(2, 0, 200)
	h.tick(time.Second)
	h.finished(2, 200)
	h.drain()
	log := buf.String()
	if !strings.Contains(log, "batch started") {
		t.Errorf("nothing announced the batch opening: %s", log)
	}
	if !strings.Contains(log, "batch drained") {
		t.Errorf("nothing announced the batch draining: %s", log)
	}
	if strings.Count(log, "batch started") != 1 || strings.Count(log, "batch drained") != 1 {
		t.Errorf("one batch produced more than one line each: %s", log)
	}
}
