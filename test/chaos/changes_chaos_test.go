package chaos

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/vfs"
)

// TestChangesSurviveKill9 covers "kill -9 with the change recorder
// running" (docs/agent-first-design.md §6.1): every batch the recorder
// committed is in agent.db after the crash — the record is SQLite, so a
// commit is a commit; what the recorder was still holding is gone, at
// most one batch, and the next recorder says so with a rescan row of
// origin restart (reliable false) before it records anything new, which
// is what pull_events and the hooks show as "may be incomplete".
func TestChangesSurviveKill9(t *testing.T) {
	r := newRig(t, rigOpt{})
	ctx := context.Background()
	agentDir := filepath.Join(r.dir, "agent")
	st, _, _ := openAgent(t, r, agentDir)
	rctx, stopRecorder := context.WithCancel(ctx)
	recorderDone := make(chan struct{})
	go func() { st.RunChangeRecorder(rctx, r.fs); close(recorderDone) }()
	for _, name := range []string{"/one.txt", "/two.txt", "/three.txt"} {
		if _, err := r.fs.WriteFile(ctx, name, []byte("recorded"), false); err != nil {
			t.Fatal(err)
		}
	}
	// The recorder flushes on its timer; wait for the rows to land.
	deadline := time.Now().Add(5 * time.Second)
	var before []agent.Change
	for time.Now().Before(deadline) {
		before, _, _ = st.Changes(ctx, agent.ChangesQuery{})
		if len(before) >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(before) < 3 {
		t.Fatalf("the recorder never flushed: %+v", before)
	}
	committed := len(before)
	// A write the recorder has not flushed when the process dies: the
	// crash is the store closing with the recorder still holding it (the
	// goroutine is stopped after, as the kernel would stop it).
	if _, err := r.fs.WriteFile(ctx, "/unflushed.txt", []byte("lost"), false); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	stopRecorder()
	<-recorderDone

	// The restart.
	st2, _, _ := openAgent(t, r, agentDir)
	defer st2.Close()
	after, _, err := st2.Changes(ctx, agent.ChangesQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) < committed {
		t.Fatalf("committed rows were lost: %d before, %d after", committed, len(after))
	}
	for i := range before {
		if after[i].ID != before[i].ID || after[i].Path != before[i].Path {
			t.Fatalf("row %d changed across the crash: %+v -> %+v", i, before[i], after[i])
		}
	}
	if lost := len(after) - committed; lost > 128 {
		t.Fatalf("more than one batch appeared from nowhere: %d", lost)
	}
	rctx2, stop2 := context.WithCancel(ctx)
	defer stop2()
	go st2.RunChangeRecorder(rctx2, r.fs)
	deadline = time.Now().Add(5 * time.Second)
	var marker *agent.Change
	for time.Now().Before(deadline) && marker == nil {
		rows, _, _ := st2.Changes(ctx, agent.ChangesQuery{After: after[len(after)-1].ID})
		for i := range rows {
			if rows[i].Kind == vfs.KindRescan.String() && rows[i].Origin == "restart" {
				marker = &rows[i]
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if marker == nil || marker.Reliable {
		t.Fatalf("the restarted recorder left no restart marker: %+v", marker)
	}
	// And it records again from there.
	if _, err := r.fs.WriteFile(ctx, "/after.txt", []byte("recorded again"), false); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, _, _ := st2.Changes(ctx, agent.ChangesQuery{After: marker.ID, Prefix: "/after.txt"})
		if n := len(rows); n > 0 && rows[n-1].Reliable {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a write after the restart was not recorded as reliable")
}

// TestHeatFlushSurvivesKill9 covers "kill -9 with read heat pending": what
// the observer flushed is a day bucket in agent.db and survives; what it
// was still debouncing in memory is gone and nothing pretends otherwise.
// The buckets are upserts, so the next flush after the restart adds to
// them rather than starting over.
func TestHeatFlushSurvivesKill9(t *testing.T) {
	r := newRig(t, rigOpt{})
	ctx := context.Background()
	agentDir := filepath.Join(r.dir, "agent")
	st, _, _ := openAgent(t, r, agentDir)
	heat := agent.NewReadObserver(st)
	for i := 0; i < 5; i++ {
		heat.Observe("/hot.txt", agent.ReadByAgent)
	}
	heat.Observe("/warm.txt", agent.ReadByKernel)
	if err := heat.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// Reads observed after the flush die with the process.
	heat.Observe("/lost.txt", agent.ReadByAgent)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, _, _ := openAgent(t, r, agentDir)
	defer st2.Close()
	hot, err := st2.HotPaths(ctx, "/", 7, 10)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]agent.HotPath{}
	for _, h := range hot {
		byPath[h.Path] = h
	}
	if h, ok := byPath["/hot.txt"]; !ok || h.Reads < 1 || h.ByKind[agent.ReadByAgent] < 1 {
		t.Fatalf("the flushed bucket was lost: %+v", hot)
	}
	if _, ok := byPath["/warm.txt"]; !ok {
		t.Fatalf("the flushed kernel bucket was lost: %+v", hot)
	}
	if _, ok := byPath["/lost.txt"]; ok {
		t.Fatalf("a read never flushed appeared after the crash: %+v", hot)
	}
	heat2 := agent.NewReadObserver(st2)
	heat2.Observe("/hot.txt", agent.ReadByAgent)
	if err := heat2.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	hot, _ = st2.HotPaths(ctx, "/", 7, 10)
	for _, h := range hot {
		if h.Path == "/hot.txt" && h.Reads < byPath["/hot.txt"].Reads+1 {
			t.Fatalf("the bucket did not add up after the restart: %+v", h)
		}
	}
}
