package cache

import (
	"bytes"
	"sync/atomic"
	"testing"
	"time"
)

// putWholeFile fills every block of a two-block file of blockSize 16.
func putWholeFile(t *testing.T, c *Cache, k FileKey) {
	t.Helper()
	for i, b := range []byte{'a', 'b'} {
		if err := c.Put(k, int64(i), bytes.Repeat([]byte{b}, 16), 32); err != nil {
			t.Fatal(err)
		}
	}
}

// hydrateTestCache keeps the janitor's own timer an hour away so the test
// drives hydrateDue itself: nothing below depends on wall clock, which is what
// lets these run under -race.
func hydrateTestCache(t *testing.T, busy *atomic.Bool) (*Cache, *clock) {
	t.Helper()
	c, clk := newTest(t, Options{BlockSize: 16, HydrateAfter: time.Hour})
	if busy != nil {
		c.SetBusy(busy.Load)
	}
	return c, clk
}

// A burst of reads keeps the mount's foreground counter above zero for its
// whole duration, so requiring a mount-wide quiet moment meant nothing
// hydrated until the burst ended — and every "cached" read in it kept paying
// the block layout's read amplification. A file nothing has touched since it
// was queued can be merged without interrupting anything.
func TestHydrationDoesNotWaitForTheWholeMountToGoQuiet(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	c, _ := hydrateTestCache(t, &busy)
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v"}
	putWholeFile(t, c, k)

	c.hydrateDue()
	if !busy.Load() {
		t.Fatal("the test stopped asserting the busy signal")
	}
	if _, ok := c.HydratedPath(k); !ok {
		t.Fatal("an idle file waited for the whole mount to go quiet")
	}
}

// The per-file window is what replaces the mount-wide one, so a file that is
// being read must still be left alone: merging it is a whole-file copy landing
// on top of the read. Its wait then restarts, and one whole interval — one
// janitor pass — has to go by untouched before it is merged.
func TestHydrationWaitsForReadsOfTheFileItself(t *testing.T) {
	c, clk := hydrateTestCache(t, nil)
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v"}
	putWholeFile(t, c, k)

	clk.advance(time.Second) // the read lands after the file was queued
	dst := make([]byte, 16)
	if _, ok := c.ReadAt(k, 0, 0, dst); !ok {
		t.Fatal("read missed")
	}
	c.hydrateDue()
	if _, ok := c.HydratedPath(k); ok {
		t.Fatal("merged a file out from under a read of that same file")
	}
	c.hydrateDue()
	if _, ok := c.HydratedPath(k); !ok {
		t.Fatal("never hydrated after the file went quiet")
	}
}

// Hydration reads the block files, so a block whose bytes the write-behind
// worker still holds in memory is not one it can merge; the file waits rather
// than failing and falling out of the queue.
func TestHydrationWaitsForBlocksNotYetOnDisk(t *testing.T) {
	c, _ := hydrateTestCache(t, nil)
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v"}
	putWholeFile(t, c, k)

	id := blockID{k.hash(), 1}
	c.mu.Lock()
	c.blocks[id].mem = bytes.Repeat([]byte("b"), 16) // a flush still in flight
	c.mu.Unlock()
	c.hydrateDue()
	if _, ok := c.HydratedPath(k); ok {
		t.Fatal("merged a file whose bytes were not all on disk")
	}
	c.mu.Lock()
	c.blocks[id].mem = nil
	c.mu.Unlock()
	c.hydrateDue()
	if _, ok := c.HydratedPath(k); !ok {
		t.Fatal("dropped the file instead of waiting for the flush")
	}
}

// A reader can arrive after the copy has begun. Finishing would put a
// whole-file write in front of it, so the copy is abandoned and the file goes
// back in the queue rather than being lost.
func TestHydrationStopsWhenTheFileIsReadMidCopy(t *testing.T) {
	c, _ := hydrateTestCache(t, nil)
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v"}
	putWholeFile(t, c, k)

	// Hydrate stamps its start from the clock; a block touched after that is
	// what a reader arriving mid-copy looks like from inside the loop.
	c.mu.Lock()
	c.blocks[blockID{k.hash(), 1}].lastAccess = c.opt.Now().Add(time.Second)
	c.mu.Unlock()
	if err := c.Hydrate(k); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.HydratedPath(k); ok {
		t.Fatal("finished a copy on top of a read of the same file")
	}
	c.mu.Lock()
	queued := len(c.hydrateWant)
	c.mu.Unlock()
	if queued != 1 {
		t.Fatalf("abandoned file not requeued: %d entries", queued)
	}
}

// A file that can never be merged must leave the queue, or the janitor re-arms
// for it for the lifetime of the mount.
func TestHydrationForgetsFilesItCanNeverMerge(t *testing.T) {
	c, _ := hydrateTestCache(t, nil)
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v"}
	putWholeFile(t, c, k)
	if !c.evictOne() {
		t.Fatal("nothing to evict")
	}

	c.hydrateDue()
	c.mu.Lock()
	queued, armed := len(c.hydrateWant), c.hydrateTimer != nil
	c.mu.Unlock()
	if queued != 0 || armed {
		t.Fatalf("incomplete file still queued: %d entries, timer armed = %v", queued, armed)
	}
}

// Merging is a whole-file copy. Individually idle files may all be ready at
// once, and copying every one of them while the mount is under load is the
// disk storm the mount-wide check used to prevent; a pass takes a few.
func TestHydrationIsRateLimitedWhileTheMountIsBusy(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	c, _ := hydrateTestCache(t, &busy)
	keys := make([]FileKey, hydrateWhileBusy+3)
	for i := range keys {
		keys[i] = FileKey{Remote: "r", RemoteID: string(rune('a' + i)), Version: "v"}
		putWholeFile(t, c, keys[i])
	}

	c.hydrateDue()
	done := 0
	for _, k := range keys {
		if _, ok := c.HydratedPath(k); ok {
			done++
		}
	}
	if done != hydrateWhileBusy {
		t.Fatalf("hydrated %d files in one busy pass, want %d", done, hydrateWhileBusy)
	}
	busy.Store(false)
	c.hydrateDue()
	for _, k := range keys {
		if _, ok := c.HydratedPath(k); !ok {
			t.Fatalf("%v never hydrated once the mount went quiet", k)
		}
	}
}
