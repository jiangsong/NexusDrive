package journal

import (
	"context"
	"testing"
)

func openTestDurability(t *testing.T, d Durability) *Journal {
	t.Helper()
	j, err := Open(Options{Dir: t.TempDir(), Durability: d})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

// TestBarrierCostsOneDeviceFlushPerCommit is the whole point of the level.
//
// `power` flushes the drive twice for one file — once for the staging file,
// once for the objects directory — and on darwin each of those is an
// F_FULLFSYNC at 4.07 ms against 74 us for an ordinary fsync, so the two of
// them are essentially the entire cost of a small-file close(2) (9.72 ms
// measured end to end, 1.15 ms under `crash`). `barrier` issues the staging
// bytes without waiting for the drive, and spends its one flush after the row
// is written, where it covers the staging data, the rename and the row
// together.
func TestBarrierCostsOneDeviceFlushPerCommit(t *testing.T) {
	for _, tc := range []struct {
		mode Durability
		want int64
	}{
		{DurabilityPower, 2},
		{DurabilityBarrier, 1},
		{DurabilityCrash, 0},
	} {
		if tc.mode == DurabilityBarrier && !separateDeviceFlush {
			// Elsewhere fsync(2) already reaches stable storage, so there is
			// no cheaper call and barrier spends what power spends. It still
			// reorders the flush, which the ordering test below pins.
			tc.want = 2
		}
		j := openTestDurability(t, tc.mode)
		before := j.DeviceFlushes()
		if err := j.Commit(context.Background(), stage(t, j, "f.txt", []byte("hello"))); err != nil {
			t.Fatal(err)
		}
		if got := j.DeviceFlushes() - before; got != tc.want {
			t.Errorf("%s: %d device flushes for one committed file, want %d", tc.mode, got, tc.want)
		}
	}
}

// TestBarrierFlushesAfterTheRowIsWritten is the correctness half, and it is
// why barrier is a stronger promise than power rather than a weaker one.
//
// Under `power` both flushes happen *before* the row insert, so the row —
// the only thing that says the blob belongs to a file — is never covered by
// a device flush at all: SQLite runs a plain fsync unless PRAGMA fullfsync is
// set, and nothing in this tree sets it (TODO.md T-62). Under `barrier` the
// flush comes last, so the row is on stable storage by the time close(2)
// returns.
func TestBarrierFlushesAfterTheRowIsWritten(t *testing.T) {
	j := openTestDurability(t, DurabilityBarrier)
	var txsAtFlush int64 = -1
	j.onDeviceFlush = func() { txsAtFlush = j.writeTxs.Load() }
	before := j.writeTxs.Load()
	if err := j.Commit(context.Background(), stage(t, j, "f.txt", []byte("hello"))); err != nil {
		t.Fatal(err)
	}
	if txsAtFlush < 0 {
		t.Fatal("barrier committed a file without flushing the device")
	}
	if txsAtFlush <= before {
		t.Fatalf("the device flush ran before the row was written (%d write transactions at the flush, %d before the commit); "+
			"the row it is meant to cover was still only in the page cache", txsAtFlush, before)
	}
}

// TestPowerStillFlushesBeforeTheRow guards the other direction: fixing power's
// ordering is a separate decision (T-62) and must not happen by accident while
// barrier is being added.
func TestPowerStillFlushesBeforeTheRow(t *testing.T) {
	j := openTestDurability(t, DurabilityPower)
	var txsAtFlush int64 = -1
	j.onDeviceFlush = func() {
		if txsAtFlush < 0 {
			txsAtFlush = j.writeTxs.Load()
		}
	}
	before := j.writeTxs.Load()
	if err := j.Commit(context.Background(), stage(t, j, "f.txt", []byte("hello"))); err != nil {
		t.Fatal(err)
	}
	if txsAtFlush != before {
		t.Fatalf("power's first flush saw %d write transactions, want %d: its ordering changed", txsAtFlush, before)
	}
}

// TestBarrierIsTheDefault: the level a person gets without saying anything is
// the one that is both faster than the old default and keeps a stronger
// promise than it did.
func TestBarrierIsTheDefault(t *testing.T) {
	j, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	if got := j.Durability(); got != DurabilityBarrier {
		t.Fatalf("default durability = %q, want %q", got, DurabilityBarrier)
	}
}

// TestBarrierKeepsTheWalSynchronous: the flush covers the row only if the row
// has been handed to the device by then. synchronous=NORMAL would leave it in
// SQLite's own buffer, where a device flush cannot reach it.
func TestBarrierKeepsTheWalSynchronous(t *testing.T) {
	j := openTestDurability(t, DurabilityBarrier)
	var mode int
	if err := j.db.QueryRow(`PRAGMA synchronous`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != 2 { // 2 = FULL
		t.Fatalf("PRAGMA synchronous = %d under barrier, want 2 (FULL)", mode)
	}
}
