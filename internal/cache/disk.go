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
//
// Free space is amortised rather than measured per reservation. Every write
// syscall reserves, and MaxWrite is 64 KiB on darwin, so one large file is
// thousands of reservations; a statfs(2) each, under the admission mutex every
// other writer also needs, is what makes a single big write starve concurrent
// small-file creates. Between measurements the estimate is the last reading
// minus every byte reserved since, which is exact with respect to our own
// writes — whether or not the reservation has been released, since a released
// one has landed on disk — and stale only with respect to other processes,
// bounded to housekeepEvery and freeCheckBytes exactly as cache admission
// already bounds it in makeRoomFor. The estimate is never the last word: a
// refusal is re-measured before it is acted on, so eviction and ErrNoSpace are
// always decided on a fresh reading.
func (c *Cache) ReserveDisk(dir string, n int64) (func(), error) {
	if n < 0 {
		return nil, errors.New("cache: negative disk reservation")
	}
	if n == 0 {
		return func() {}, nil
	}
	c.admitMu.Lock()
	defer c.admitMu.Unlock()
	measure := false
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
		free, fresh, err := c.diskFree(dir, measure)
		if err != nil {
			return nil, fmt.Errorf("cache: check write free space: %w", err)
		}
		if free >= minFree && n <= free-minFree && reserved <= free-minFree-n {
			break
		}
		if !fresh {
			// Estimates only ever refuse too much: writeReserved is subtracted
			// both through reserved and through the estimate. Confirm against
			// the filesystem before evicting anything over one.
			measure = true
			continue
		}
		if !c.evictOne() {
			return nil, ErrNoSpace
		}
	}
	c.mu.Lock()
	c.writeReserved += n
	c.diskSinceCheck += n
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

// diskFree reports the free bytes ReserveDisk should judge against, and
// whether that figure came from the filesystem rather than from the estimate.
// A reading is reused only for the directory it was taken on: reservations are
// shared across volumes but a free-space figure is not transferable between
// them. Callers force a measurement by passing measure, which is how the
// eviction loop avoids spinning against a value its evictions cannot move.
func (c *Cache) diskFree(dir string, measure bool) (free int64, fresh bool, err error) {
	now := c.opt.Now()
	c.mu.Lock()
	usable := !c.diskCheckAt.IsZero() && c.diskCheckDir == dir &&
		now.Sub(c.diskCheckAt) < housekeepEvery && c.diskSinceCheck < freeCheckBytes
	est := c.diskFreeAt - c.diskSinceCheck
	c.mu.Unlock()
	if !measure && usable {
		return est, false, nil
	}
	free, err = c.opt.FreeSpace(dir)
	if err != nil {
		return 0, false, err
	}
	c.mu.Lock()
	c.diskCheckAt, c.diskCheckDir, c.diskFreeAt, c.diskSinceCheck = now, dir, free, 0
	c.mu.Unlock()
	return free, true, nil
}
