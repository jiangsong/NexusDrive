package cache

import (
	"errors"
	"testing"
)

// A journal write reserves before every write syscall, and MaxWrite is 64 KiB
// on darwin, so a single 64 MiB file is a thousand reservations. Each one
// calling statfs(2) under the one admission mutex is what starves concurrent
// small-file creates, so the reservation path must amortise the syscall the
// way cache admission already does.
func TestDiskReservationDoesNotStatfsEveryWrite(t *testing.T) {
	calls := 0
	c, _ := newTest(t, Options{MinFree: 1 << 20, FreeSpace: func(string) (int64, error) {
		calls++
		return 1 << 30, nil
	}})
	for i := 0; i < 1024; i++ {
		release, err := c.ReserveDisk(c.opt.Dir, 64<<10)
		if err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
		release()
	}
	// 1024 x 64 KiB is 64 MiB: one reading to start with, and one more when
	// 32 MiB have been admitted against it. The clock never advances, so the
	// interval rule forces nothing on top of that.
	if calls > 2 {
		t.Fatalf("statfs calls = %d, want <= 2 for 1024 reservations", calls)
	}
}

// The amortised estimate is exact for our own reservations and stale only with
// respect to other processes, bounded by the same interval cache admission
// uses. Once that bound expires a disk another process filled must still be
// refused rather than admitted off the remembered value.
func TestDiskReservationStillRefusesWhenFreeSpaceFalls(t *testing.T) {
	free := int64(1 << 30)
	calls := 0
	c, clk := newTest(t, Options{MinFree: 1 << 20, FreeSpace: func(string) (int64, error) {
		calls++
		return free, nil
	}})
	release, err := c.ReserveDisk(c.opt.Dir, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	release()
	free = 1 << 10 // another process filled the disk behind us
	release, err = c.ReserveDisk(c.opt.Dir, 1<<10)
	if err != nil {
		t.Fatalf("estimate window: %v", err)
	}
	release()
	if calls != 1 {
		t.Fatalf("statfs calls inside the estimate window = %d, want 1", calls)
	}
	clk.advance(2 * housekeepEvery)
	if _, err := c.ReserveDisk(c.opt.Dir, 1<<10); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("stale estimate admitted a write onto a full disk: %v", err)
	}
}

// Lowering MinFree lets writes through that a full disk was refusing, and
// raising it must bite immediately; either way the remembered free value was
// measured against the old headroom and cannot answer for the new one.
func TestDiskReservationRecheckesAfterBudgetChange(t *testing.T) {
	free := int64(100)
	c, _ := newTest(t, Options{MinFree: 40, FreeSpace: func(string) (int64, error) { return free, nil }})
	release, err := c.ReserveDisk(c.opt.Dir, 8)
	if err != nil {
		t.Fatal(err)
	}
	release()
	free = 50
	c.SetBudget(0, 45)
	if _, err := c.ReserveDisk(c.opt.Dir, 8); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("new headroom judged against the old free-space reading: %v", err)
	}
}

// Reservations for two staging volumes share one counter but not one free
// figure: a value read from one filesystem says nothing about the other.
func TestDiskReservationDoesNotReuseAnotherVolumesFreeSpace(t *testing.T) {
	c, _ := newTest(t, Options{MinFree: 40, FreeSpace: func(dir string) (int64, error) {
		if dir == "full-volume" {
			return 10, nil
		}
		return 100, nil
	}})
	release, err := c.ReserveDisk("roomy-volume", 8)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := c.ReserveDisk("full-volume", 8); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("one volume's free space admitted a write to another: %v", err)
	}
}

// The eviction loop must not spin on the estimate: every pass past a refusal
// asks the filesystem again, so eviction is judged on what it actually freed.
func TestDiskReservationEvictionSeesEachStatfs(t *testing.T) {
	var c *Cache
	calls := 0
	c, _ = newTest(t, Options{BlockSize: 64, MinFree: 64, FreeSpace: func(string) (int64, error) {
		calls++
		if c == nil {
			return 160, nil
		}
		return 160 - c.Stats().Bytes, nil
	}})
	for i := int64(0); i < 2; i++ {
		if err := c.Put(FileKey{Remote: "r", RemoteID: "f", Version: "v"}, i, make([]byte, 32), 64); err != nil {
			t.Fatal(err)
		}
	}
	before := calls
	release, err := c.ReserveDisk(c.opt.Dir, 40)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if calls-before < 2 {
		t.Fatalf("eviction ran on a single stale reading: %d statfs calls", calls-before)
	}
}
