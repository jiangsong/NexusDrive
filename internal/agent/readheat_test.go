package agent

import (
	"context"
	"testing"
	"time"
)

func TestReadHeatBumpsAggregateByPathAndKind(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	ctx := context.Background()
	samples := []ReadSample{
		{Path: "/work/a.md", ActorKind: ReadByAgent, TS: base},
		{Path: "/work/a.md", ActorKind: ReadByAgent, TS: base.Add(time.Hour), Count: 2},
		{Path: "/work/a.md", ActorKind: ReadByKernel, TS: base.Add(-24 * time.Hour)},
		{Path: "/work/b.md", ActorKind: ReadByAgent, TS: base},
		{Path: "/other/c.md", ActorKind: ReadByAgent, TS: base.Add(-10 * 24 * time.Hour)},
	}
	if err := s.BumpReadHeat(ctx, samples); err != nil {
		t.Fatal(err)
	}
	hot, err := s.HotPaths(ctx, "/", 7, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hot) != 2 || hot[0].Path != "/work/a.md" || hot[0].Reads != 4 || hot[0].ByKind[ReadByAgent] != 3 || hot[0].ByKind[ReadByKernel] != 1 || !hot[0].LastRead.Equal(base.Add(time.Hour)) {
		t.Fatalf("%+v", hot)
	}
	if hot[1].Path != "/work/b.md" || hot[1].Reads != 1 {
		t.Fatalf("%+v", hot)
	}
	// A wider window brings the old read in; a prefix narrows it.
	if hot, _ := s.HotPaths(ctx, "/", 30, 10); len(hot) != 3 {
		t.Fatalf("30 days: %+v", hot)
	}
	if hot, _ := s.HotPaths(ctx, "/other", 30, 10); len(hot) != 1 || hot[0].Path != "/other/c.md" {
		t.Fatalf("prefix: %+v", hot)
	}
	if hot, _ := s.HotPaths(ctx, "/work", 7, 1); len(hot) != 1 {
		t.Fatalf("limit: %+v", hot)
	}
	if n, err := s.PruneReadHeat(ctx, 5); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
}

func TestReadObserverDebouncesAndFlushes(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	o := NewReadObserver(s)
	for i := 0; i < 100; i++ {
		o.Observe("/work/big.bin", ReadByKernel) // one sequential read, many blocks
	}
	o.Observe("/work/big.bin", ReadByAgent)
	o.Observe("/work/note.md", ReadByAgent)
	o.Observe("", ReadByAgent)
	if o.Pending() != 3 {
		t.Fatalf("pending = %d", o.Pending())
	}
	if err := o.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	hot, _ := s.HotPaths(context.Background(), "/", 7, 10)
	if len(hot) != 2 || hot[0].Path != "/work/big.bin" || hot[0].Reads != 2 || hot[0].ByKind[ReadByKernel] != 1 {
		t.Fatalf("%+v", hot)
	}
	// Inside the window the same reader is not counted again; past it, it is.
	o.Observe("/work/big.bin", ReadByKernel)
	if o.Pending() != 0 {
		t.Fatal("a read inside the debounce window was counted")
	}
	now = now.Add(11 * time.Minute)
	o.Observe("/work/big.bin", ReadByKernel)
	if o.Pending() != 1 {
		t.Fatal("a read past the debounce window was not counted")
	}
	if err := o.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	hot, _ = s.HotPaths(context.Background(), "/work/big.bin", 7, 10)
	if hot[0].Reads != 3 {
		t.Fatalf("%+v", hot)
	}
	// Run flushes on cancel.
	o.Observe("/work/late.md", ReadByAgent)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); o.Run(ctx, time.Hour) }()
	cancel()
	<-done
	if hot, _ := s.HotPaths(context.Background(), "/work/late.md", 7, 10); len(hot) != 1 {
		t.Fatal("Run did not flush on cancel")
	}
}
