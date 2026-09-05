package vfs

import (
	"testing"
	"time"
)

// TestPrefetchStandsAsideForReads: warming listings in the background costs a
// provider round trip and a metadata write transaction, both of which a read
// waiting on the kernel would queue behind. The prefetcher waits while reads
// are in flight.
func TestPrefetchStandsAsideForReads(t *testing.T) {
	f := &FS{}
	p := &prefetcher{fs: f, depth: 1, stopC: make(chan struct{})}

	done := make(chan struct{})
	f.fgIO.Add(1)
	go func() { p.waitIdle(); close(done) }()
	select {
	case <-done:
		t.Fatal("prefetch ran while a read was in flight")
	case <-time.After(20 * time.Millisecond):
	}
	f.fgIO.Add(-1)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("prefetch never resumed after the reads finished")
	}

	// Reads never stop entirely on a busy mount, so the wait is bounded.
	f.fgIO.Add(1)
	defer f.fgIO.Add(-1)
	stopped := make(chan struct{})
	go func() { p.waitIdle(); close(stopped) }()
	close(p.stopC)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("a stopped prefetcher kept waiting")
	}
}
