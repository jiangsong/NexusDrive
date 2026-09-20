package agent

import (
	"context"
	"errors"
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

// TestObserveInoResolvesPathsOnlyAtTheFlush: the form the VFS read
// observer uses names no file — resolving an inode is a recursive walk
// of the metadata tree, and a read must not pay for one. The walk
// happens once per observed inode, at the flush, which is also what
// decides how a rename or a delete in between is counted.
func TestObserveInoResolvesPathsOnlyAtTheFlush(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	o := NewReadObserver(s)
	// No resolver: the observer cannot name what it hears, so it keeps
	// nothing rather than growing a map nothing will ever drain.
	o.ObserveIno(7, ReadByKernel)
	if o.Pending() != 0 {
		t.Fatalf("pending without a resolver = %d", o.Pending())
	}
	tree := map[uint64]string{7: "/work/big.bin", 8: "/work/note.md", 9: "/work/doomed.md"}
	resolved := 0
	o.SetPathResolver(func(_ context.Context, ino uint64) (string, error) {
		resolved++
		p, ok := tree[ino]
		if !ok {
			return "", errors.New("no such inode")
		}
		return p, nil
	})
	for i := 0; i < 100; i++ {
		o.ObserveIno(7, ReadByKernel) // one sequential read, many blocks
	}
	o.ObserveIno(7, ReadByAgent)
	o.ObserveIno(8, ReadByAgent)
	o.ObserveIno(9, ReadByAgent)
	o.ObserveIno(0, ReadByAgent)
	o.ObserveIno(8, "")
	if resolved != 0 {
		t.Fatalf("%d paths resolved while observing, want 0", resolved)
	}
	if o.Pending() != 4 {
		t.Fatalf("pending = %d, want 4", o.Pending())
	}
	// Between the reads and the flush one file is renamed and one is
	// deleted. The renamed file is counted under the name it has now;
	// the deleted one is counted nowhere.
	tree[8] = "/work/renamed.md"
	delete(tree, 9)
	if err := o.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resolved != 4 {
		t.Fatalf("the flush resolved %d inodes, want one per observed (ino, kind) pair: 4", resolved)
	}
	hot, err := s.HotPaths(context.Background(), "/", 7, 10)
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]int64{}
	for _, h := range hot {
		reads[h.Path] = h.Reads
	}
	if len(hot) != 2 || reads["/work/big.bin"] != 2 || reads["/work/renamed.md"] != 1 {
		t.Fatalf("%+v", hot)
	}
	if _, ok := reads["/work/note.md"]; ok {
		t.Fatal("the read was counted under the name the file no longer has")
	}
	if _, ok := reads["/work/doomed.md"]; ok {
		t.Fatal("a deleted file was counted")
	}
	// The debounce is unchanged, only keyed by inode now.
	o.ObserveIno(7, ReadByKernel)
	if o.Pending() != 0 {
		t.Fatal("a read inside the debounce window was counted")
	}
	now = now.Add(11 * time.Minute)
	o.ObserveIno(7, ReadByKernel)
	if o.Pending() != 1 {
		t.Fatal("a read past the debounce window was not counted")
	}
}

// TestPruneReadHeatFoldsOldBucketsIntoAllTime: buckets older than the
// retention fold into the path's all-time bucket (day 0) by kind and are
// dropped; buckets inside the window and the all-time bucket itself are
// left alone; a windowed HotPaths never counts the all-time bucket; and
// a second prune changes nothing.
func TestPruneReadHeatFoldsOldBucketsIntoAllTime(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	if err := s.BumpReadHeat(ctx, []ReadSample{
		{Path: "/a", ActorKind: ReadByAgent, TS: now.Add(-500 * 24 * time.Hour), Count: 3},
		{Path: "/a", ActorKind: ReadByAgent, TS: now.Add(-450 * 24 * time.Hour), Count: 4},
		{Path: "/a", ActorKind: ReadByKernel, TS: now.Add(-450 * 24 * time.Hour), Count: 1},
		{Path: "/a", ActorKind: ReadByAgent, TS: now, Count: 2},
		{Path: "/b", ActorKind: ReadByAgent, TS: now.Add(-10 * 24 * time.Hour), Count: 5},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneReadHeat(ctx, 400)
	if err != nil || n != 3 {
		t.Fatalf("pruned %d %v", n, err)
	}
	var count int64
	if err := s.db.QueryRow(`SELECT count FROM read_heat WHERE path = '/a' AND day = ? AND actor_kind = ?`, AllTimeDay, ReadByAgent).Scan(&count); err != nil || count != 7 {
		t.Fatalf("all-time agent bucket: %d %v", count, err)
	}
	if err := s.db.QueryRow(`SELECT count FROM read_heat WHERE path = '/a' AND day = ? AND actor_kind = ?`, AllTimeDay, ReadByKernel).Scan(&count); err != nil || count != 1 {
		t.Fatalf("all-time kernel bucket: %d %v", count, err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT count(*) FROM read_heat`).Scan(&rows); err != nil || rows != 4 {
		t.Fatalf("rows after prune: %d %v", rows, err)
	}
	hot, err := s.HotPaths(ctx, "/", 30, 10)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]int64{}
	for _, h := range hot {
		by[h.Path] = h.Reads
	}
	if by["/a"] != 2 || by["/b"] != 5 {
		t.Fatalf("windowed hot paths count the all-time bucket: %+v", hot)
	}
	if n, err := s.PruneReadHeat(ctx, 400); err != nil || n != 0 {
		t.Fatalf("second prune: %d %v", n, err)
	}
}
