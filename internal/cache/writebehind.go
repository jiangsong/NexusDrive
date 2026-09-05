package cache

import (
	"os"
	"path/filepath"
	"sync"
)

// Write-behind for whole blocks.
//
// A cold sequential read fetches blocks faster than a busy disk can take
// them, and writing each block to the cache before handing it to the reader
// put the disk on the read path: on a machine whose disk sustained ~250 MB/s
// the read stalled at exactly that, with the link idle at twice the rate.
// PutAsync registers the block as present, keeps its bytes in memory, and
// lets a couple of workers write it out; reads meanwhile are served from the
// copy in memory. The memory held this way is bounded, and past the bound
// PutAsync degrades to the synchronous Put.

const (
	writeBehindMax     = 256 << 20
	writeBehindWorkers = 2
	// subBlockReserve is the part of the bound whole blocks may not use, so
	// sub-block misses always have memory to land in.
	subBlockReserve = 32 << 20
)

type writeBehind struct {
	once    sync.Once
	queue   chan blockID
	pending int64 // bytes held in memory, under Cache.mu
	wg      sync.WaitGroup
	closed  bool // under Cache.mu; after Close, PutAsync writes synchronously
	// sendMu is held for reading while a block id is on its way to the
	// queue and for writing while the queue is closed. Checking closed
	// under Cache.mu and sending after the unlock is a send on a closed
	// channel — a panic, and the worker doing the requeue is exactly the
	// one Close is waiting for.
	sendMu sync.RWMutex
}

// enqueue hands a block to the write-behind workers unless the cache is
// closing. It reports whether the block was queued.
func (c *Cache) enqueue(id blockID) bool {
	c.wb.sendMu.RLock()
	defer c.wb.sendMu.RUnlock()
	c.mu.Lock()
	closed, q := c.wb.closed, c.wb.queue
	c.mu.Unlock()
	if closed || q == nil {
		return false
	}
	q <- id
	return true
}

// Close stops the write-behind workers after they have drained what was
// queued, and cancels pending hydrations. Nothing may be in flight on a
// directory a test is about to remove, or on a cache the daemon is closing.
func (c *Cache) Close() error {
	c.mu.Lock()
	if c.wb.closed {
		c.mu.Unlock()
		return nil
	}
	c.wb.closed = true
	if c.claimSweep != nil {
		c.claimSweep.Stop()
		c.claimSweep = nil
	}
	q := c.wb.queue
	if c.hydrateTimer != nil {
		c.hydrateTimer.Stop()
		c.hydrateTimer = nil
	}
	c.hydrateWant = nil
	c.mu.Unlock()
	if q != nil {
		// No sender may be between "not closed" and the send itself.
		c.wb.sendMu.Lock()
		close(q)
		c.wb.sendMu.Unlock()
		c.wb.wg.Wait()
	}
	c.sweepAllClaims()
	c.hydrateWG.Wait()
	return nil
}

// PutAsync stores a whole block like Put, but returns as soon as the block
// is registered and readable; the file write happens in the background.
func (c *Cache) PutAsync(k FileKey, idx int64, data []byte, fileSize int64) error {
	c.admitMu.Lock()
	locked := true
	defer func() {
		if locked {
			c.admitMu.Unlock()
		}
	}()
	if int64(len(data)) > c.opt.BlockSize {
		return c.put(k, idx, data, fileSize)
	}
	if err := c.blockRoom(k, idx, 0, int64(len(data)), fileSize, false); err != nil {
		return err
	}
	limit := c.opt.WriteBehind
	if limit <= 0 {
		limit = writeBehindMax
	}
	c.mu.Lock()
	if c.wb.closed || c.wb.pending+int64(len(data)) > limit-subBlockReserve {
		c.mu.Unlock()
		return c.put(k, idx, data, fileSize)
	}
	fh := k.hash()
	id := blockID{fh, idx}
	if old, ok := c.blocks[id]; ok {
		c.bytes -= old.size
		c.dropBlockLocked(id)
	}
	// The caller hands over the buffer: it was fetched for this block and
	// the reader copies out of it before anything else can touch it.
	m := &blockMeta{size: int64(len(data)), lastAccess: c.opt.Now(), mem: data}
	c.blocks[id] = m
	c.bytes += int64(len(data))
	c.wb.pending += int64(len(data))
	fs, ok := c.files[fh]
	if !ok {
		fs = &fileState{key: k, present: map[int64]bool{}}
		c.files[fh] = fs
	}
	fs.key, fs.size = k, fileSize
	fs.generation++
	remember := !fs.keyWritten
	fs.keyWritten = true
	fs.present[idx] = true
	if fs.pinned || fs.userPinned {
		m.pinned = true
	}
	c.wb.once.Do(func() {
		c.wb.queue = make(chan blockID, 4096)
		c.wb.wg.Add(writeBehindWorkers)
		for i := 0; i < writeBehindWorkers; i++ {
			go c.writeBehindWorker()
		}
	})
	c.mu.Unlock()
	if remember {
		c.rememberKey(k)
	}
	c.admitMu.Unlock()
	locked = false
	if !c.enqueue(id) {
		// Closed between the check above and here: write it out now, so the
		// block is not left in memory with nobody to flush it.
		c.flushBlock(id)
	}
	return nil
}

