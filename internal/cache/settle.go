package cache

import (
	"context"
	"errors"
	"time"
)

// SettleFile waits for this file's write-behind data and verifies completeness.
// Pin uses this barrier: a block merely admitted to memory can still be rejected
// by the disk. Ordinary reads do not pay for the barrier. This is cache writeback
// completion, not a promise of power-loss durability like the journal's fsync.
func (c *Cache) SettleFile(ctx context.Context, k FileKey, size int64) error {
	fh := k.hash()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		pending := c.anyInMemoryLocked(fh)
		fs := c.files[fh]
		complete := fs != nil && (fs.hydrated || int64(len(fs.present)) == c.BlockCount(size))
		c.mu.Unlock()
		if !pending {
			if !complete {
				return errors.New("cache: pinned file is incomplete after cache writeback")
			}
			return nil
		}
		t := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
