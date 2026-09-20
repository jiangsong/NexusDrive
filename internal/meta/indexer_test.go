package meta

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// openTestIndexer opens a store whose indexer ticks fast enough for a test to
// watch several intervals go by.
func openTestIndexer(t *testing.T, interval time.Duration) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "meta.db"), Options{IndexInterval: interval})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// pendingRows reports how many names are still waiting to enter the FTS index.
func pendingRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM name_index_pending`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestIdleIndexerDoesNotOpenWriteTransactions: the DSN carries
// _txlock=immediate, so every transaction the indexer opens is a write
// transaction that takes the in-process writer lock and SQLite's. An idle
// mount paid that twice a second for a table it knew nothing about, which is
// both a wakeup and a stall on whatever the FUSE path was about to write.
func TestIdleIndexerDoesNotOpenWriteTransactions(t *testing.T) {
	const interval = 5 * time.Millisecond
	s := openTestIndexer(t, interval)
	ctx := context.Background()
	// Drain whatever the migrations queued, so what follows is a truly idle
	// store rather than one with work to do.
	if err := s.FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if n := pendingRows(t, s); n != 0 {
		t.Fatalf("store is not idle: %d pending names", n)
	}
	_, before := s.QueryStats()
	time.Sleep(20 * interval)
	_, after := s.QueryStats()
	if got := after - before; got != 0 {
		t.Fatalf("an idle indexer opened %d write transactions over 20 tick intervals, want 0", got)
	}
}

// TestIndexerPicksUpARowQueuedAfterAnIdleTick is the other half of the cheap
// idle tick: skipping the transaction must never skip the work. A name that
// lands just after a tick found the table empty has to be indexed by the next
// one.
func TestIndexerPicksUpARowQueuedAfterAnIdleTick(t *testing.T) {
	const interval = 5 * time.Millisecond
	s := openTestIndexer(t, interval)
	ctx := context.Background()
	if err := s.FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
	// Let several idle ticks go by, then queue a name without flushing it.
	time.Sleep(10 * interval)
	if _, err := s.Upsert(ctx, file(RootIno, "queued-after-idle.txt", 1)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if pendingRows(t, s) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a name queued after an idle tick was never indexed")
		}
		time.Sleep(interval)
	}
}