func (c *Cache) writeBehindWorker() {
	defer c.wb.wg.Done()
	for id := range c.wb.queue {
		c.flushBlock(id)
	}
}

// flushBlock writes one pending block to disk and releases its memory.
func (c *Cache) flushBlock(id blockID) {
	// Freeze the transition into an in-flight disk write while an admission
	// computes replacement credit. The IO itself remains parallel.
	c.admitMu.Lock()
	c.mu.Lock()
	m := c.blocks[id]
	if m != nil && m.mem == nil && m.partial != nil {
		c.mu.Unlock()
		c.admitMu.Unlock()
		c.flushSubs(id)
		return
	}
	if m == nil || m.mem == nil || m.flushing {
		c.mu.Unlock()
		c.admitMu.Unlock()
		return
	}
	data := m.mem
	m.flushing = true
	c.mu.Unlock()
	c.admitMu.Unlock()
	var temp string
	defer func() {
		if temp != "" {
			c.discardTemp(temp)
		}
		c.mu.Lock()
		m.flushing = false
		if m.flushDetached {
			c.detachedFlushBytes -= int64(len(data))
			c.detachedFlushEntries--
			m.flushDetached = false
		}
		c.mu.Unlock()
	}()
	// A unique unpublished file cannot overwrite another generation's temp.
	p := c.blockPath(id.file, id.index)
	err := os.MkdirAll(filepath.Dir(p), 0o700)
	if err == nil {
		var out *os.File
		out, err = os.CreateTemp(filepath.Dir(p), ".flush-*")
		if err == nil {
			temp = out.Name()
			_, err = out.Write(data)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
		}
	}
	c.mu.Lock()
	var key FileKey
	complete := false
	// Validate BEFORE publication, under the same lock as eviction. An old
	// flush must not resurrect a forgotten block or overwrite its replacement.
	if cur := c.blocks[id]; cur == m && cur.mem != nil {
		if err == nil {
			err = os.Rename(temp, p)
		}
		if err == nil {
			os.Remove(c.sidecarPath(id.file, id.index))
		}
		if err != nil {
			c.bytes -= m.size
			c.dropBlockLocked(id)
			if fs := c.files[id.file]; fs != nil {
				delete(fs.present, id.index)
			}
		} else {
			m.mem = nil
			c.wb.pending -= int64(len(data))
			if fs := c.files[id.file]; fs != nil && !fs.hydrated {
				key = fs.key
				complete = int64(len(fs.present)) == c.BlockCount(fs.size) && fs.size > 0 && !c.anyInMemoryLocked(id.file)
			}
		}
	}
	c.mu.Unlock()
	if complete {
		c.scheduleHydrate(key)
	}
}

// anyInMemoryLocked reports whether a file still has blocks not yet on disk.
func (c *Cache) anyInMemoryLocked(fh string) bool {
	for id, m := range c.blocks {
		if id.file == fh && m.mem != nil {
			return true
		}
	}
	return false
}

// WriteBehindPending reports bytes registered but not yet on disk.
func (c *Cache) WriteBehindPending() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wb.pending
}
