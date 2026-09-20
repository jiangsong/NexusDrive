package journal

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestCommitCountsItsDurabilityWork pins what one committed file costs in
// disk flushes. Every later claim about the write path being cheaper is
// measured with these counters, so they have to agree with the obvious case
// first: one staging fsync, one objects-directory fsync, one transaction.
func TestCommitCountsItsDurabilityWork(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	staging0, objects0, txs0 := j.DurabilityStats()
	u := stage(t, j, "counted.txt", []byte("hello"))
	if err := j.Commit(ctx, u); err != nil {
		t.Fatal(err)
	}
	staging, objects, txs := j.DurabilityStats()
	if got := staging - staging0; got != 1 {
		t.Errorf("staging fsyncs=%d, want 1", got)
	}
	if got := objects - objects0; got != 1 {
		t.Errorf("objects directory fsyncs=%d, want 1", got)
	}
	if got := txs - txs0; got != 1 {
		t.Errorf("write transactions=%d, want 1", got)
	}
}

// TestDirectoryCommitDoesNotSyncTheObjectsDirectory: a row that carries no
// blob points at nothing in objects/, so making that directory's entries
// durable buys nothing — and on darwin an os.File.Sync is an F_FULLFSYNC, so
// every `mkdir` in a recursive copy used to pay one for nothing.
func TestDirectoryCommitDoesNotSyncTheObjectsDirectory(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()

	_, objects0, _ := j.DurabilityStats()
	dir := Upload{ID: NewID(), Kind: KindMkdir, Remote: "ali", RemoteParentID: "root", Name: "d", Ino: 11}
	if err := j.Commit(ctx, dir); err != nil {
		t.Fatal(err)
	}
	_, objects1, _ := j.DurabilityStats()
	if got := objects1 - objects0; got != 0 {
		t.Errorf("directory commit paid %d objects directory fsyncs, want 0", got)
	}

	file := stage(t, j, "f.txt", []byte("bytes"))
	if err := j.Commit(ctx, file); err != nil {
		t.Fatal(err)
	}
	_, objects2, _ := j.DurabilityStats()
	if got := objects2 - objects1; got != 1 {
		t.Errorf("file commit paid %d objects directory fsyncs, want 1", got)
	}

	// A batch that mixes both still has to sync: the file row in it names a
	// staged object. Build the batch by hand — waiting for two concurrent
	// commits to land in the same one is not something a test can demand.
	mixedDir := Upload{ID: NewID(), Kind: KindMkdir, Remote: "ali", RemoteParentID: "root", Name: "d2", Ino: 12}
	mixedFile := stage(t, j, "g.txt", []byte("more bytes"))
	_, objects3, _ := j.DurabilityStats()
	batch := []commitReq{
		{u: mixedDir, done: make(chan error, 1)},
		{u: mixedFile, done: make(chan error, 1)},
	}
	j.committer.commitBatch(batch)
	for _, r := range batch {
		if err := <-r.done; err != nil {
			t.Fatal(err)
		}
	}
	_, objects4, _ := j.DurabilityStats()
	if got := objects4 - objects3; got != 1 {
		t.Errorf("mixed batch paid %d objects directory fsyncs, want 1", got)
	}
	for _, id := range []string{dir.ID, file.ID, mixedDir.ID, mixedFile.ID} {
		if _, err := j.Get(ctx, id); err != nil {
			t.Fatalf("row %s missing after commit: %v", id, err)
		}
	}
}

// TestCommitDoesNotAcknowledgeFailedDirectorySync: whatever the level, a
// commit that could not make the objects directory durable must not be
// reported as durable.
//
// What differs is what is left behind, and that follows from where the flush
// sits. power flushes before the row insert, so a failure leaves no row at
// all. barrier flushes after it, so the row stays — the close(2) is refused
// while the file is queued anyway, which is a spurious error rather than a
// lost write. That is the safe direction of the two, and it is the price of
// having one flush cover the row instead of never covering it.
func TestCommitDoesNotAcknowledgeFailedDirectorySync(t *testing.T) {
	for _, mode := range []Durability{DurabilityPower, DurabilityBarrier} {
		t.Run(string(mode), func(t *testing.T) {
			testFailedDirectorySync(t, mode)
		})
	}
}

func testFailedDirectorySync(t *testing.T, mode Durability) {
	j := openTestDurability(t, mode)
	u := stage(t, j, "durable", []byte("must be recoverable"))
	saved := j.ObjectsDir() + ".saved"
	if err := os.Rename(j.ObjectsDir(), saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Rename(saved, j.ObjectsDir()) })
	ctx := context.Background()
	if err := j.Commit(ctx, u); err == nil {
		t.Fatal("commit acknowledged without syncing objects directory")
	}
	_, err := j.Get(ctx, u.ID)
	if mode == DurabilityPower && !errors.Is(err, ErrNotFound) {
		t.Fatalf("power flushes before the insert, so a failed commit must leave no row: %v", err)
	}
	if mode == DurabilityBarrier && err != nil {
		t.Fatalf("barrier flushes after the insert, so the row stays and the file still uploads: %v", err)
	}
	if err := os.Rename(saved, j.ObjectsDir()); err != nil {
		t.Fatal(err)
	}
	// The retry succeeds either way: under power it inserts the row it never
	// got to, and under barrier it re-commits one that is already there.
	if err := j.Commit(ctx, u); err != nil {
		t.Fatalf("retry after restoring storage: %v", err)
	}
	if _, err := j.Get(ctx, u.ID); err != nil {
		t.Fatalf("the retried commit left no row: %v", err)
	}
}
