package cache

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
)

// ReserveDisk reserves headroom for a journal write before the write syscall.
// The caller must release it after the syscall, on success or failure. Journal
// data is not charged to MaxBytes: it is durable user data, not disposable cache.
// The target directory is checked explicitly, including when staging lives on
// a separately mounted filesystem. Reservations are conservatively shared even
// across volumes. Other processes and filesystem metadata can still consume
// space; this is admission control, not a filesystem-level quota.
func (c *Cache) ReserveDisk(dir string, n int64) (func(), error) {
	if n < 0 {
		return nil, errors.New("cache: negative disk reservation")
	}
	if n == 0 {
		return func() {}, nil
	}
	c.admitMu.Lock()
	defer c.admitMu.Unlock()
	for {
		c.mu.Lock()
		closed := c.wb.closed
		reserved := c.reservedBytes + c.writeReserved + c.wb.pending + c.detachedFlushBytes
		c.mu.Unlock()
		if closed {
			return nil, os.ErrClosed
		}
		if n > math.MaxInt64-reserved {
			return nil, ErrNoSpace
		}
		minFree := c.minFree.Load()
		if minFree == 0 {
			break
		}
		free, err := c.opt.FreeSpace(dir)
		if err != nil {
			return nil, fmt.Errorf("cache: check write free space: %w", err)
		}
		if free >= minFree && n <= free-minFree && reserved <= free-minFree-n {
			break
		}
		if !c.evictOne() {
			return nil, ErrNoSpace
		}
	}
	c.mu.Lock()
	c.writeReserved += n
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.writeReserved -= n
			c.mu.Unlock()
		})
	}, nil
}
