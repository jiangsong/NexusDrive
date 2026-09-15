package chaos

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/testx"
	"cloudfs/internal/trigger"
	"cloudfs/internal/vfs"
)

// The trigger engine (docs/agent-roadmap.md §5.3, TODO.md T-41) joins the
// reliability matrix here: a delivery that was running when the process
// died must run again on the next start, and a change storm that overflows
// the VFS change queue must reach every rule as exactly one rescan, not as
// nothing and not as a flood.

// TestMain doubles as the child the exec rules run, the way the trigger
// package's own tests do: the test binary re-executes itself with a mode
// in argv[1]. The exec runner scrubs the environment, so the mode cannot
// travel in a variable.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-echo-argv":
			for _, a := range os.Args[2:] {
				fmt.Println(a)
			}
			os.Exit(0)
		case "-block-once":
			// First run: leave a marker and hang until killed. Second run,
			// which finds the marker: say so and exit cleanly.
			marker := os.Args[2]
			if _, err := os.Stat(marker); err == nil {
				fmt.Println("second run")
				for _, a := range os.Args[3:] {
					fmt.Println(a)
				}
				os.Exit(0)
			}
			if err := os.WriteFile(marker, []byte("started\n"), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			fmt.Println("first run, blocking")
			time.Sleep(time.Hour)
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

// self is the program the exec rules run: this test binary.
func self(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeClock is the engine's time source in these tests, so a debounce
// window closes when the test says so rather than after a real wait.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// engineTick is the workers' poll interval. Under the race detector every
// poll is a slow SQLite read, so it is relaxed there.
func engineTick() time.Duration {
	if testx.RaceEnabled {
		return 50 * time.Millisecond
	}
	return 5 * time.Millisecond
}

func execRule(t *testing.T, name string, paths []string, argv ...string) config.Trigger {
	t.Helper()
	return config.Trigger{
		Name: name, Paths: paths,
		Events: append([]string(nil), config.TriggerEvents...), Origins: append([]string(nil), config.TriggerOrigins...),
		Debounce: 2 * time.Second, OnRescan: config.OnRescanDeliver,
		Action: config.TriggerAction{Exec: &config.ExecAction{Command: append([]string{self(t)}, argv...), Timeout: 10 * time.Minute}},
	}
}

func openAgentStore(t *testing.T) *agent.Store {
	t.Helper()
	st, err := agent.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func waitDelivery(t *testing.T, q *agent.Deliveries, id int64, what string, cond func(agent.Delivery) bool) agent.Delivery {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		d, err := q.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if cond(d) {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %+v", what, d)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDeliveryRunningAtCrashIsRedelivered covers "投递 running 时 kill -9".
// The first engine claims a row whose command blocks forever, and is
// stopped while the command runs — the row is never marked done or failed,
// exactly what a kill -9 leaves behind. A second engine over the same
// agent.db re-queues it on start, runs it again, and the row ends done with
// attempts=2; no second row for the same (rule, path) appears.
func TestDeliveryRunningAtCrashIsRedelivered(t *testing.T) {
	st := openAgentStore(t)
	q := st.Deliveries()
	marker := filepath.Join(t.TempDir(), "first-run")
	rule := execRule(t, "crash", []string{"/**"}, "-block-once", marker, "{path}")
	rule.Debounce = 0
	newEngine := func() *trigger.Engine {
		eng := trigger.New(trigger.Options{
			Rules: []config.Trigger{rule}, Store: st, Tick: engineTick(),
			Logger: slog.New(slog.NewTextHandler(chaosLog{t}, nil)),
		})
		if err := eng.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(eng.Close)
		return eng
	}

	first := newEngine()
	id, err := first.Test(context.Background(), "crash", "/work/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	running := waitDelivery(t, q, id, "the first claim", func(d agent.Delivery) bool { return d.State == agent.DeliveryRunning })
	if running.Attempts != 1 {
		t.Fatalf("first claim: %+v", running)
	}
	// The child has started once the marker exists; only then is the
	// crash a crash mid-action rather than before it.
	deadline := time.Now().Add(15 * time.Second)
	for !fileExists(marker) {
		if time.Now().After(deadline) {
			t.Fatal("the exec child never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Kill -9: the engine goes away with the row still running. Close
	// kills the child and records nothing, which is the same table a
	// process that simply vanished leaves behind.
	first.Close()
	if d, err := q.Get(context.Background(), id); err != nil || d.State != agent.DeliveryRunning || d.Attempts != 1 {
		t.Fatalf("after the crash the row should still be running with one attempt: %+v %v", d, err)
	}

	second := newEngine()
	defer second.Close()
	done := waitDelivery(t, q, id, "the redelivery", func(d agent.Delivery) bool { return d.State == agent.DeliveryDone })
	if done.Attempts != 2 {
		t.Fatalf("redelivered row should count two attempts: %+v", done)
	}
	if !strings.HasPrefix(done.Output, "second run\n/work/a.txt\n") {
		t.Fatalf("the second run did not receive the path: %q", done.Output)
	}
	rows, _, err := q.List(context.Background(), agent.DeliveryQuery{Rule: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("the restart must reuse the row, not add one: %+v", rows)
	}
}

// chaosLog routes the engine's log lines into the test log.
type chaosLog struct{ t *testing.T }

func (w chaosLog) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// TestStormUnderOverflowDeliversOneRescan covers "1 万事件风暴 / 队列溢出".
// A real vfs.FS feeds the engine through its 64-slot change queue while the
// consumer is held back, so the queue overflows and the VFS collapses the
// backlog into rescan hints. Once released, the engine must leave exactly
// one pending rescan row for a rule that delivers rescans and none for a
// rule with on_rescan: ignore, keep the per-path rows bounded by the number
// of paths, and run the rescan with an empty path when its window closes.
func TestStormUnderOverflowDeliversOneRescan(t *testing.T) {
	r := newRig(t, rigOpt{})
	st := openAgentStore(t)
	q := st.Deliveries()
	ctx := context.Background()

	deliver := execRule(t, "deliver", []string{"/storm/**"}, "-echo-argv", "{kind}", "{path}")
	ignore := execRule(t, "ignore", []string{"/storm/**"}, "-echo-argv", "{kind}", "{path}")
	ignore.OnRescan = config.OnRescanIgnore
	sentinel := execRule(t, "sentinel", []string{"/sentinel/**"}, "-echo-argv", "{kind}")
	sentinel.OnRescan = config.OnRescanIgnore

	// The consumer of the FS change stream is held until the storm is
	// over: the gate is the slow worker.
	gate := make(chan struct{})
	subscribed := make(chan struct{})
	stopForward := make(chan struct{})
	var rescans, forwarded atomic.Int64
	var queue <-chan vfs.Change
	watch := func() (<-chan vfs.Change, func()) {
		ch, stop := r.fs.WatchChanges()
		queue = ch
		close(subscribed)
		out := make(chan vfs.Change)
		go func() {
			defer close(out)
			select {
			case <-gate:
			case <-stopForward:
				return
			}
			for c := range ch {
				if c.Rescan {
					rescans.Add(1)
				}
				select {
				case out <- c:
					forwarded.Add(1)
				case <-stopForward:
					return
				}
			}
		}()
		return out, stop
	}
	t.Cleanup(func() { close(stopForward) })

	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	eng := trigger.New(trigger.Options{
		FS: r.fs, Rules: []config.Trigger{deliver, ignore, sentinel}, Store: st,
		Watch: watch, Now: clock.Now, Tick: engineTick(),
		Logger: slog.New(slog.NewTextHandler(chaosLog{t}, nil)),
	})
	if err := eng.Run(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Close)
	select {
	case <-subscribed:
	case <-time.After(5 * time.Second):
		t.Fatal("the engine never subscribed to the change stream")
	}

	root, err := r.fs.StatPath(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.fs.Mkdir(ctx, root.Ino, "storm"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fs.Mkdir(ctx, root.Ino, "sentinel"); err != nil {
		t.Fatal(err)
	}
	// Each WriteFile announces a create and a write: four hundred changes
	// against a queue of sixty-four.
	const paths = 200
	for i := 0; i < paths; i++ {
		if _, err := r.fs.WriteFile(ctx, fmt.Sprintf("/storm/f%03d.txt", i), []byte("x"), false); err != nil {
			t.Fatal(err)
		}
	}
	if rescans.Load() != 0 {
		t.Fatal("nothing may be forwarded while the gate is shut")
	}
	close(gate)
	// The sentinel is written once the queue has room again, so it cannot
	// itself be collapsed into a rescan; the matcher is serial, so its row
	// proves everything before it has been handled.
	deadline := time.Now().Add(20 * time.Second)
	for len(queue) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the engine did not drain the queue (%d changes forwarded)", forwarded.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := r.fs.WriteFile(ctx, "/sentinel/done.txt", []byte("x"), false); err != nil {
		t.Fatal(err)
	}
	for {
		rows, _, err := q.List(ctx, agent.DeliveryQuery{Rule: "sentinel"})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].Path == "/sentinel/done.txt" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sentinel change never reached the engine (%d forwarded): %+v", forwarded.Load(), rows)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rescans.Load() == 0 {
		t.Fatalf("the change queue never overflowed (%d changes forwarded), so nothing was tested", forwarded.Load())
	}

	rescanRows := func(rule string) []agent.Delivery {
		t.Helper()
		rows, _, err := q.List(ctx, agent.DeliveryQuery{Rule: rule})
		if err != nil {
			t.Fatal(err)
		}
		var out []agent.Delivery
		seen := make(map[string]bool, len(rows))
		for _, d := range rows {
			if d.Kind == "rescan" {
				out = append(out, d)
			}
			if seen[d.Path] {
				t.Fatalf("rule %s has two rows for %q; the storm was not merged", rule, d.Path)
			}
			seen[d.Path] = true
		}
		// One row per path, the directory itself, and at most one rescan.
		if len(rows) > paths+2 {
			t.Fatalf("rule %s has %d rows for %d paths; the storm was not bounded", rule, len(rows), paths)
		}
		return out
	}
	got := rescanRows("deliver")
	if len(got) != 1 || got[0].Path != "" || got[0].State != agent.DeliveryPending || got[0].Origin != "remote" {
		t.Fatalf("deliver: %d rescan rows after %d overflows, want exactly one pending path='' row: %+v", len(got), rescans.Load(), got)
	}
	if ignored := rescanRows("ignore"); len(ignored) != 0 {
		t.Fatalf("on_rescan: ignore still got %d rescan rows: %+v", len(ignored), ignored)
	}

	// The window closes and the one rescan runs with an empty path.
	clock.Advance(2 * time.Second)
	done := waitDelivery(t, q, got[0].ID, "the rescan delivery", func(d agent.Delivery) bool { return d.State == agent.DeliveryDone })
	if done.Attempts != 1 || !strings.HasPrefix(done.Output, "rescan\n\n") {
		t.Fatalf("rescan delivery = %+v", done)
	}
	if again := rescanRows("deliver"); len(again) != 1 {
		t.Fatalf("running the rescan must not spawn another: %+v", again)
	}
}
