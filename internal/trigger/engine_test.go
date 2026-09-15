package trigger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/testx"
	"cloudfs/internal/vfs"
)

// clock is a fake time source the engine and the tests share.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// harness runs an engine over a fake change stream and a fresh agent.db.
type harness struct {
	t       *testing.T
	st      *agent.Store
	q       *agent.Deliveries
	changes chan vfs.Change
	clock   *clock
	eng     *Engine
	rules   []config.Trigger
	agents  []config.Agent
}

func newHarness(t *testing.T, rules []config.Trigger, agents []config.Agent) *harness {
	t.Helper()
	st, err := agent.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &harness{t: t, st: st, q: st.Deliveries(), changes: make(chan vfs.Change), clock: &clock{t: time.Unix(1_800_000_000, 0)}, rules: rules, agents: agents}
	h.start()
	return h
}

// start builds and runs an engine on the harness's store; a test calls it
// again after Close to simulate a restart.
func (h *harness) start() {
	h.t.Helper()
	// The tick is the poll interval for due rows; the tests advance a fake
	// clock and wait, so a short tick keeps them quick. Under the race
	// detector every poll is a slow SQLite read, and two workers polling
	// every 5 ms would starve the storm test's inserts.
	tick := 5 * time.Millisecond
	if testx.RaceEnabled {
		tick = 50 * time.Millisecond
	}
	h.eng = New(Options{
		Rules: h.rules, Agents: h.agents, Store: h.st,
		Watch: func() (<-chan vfs.Change, func()) { return h.changes, func() {} },
		Now:   h.clock.Now, Tick: tick,
		Logger: slog.New(slog.NewTextHandler(testWriter{h.t}, nil)),
	})
	if err := h.eng.Run(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(h.eng.Close)
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// emit feeds one change and waits until the matcher has consumed it, so a
// following assertion on the table is not racing the matcher.
func (h *harness) emit(c vfs.Change) {
	h.t.Helper()
	before := h.eng.changesSeen()
	select {
	case h.changes <- c:
	case <-time.After(5 * time.Second):
		h.t.Fatal("the matcher stopped consuming changes")
	}
	h.waitFor("change consumed", func() bool { return h.eng.changesSeen() > before })
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) rows(q agent.DeliveryQuery) []agent.Delivery {
	h.t.Helper()
	rows, _, err := h.q.List(context.Background(), q)
	if err != nil {
		h.t.Fatal(err)
	}
	return rows
}

func (h *harness) get(id int64) agent.Delivery {
	h.t.Helper()
	d, err := h.q.Get(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) waitState(id int64, state string) agent.Delivery {
	h.t.Helper()
	var d agent.Delivery
	h.waitFor("delivery "+state, func() bool {
		d = h.get(id)
		return d.State == state
	})
	return d
}

func execRule(name string, paths []string, argv ...string) config.Trigger {
	return config.Trigger{
		Name: name, Paths: paths,
		Events: append([]string(nil), config.TriggerEvents...), Origins: append([]string(nil), config.TriggerOrigins...),
		Debounce: 2 * time.Second, OnRescan: config.OnRescanDeliver,
		Action: config.TriggerAction{Exec: &config.ExecAction{Command: argv, Timeout: 10 * time.Second}},
	}
}

func write(p string, origin vfs.Origin) vfs.Change {
	return vfs.Change{Paths: []string{p}, Kind: vfs.KindWrite, Origin: origin}
}

// TestDebounceCollapsesABurstIntoOneDelivery: twenty kernel writes to one
// path inside the window become one pending row; when the window passes
// the action runs once with that path.
func TestDebounceCollapsesABurstIntoOneDelivery(t *testing.T) {
	h := newHarness(t, []config.Trigger{execRule("inbox", []string{"/work/inbox/**"}, self(), "-echo-argv", "{path}", "{kind}")}, nil)
	for i := 0; i < 20; i++ {
		h.emit(write("/work/inbox/a.txt", vfs.OriginKernel))
		time.Sleep(2 * time.Millisecond) // the burst spans well under 50 ms
	}
	rows := h.rows(agent.DeliveryQuery{})
	if len(rows) != 1 {
		t.Fatalf("%d rows for one path, want 1: %+v", len(rows), rows)
	}
	d := rows[0]
	if d.State != agent.DeliveryPending || d.Kind != "write" || d.Origin != "kernel" || d.Path != "/work/inbox/a.txt" || d.Rule != "inbox" {
		t.Fatalf("row = %+v", d)
	}
	if !d.DueAt.Equal(h.clock.Now().Add(2 * time.Second)) {
		t.Fatalf("due_at = %v, want now+2s", d.DueAt)
	}
	// Nothing runs before the window closes.
	time.Sleep(30 * time.Millisecond)
	if got := h.get(d.ID); got.State != agent.DeliveryPending {
		t.Fatalf("ran before due: %+v", got)
	}
	h.clock.Advance(2 * time.Second)
	done := h.waitState(d.ID, agent.DeliveryDone)
	if done.Attempts != 1 || !strings.HasPrefix(done.Output, "/work/inbox/a.txt\nwrite\n") {
		t.Fatalf("done = %+v", done)
	}
	if len(h.rows(agent.DeliveryQuery{})) != 1 {
		t.Fatal("more rows appeared")
	}
}

// TestAPIOriginCanBeExcluded: a rule with origins [kernel, remote] ignores
// the agent's own writes and still fires on the kernel's.
func TestAPIOriginCanBeExcluded(t *testing.T) {
	r := execRule("inbox", []string{"/work/**"}, self(), "-echo-argv", "{path}")
	r.Origins = []string{"kernel", "remote"}
	h := newHarness(t, []config.Trigger{r}, nil)
	h.emit(write("/work/a.txt", vfs.OriginAPI))
	h.emit(vfs.Change{Paths: []string{"/work/b.txt"}, Kind: vfs.KindCreate, Origin: vfs.OriginAPI})
	if rows := h.rows(agent.DeliveryQuery{}); len(rows) != 0 {
		t.Fatalf("api-origin changes were queued: %+v", rows)
	}
	h.emit(write("/work/a.txt", vfs.OriginKernel))
	h.emit(vfs.Change{Paths: []string{"/work/c.txt"}, Kind: vfs.KindRemote, Origin: vfs.OriginRemote})
	rows := h.rows(agent.DeliveryQuery{})
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestEventsAndGlobsFilter: the kind list and the glob both gate a match;
// a rename delivers the names that match, and a change under a rule's
// glob root but outside its pattern does not fire.
func TestEventsAndGlobsFilter(t *testing.T) {
	r := execRule("md", []string{"/work/inbox/*.md"}, self(), "-echo-argv", "{path}")
	r.Events = []string{"write", "rename"}
	h := newHarness(t, []config.Trigger{r}, nil)
	h.emit(write("/work/inbox/a.txt", vfs.OriginKernel))                                                    // wrong extension
	h.emit(write("/work/inbox/deep/b.md", vfs.OriginKernel))                                                // "*" does not cross "/"
	h.emit(vfs.Change{Paths: []string{"/work/inbox/c.md"}, Kind: vfs.KindCreate, Origin: vfs.OriginKernel}) // kind excluded
	if rows := h.rows(agent.DeliveryQuery{}); len(rows) != 0 {
		t.Fatalf("unexpected rows %+v", rows)
	}
	h.emit(vfs.Change{Paths: []string{"/work/inbox/old.md", "/work/inbox/new.md"}, Subtree: true, Kind: vfs.KindRename, Origin: vfs.OriginKernel})
	rows := h.rows(agent.DeliveryQuery{})
	if len(rows) != 2 {
		t.Fatalf("rename rows = %+v", rows)
	}
	paths := rows[0].Path + " " + rows[1].Path
	if !strings.Contains(paths, "/work/inbox/old.md") || !strings.Contains(paths, "/work/inbox/new.md") || rows[0].Kind != "rename" {
		t.Fatalf("rename rows = %+v", rows)
	}
}

// TestSubtreeChangeReachesRulesRootedBelowIt: renaming or removing
// /work affects everything under it, so a rule on /work/inbox/** fires
// with the changed path itself; a non-subtree change to /work does not.
func TestSubtreeChangeReachesRulesRootedBelowIt(t *testing.T) {
	h := newHarness(t, []config.Trigger{execRule("inbox", []string{"/work/inbox/**"}, self(), "-echo-argv", "{path}")}, nil)
	h.emit(vfs.Change{Paths: []string{"/work"}, Kind: vfs.KindMkdir, Origin: vfs.OriginKernel})
	h.emit(vfs.Change{Paths: []string{"/other"}, Subtree: true, Kind: vfs.KindRemove, Origin: vfs.OriginKernel})
	if rows := h.rows(agent.DeliveryQuery{}); len(rows) != 0 {
		t.Fatalf("unexpected rows %+v", rows)
	}
	h.emit(vfs.Change{Paths: []string{"/work"}, Subtree: true, Kind: vfs.KindRemove, Origin: vfs.OriginKernel})
	rows := h.rows(agent.DeliveryQuery{})
	if len(rows) != 1 || rows[0].Path != "/work" || rows[0].Kind != "remove" {
		t.Fatalf("rows = %+v", rows)
	}
	// The root itself vanishing reaches every rule.
	h.emit(vfs.Change{Paths: []string{"/"}, Subtree: true, Kind: vfs.KindRemote, Origin: vfs.OriginRemote})
	if rows := h.rows(agent.DeliveryQuery{}); len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestOnRescanIgnoreSkipsTheRescanRow: a queue overflow reaches every rule
// as one path=” kind=rescan row, except a rule that opted out.
func TestOnRescanIgnoreSkipsTheRescanRow(t *testing.T) {
	deliver := execRule("deliver", []string{"/a/**"}, self(), "-echo-argv", "{kind}")
	ignore := execRule("ignore", []string{"/b/**"}, self(), "-echo-argv", "{kind}")
	ignore.OnRescan = config.OnRescanIgnore
	h := newHarness(t, []config.Trigger{deliver, ignore}, nil)
	h.emit(vfs.Change{Rescan: true, Kind: vfs.KindRescan, Origin: vfs.OriginRemote})
	h.emit(vfs.Change{Rescan: true, Kind: vfs.KindRescan, Origin: vfs.OriginRemote})
	rows := h.rows(agent.DeliveryQuery{})
	if len(rows) != 1 || rows[0].Rule != "deliver" || rows[0].Path != "" || rows[0].Kind != "rescan" || rows[0].Origin != "remote" {
		t.Fatalf("rows = %+v", rows)
	}
	h.clock.Advance(2 * time.Second)
	done := h.waitState(rows[0].ID, agent.DeliveryDone)
	if !strings.HasPrefix(done.Output, "rescan\n") {
		t.Fatalf("output = %q", done.Output)
	}
}

// TestStormStaysBounded: ten thousand changes over a hundred paths and
// two rules leave at most rules × paths rows, and an overflow that the
// VFS collapsed into a rescan is one row per rule however often it
// repeats.
func TestStormStaysBounded(t *testing.T) {
	rules := []config.Trigger{
		execRule("one", []string{"/storm/**"}, self(), "-echo-argv", "{path}"),
		execRule("two", []string{"/storm/*.txt"}, self(), "-echo-argv", "{path}"),
	}
	h := newHarness(t, rules, nil)
	kinds := []vfs.ChangeKind{vfs.KindWrite, vfs.KindCreate, vfs.KindRemove}
	before := h.eng.changesSeen()
	for i := 0; i < 10_000; i++ {
		p := fmt.Sprintf("/storm/f%02d.txt", i%100)
		h.changes <- vfs.Change{Paths: []string{p}, Kind: kinds[i%3], Origin: vfs.OriginKernel}
	}
	h.waitFor("storm consumed", func() bool { return h.eng.changesSeen() >= before+10_000 })
	pending, _, err := h.q.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pending > 2*100 || pending == 0 {
		t.Fatalf("pending = %d, want at most rules × paths = %d", pending, 2*100)
	}
	for i := 0; i < 3; i++ {
		h.emit(vfs.Change{Rescan: true, Kind: vfs.KindRescan, Origin: vfs.OriginRemote})
	}
	for _, rule := range []string{"one", "two"} {
		rows := h.rows(agent.DeliveryQuery{Rule: rule, State: agent.DeliveryPending})
		n := 0
		for _, d := range rows {
			if d.Kind == "rescan" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("rule %s has %d rescan rows, want 1", rule, n)
		}
	}
}

// TestRunningDeliveriesRestartAsPending: a delivery that was running when
// the engine stopped (kill -9, or a shutdown that cut the action short)
// is pending again on the next start and is claimed with attempts == 2.
func TestRunningDeliveriesRestartAsPending(t *testing.T) {
	h := newHarness(t, []config.Trigger{execRule("slow", []string{"/**"}, self(), "-sleep", "60s")}, nil)
	id, err := h.eng.Test(context.Background(), "slow", "/work/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	d := h.waitState(id, agent.DeliveryRunning)
	if d.Attempts != 1 || d.Kind != "test" {
		t.Fatalf("running = %+v", d)
	}
	// Give the child a moment to actually start, then stop the engine
	// without letting the delivery finish. The row must stay running: a
	// clean stop and a crash look the same to the next process.
	h.waitFor("child started", func() bool { return h.get(id).State == agent.DeliveryRunning })
	h.eng.Close()
	if got := h.get(id); got.State != agent.DeliveryRunning {
		t.Fatalf("after close: %+v", got)
	}
	h.start()
	d = h.waitFor2(id, func(d agent.Delivery) bool { return d.State == agent.DeliveryRunning && d.Attempts == 2 })
	if d.Attempts != 2 {
		t.Fatalf("after restart: %+v", d)
	}
}

func (h *harness) waitFor2(id int64, cond func(agent.Delivery) bool) agent.Delivery {
	h.t.Helper()
	var d agent.Delivery
	h.waitFor("delivery condition", func() bool {
		d = h.get(id)
		return cond(d)
	})
	return d
}

// TestBackoffThenDead: a failing action is retried with a doubling wait
// from 1 s, and after eight attempts the row is dead. The clock is fake,
// so the test asserts the due_at spacing rather than waiting for it.
func TestBackoffThenDead(t *testing.T) {
	h := newHarness(t, []config.Trigger{execRule("bad", []string{"/**"}, self(), "-exit", "1")}, nil)
	id, err := h.eng.Test(context.Background(), "bad", "/x")
	if err != nil {
		t.Fatal(err)
	}
	wantWait := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 64 * time.Second}
	for attempt := 1; attempt <= 7; attempt++ {
		d := h.waitFor2(id, func(d agent.Delivery) bool { return d.State == agent.DeliveryPending && d.Attempts == attempt })
		if wait := d.DueAt.Sub(h.clock.Now()); wait != wantWait[attempt-1] {
			t.Fatalf("attempt %d: due in %v, want %v", attempt, wait, wantWait[attempt-1])
		}
		if !strings.Contains(d.LastError, "exit status 1") || !strings.Contains(d.Output, "failing on purpose") {
			t.Fatalf("attempt %d: %+v", attempt, d)
		}
		h.clock.Set(d.DueAt)
	}
	d := h.waitState(id, agent.DeliveryDead)
	if d.Attempts != 8 || !strings.Contains(d.LastError, "exit status 1") {
		t.Fatalf("dead = %+v", d)
	}
	if n, err := h.eng.DeadCount(context.Background()); err != nil || n != 1 {
		t.Fatalf("dead count = %d %v", n, err)
	}
	// The cap holds for a rule reopened many times.
	if got := backoff(20); got != 5*time.Minute {
		t.Fatalf("backoff(20) = %v", got)
	}
}

// TestRetryReopensADeadDelivery: Retry puts a dead row back in the queue
// and the worker picks it up at once, attempts continuing from where they
// stopped.
func TestRetryReopensADeadDelivery(t *testing.T) {
	h := newHarness(t, []config.Trigger{execRule("bad", []string{"/**"}, self(), "-exit", "1")}, nil)
	id, err := h.eng.Test(context.Background(), "bad", "/x")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 7; attempt++ {
		d := h.waitFor2(id, func(d agent.Delivery) bool { return d.State == agent.DeliveryPending && d.Attempts == attempt })
		h.clock.Set(d.DueAt)
	}
	h.waitState(id, agent.DeliveryDead)
	if err := h.eng.Retry(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	d := h.waitFor2(id, func(d agent.Delivery) bool { return d.State == agent.DeliveryDead && d.Attempts == 9 })
	if d.Attempts != 9 {
		t.Fatalf("after retry: %+v", d)
	}
	if err := h.eng.Retry(context.Background(), 12345); !errors.Is(err, agent.ErrDeliveryNotFound) {
		t.Fatalf("retry of a missing row: %v", err)
	}
}

// TestTestRunsThroughTheQueue: Test enqueues due now and the worker runs
// it at once, so the console gets a row id it can follow.
func TestTestRunsThroughTheQueue(t *testing.T) {
	h := newHarness(t, []config.Trigger{execRule("inbox", []string{"/work/**"}, self(), "-echo-argv", "{path}", "{kind}")}, nil)
	if _, err := h.eng.Test(context.Background(), "nope", "/work/a.txt"); !errors.Is(err, ErrUnknownRule) {
		t.Fatalf("unknown rule: %v", err)
	}
	id, err := h.eng.Test(context.Background(), "inbox", "/work/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	d := h.waitState(id, agent.DeliveryDone)
	if d.Kind != "test" || d.Origin != "console" || !strings.HasPrefix(d.Output, "/work/a.txt\ntest\n") {
		t.Fatalf("done = %+v", d)
	}
}

// TestInvokeAppendsPathsAsSeparateArgv: an agent run substitutes {prompt}
// as one element and appends every path as its own element; nothing is
// joined into a command line.
func TestInvokeAppendsPathsAsSeparateArgv(t *testing.T) {
	ag := config.Agent{Name: "claude", Exec: config.ExecAction{Command: []string{self(), "-echo-argv", "-p", "{prompt}", "--"}, Timeout: 10 * time.Second}}
	h := newHarness(t, nil, []config.Agent{ag})
	if _, err := h.eng.Invoke(context.Background(), "nope", nil, "x"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("unknown agent: %v", err)
	}
	id, err := h.eng.Invoke(context.Background(), "claude", []string{"/work/a b.txt", "/work/c.txt; rm -rf /"}, "Summarize these; echo pwned")
	if err != nil {
		t.Fatal(err)
	}
	d := h.waitState(id, agent.DeliveryDone)
	want := "-p\nSummarize these; echo pwned\n--\n/work/a b.txt\n/work/c.txt; rm -rf /\n"
	if d.Output != want {
		t.Fatalf("output = %q, want %q", d.Output, want)
	}
	if d.Rule != "agent:claude" || d.Path != "/work/a b.txt" || d.Kind != "invoke" || d.Origin != "console" {
		t.Fatalf("row = %+v", d)
	}
}

// TestInvokeFailureIsDeadAtOnce: a person is waiting on an agent run, so a
// failure is parked for them immediately instead of retried for an hour.
func TestInvokeFailureIsDeadAtOnce(t *testing.T) {
	ag := config.Agent{Name: "bad", Exec: config.ExecAction{Command: []string{self(), "-exit", "2"}, Timeout: 10 * time.Second}}
	h := newHarness(t, nil, []config.Agent{ag})
	id, err := h.eng.Invoke(context.Background(), "bad", []string{"/x"}, "go")
	if err != nil {
		t.Fatal(err)
	}
	d := h.waitState(id, agent.DeliveryDead)
	if d.Attempts != 1 || !strings.Contains(d.LastError, "exit status 2") {
		t.Fatalf("dead = %+v", d)
	}
}

// TestChangesStopWhenTheStreamCloses: a closed change stream (FS.Close)
// ends the matcher without a panic and Close still returns.
func TestChangesStopWhenTheStreamCloses(t *testing.T) {
	h := newHarness(t, []config.Trigger{execRule("r", []string{"/**"}, self(), "-echo-argv")}, nil)
	close(h.changes)
	h.eng.Close()
}

// TestChangeDuringARunIsDeliveredAgain: once a row is claimed the pending
// cache forgets it, so a change to the same path while the action runs
// makes a new pending row and the action runs a second time after the
// first. The debounce merges bursts, never a change made during a run.
func TestChangeDuringARunIsDeliveredAgain(t *testing.T) {
	r := execRule("slow", []string{"/work/**"}, self(), "-sleep", "150ms")
	r.Debounce = 0
	h := newHarness(t, []config.Trigger{r}, nil)
	first, err := h.eng.Test(context.Background(), "slow", "/work/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	h.waitState(first, agent.DeliveryRunning)
	h.emit(write("/work/a.txt", vfs.OriginKernel))
	h.emit(write("/work/a.txt", vfs.OriginKernel)) // merges into the new pending row, not the running one
	rows := h.rows(agent.DeliveryQuery{Rule: "slow"})
	if len(rows) != 2 || rows[0].State != agent.DeliveryPending || rows[1].ID != first {
		t.Fatalf("rows = %+v", rows)
	}
	h.waitState(first, agent.DeliveryDone)
	h.waitState(rows[0].ID, agent.DeliveryDone)
	if len(h.rows(agent.DeliveryQuery{Rule: "slow"})) != 2 {
		t.Fatal("row count changed")
	}
}
