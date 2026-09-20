package journal

import (
	"context"
	"errors"
	"testing"
)

// A flush that fails after the insert transaction has committed must be
// reported and must not be retried.
//
// The retry path exists for a transaction that failed: nothing was written, so
// re-running the insert row by row is free and each caller gets its own
// verdict. After a commit it is not free. insertUploadTx is an upsert whose
// DO UPDATE sets state from the incoming row, so re-running it against a row a
// worker has already claimed puts the row back to pending and the file is
// uploaded twice; against a row that has completed and been trimmed it brings
// the row back from the dead. It also hands every caller the retry's nil,
// which is close(2) claiming a durability the flush just failed to give it.
func TestBarrierFlushFailureIsReportedAndNotRetried(t *testing.T) {
	j := openTestDurability(t, DurabilityBarrier)
	first := stage(t, j, "a.txt", []byte("a"))
	second := stage(t, j, "b.txt", []byte("b"))

	boom := errors.New("objects flush failed")
	j.flushFault = func() error { return boom }
	defer func() { j.flushFault = nil }()

	batch := []commitReq{
		{u: first, done: make(chan error, 1)},
		{u: second, done: make(chan error, 1)},
	}
	txsBefore := j.writeTxs.Load()
	j.committer.commitBatch(batch)

	for i, r := range batch {
		if err := <-r.done; !errors.Is(err, boom) {
			t.Fatalf("row %d was told the commit succeeded (%v); the objects flush that covers it failed", i, err)
		}
	}
	if txs := j.writeTxs.Load() - txsBefore; txs != 1 {
		t.Fatalf("the batch ran %d write transactions, want 1: the committed rows were re-inserted after the flush failed", txs)
	}

	// The rows are committed, which is the whole reason re-inserting them is
	// unsafe: they are live work the upload queue owns from here on.
	for _, u := range []Upload{first, second} {
		got, err := j.Get(context.Background(), u.ID)
		if err != nil {
			t.Fatalf("committed row %s is gone after a failed flush: %v", u.ID, err)
		}
		if got.State != StatePending {
			t.Fatalf("row %s is in state %q, want %q", u.ID, got.State, StatePending)
		}
	}
}

// A transaction that fails still gets the row-by-row retry: one bad row must
// not sink the rest of the batch.
func TestBatchTransactionFailureStillRetriesRowByRow(t *testing.T) {
	j := openTestDurability(t, DurabilityBarrier)
	good := stage(t, j, "good.txt", []byte("good"))
	// A delete row that names a node is refused by insertUploadTx, so this one
	// row fails the whole batch's transaction.
	bad := Upload{ID: NewID(), Remote: "ali", Kind: KindDelete, RemoteID: "r1", Ino: 7}

	batch := []commitReq{
		{u: good, done: make(chan error, 1)},
		{u: bad, done: make(chan error, 1)},
	}
	j.committer.commitBatch(batch)

	if err := <-batch[0].done; err != nil {
		t.Fatalf("a good row was sunk by its neighbour: %v", err)
	}
	if err := <-batch[1].done; err == nil {
		t.Fatal("the invalid row was reported as committed")
	}
	if _, err := j.Get(context.Background(), good.ID); err != nil {
		t.Fatalf("the good row was not committed: %v", err)
	}
}
