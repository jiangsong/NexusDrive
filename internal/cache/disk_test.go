package cache

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
)

func TestWriteReservationsAreSharedAndOutsideCacheQuota(t *testing.T) {
	c, _ := newTest(t, Options{MaxBytes: 1, MinFree: 40, FreeSpace: func(dir string) (int64, error) {
		if dir != "journal-volume" {
			t.Errorf("target: %s", dir)
		}
		return 100, nil
	}})
	var wg sync.WaitGroup
	releases := make(chan func(), 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := c.ReserveDisk("journal-volume", 8)
			if err == nil {
				releases <- release
			} else if !errors.Is(err, syscall.ENOSPC) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	close(releases)
	if s := c.Stats(); s.WriteReservedBytes != 56 || s.Bytes != 0 {
		t.Fatalf("reservations: %+v", s)
	}
	for release := range releases {
		release()
		release()
	}
	if c.Stats().WriteReservedBytes != 0 {
		t.Fatal("release leaked or ran twice")
	}
	r, err := c.ReserveDisk("journal-volume", 60)
	if err != nil {
		t.Fatal(err)
	}
	r()
}

func TestCacheAdmissionCannotConsumeJournalReservation(t *testing.T) {
	c, _ := newTest(t, Options{MinFree: 40, FreeSpace: func(string) (int64, error) { return 100, nil }})
	release, err := c.ReserveDisk(c.opt.Dir, 60)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put(key, 0, []byte("x"), 1); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("cache stole reservation: %v", err)
	}
	release()
	if err := c.Put(key, 0, []byte("x"), 1); err != nil {
		t.Fatal(err)
	}
}

func TestWritePressureEvictsOnlyDisposableCache(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprint(pinned), func(t *testing.T) {
			var c *Cache
			c, _ = newTest(t, Options{BlockSize: 64, MinFree: 64, FreeSpace: func(string) (int64, error) {
				if c == nil {
					return 160, nil
				}
				return 160 - c.Stats().Bytes, nil
			}})
			if err := c.Put(key, 0, make([]byte, 64), 64); err != nil {
				t.Fatal(err)
			}
			c.Pin(key, pinned)
			release, err := c.ReserveDisk(c.opt.Dir, 40)
			if pinned {
				if !errors.Is(err, ErrNoSpace) || !c.Has(key, 0) {
					t.Fatalf("discarded protected bytes: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				release()
				if c.Has(key, 0) {
					t.Fatal("did not reclaim disposable content")
				}
			}
		})
	}
}

func TestWriteReservationAccountsForUnflushedAndHydratingBytes(t *testing.T) {
	c, _ := newTest(t, Options{MinFree: 40, FreeSpace: func(string) (int64, error) { return 100, nil }})
	// Hold deterministic in-flight states without allowing a worker to finish
	// before admission. Both represent promised bytes not yet visible to statfs.
	c.mu.Lock()
	c.wb.pending = 32
	c.reservedBytes = 20
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.wb.pending = 0; c.reservedBytes = 0; c.mu.Unlock() }()
	if _, err := c.ReserveDisk(c.opt.Dir, 9); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("overcommitted pending disk writes: %v", err)
	}
	release, err := c.ReserveDisk(c.opt.Dir, 8)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestFreeSpaceErrorsFailClosedAndClosedCacheRejectsWrites(t *testing.T) {
	want := errors.New("statfs unavailable")
	c, _ := newTest(t, Options{MinFree: 1, FreeSpace: func(string) (int64, error) { return 0, want }})
	if _, err := c.ReserveDisk(c.opt.Dir, 1); !errors.Is(err, want) {
		t.Fatalf("write failed open: %v", err)
	}
	if err := c.Put(key, 0, []byte("x"), 1); !errors.Is(err, want) {
		t.Fatalf("cache failed open: %v", err)
	}
	c.Close()
	if _, err := c.ReserveDisk(c.opt.Dir, 1); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed: %v", err)
	}
}

func TestRetiredFlushKeepsItsReservation(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 16, MinFree: 1, FreeSpace: func(string) (int64, error) { return 17, nil }})
	id := blockID{key.hash(), 0}
	m := &blockMeta{mem: make([]byte, 16), size: 16, flushing: true}
	c.mu.Lock()
	c.blocks[id] = m
	c.files[id.file] = &fileState{key: key, size: 16, present: map[int64]bool{0: true}}
	c.bytes, c.wb.pending = 16, 16
	c.mu.Unlock()
	if err := c.Put(key, 0, make([]byte, 16), 16); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("replacement spent active flush credit: %v", err)
	}
	c.Forget(key)
	if s := c.Stats(); s.Bytes != 0 || s.ReservedBytes != 16 {
		t.Fatalf("retired flush disappeared: %+v", s)
	}
	if _, err := c.ReserveDisk(c.opt.Dir, 1); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("journal spent retired flush credit: %v", err)
	}
	if err := c.Put(key, 0, []byte("x"), 1); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("cache spent retired flush credit: %v", err)
	}
	// Model the writer finishing and removing its unpublished temporary.
	c.mu.Lock()
	c.detachedFlushBytes, c.detachedFlushEntries = 0, 0
	m.flushing, m.flushDetached = false, false
	c.mu.Unlock()
	if err := c.Put(key, 0, []byte("x"), 1); err != nil {
		t.Fatal(err)
	}
}
