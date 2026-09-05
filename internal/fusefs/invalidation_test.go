package fusefs

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestBulkInvalidationIsBoundedAndReplaysConcurrentChanges(t *testing.T) {
	var c coalescedInvalidation
	started := make(chan int, 4)
	release := make(chan struct{})
	var calls, active, maxActive atomic.Int32
	run := func() {
		n := active.Add(1)
		for {
			old := maxActive.Load()
			if n <= old || maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		started <- int(calls.Add(1))
		<-release
		active.Add(-1)
	}
	c.schedule(run)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first invalidation did not start")
	}
	for range 1000 {
		c.schedule(run)
	}
	if calls.Load() != 1 || active.Load() != 1 {
		t.Fatal("requests spawned concurrent full traversals")
	}
	release <- struct{}{}
	select {
	case n := <-started:
		if n != 2 {
			t.Fatalf("unexpected traversal %d", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("changes during traversal were lost")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		running := c.running
		c.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("invalidation worker did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 2 || maxActive.Load() != 1 {
		t.Fatalf("unbounded work: calls=%d active=%d", calls.Load(), maxActive.Load())
	}
}
