package vfs

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// One batch of remote changes costs one scan of the upload queue, not one per
// changed node.
//
// The set of inodes with bytes still in the journal protects a locally written
// file from a change event that would overwrite or delete it, so every node a
// batch touches has to consult it. Building it per node means a SELECT over
// the whole queue, and a map the size of it, for each event: a feed that
// delivers hundreds of changes while a large copy is queued pays hundreds of
// scans for one answer that does not change within the batch.
func TestDeltaBatchScansTheUploadQueueOnce(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	const files = 16
	for i := 0; i < files; i++ {
		e.fake.Seed(fmt.Sprintf("file%02d", i), []byte("old"))
	}
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetCursor(ctx, "ali", e.fake.Cursor()); err != nil {
		t.Fatal(err)
	}
	// A queued local write, so the set the batch consults is not empty.
	if _, err := e.fs.WriteFile(ctx, "/ali/local.txt", []byte("mine"), false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < files; i++ {
		e.fake.Seed(fmt.Sprintf("file%02d", i), []byte("changed remotely"))
	}

	r := NewRefresher(e.fs, time.Hour)
	before := e.j.QueuedInoScans()
	applied, err := r.PollOnce(ctx, e.mount())
	if err != nil {
		t.Fatal(err)
	}
	if applied == 0 {
		t.Fatal("the poll applied nothing; the fixture did not produce a batch")
	}
	if scans := e.j.QueuedInoScans() - before; scans != 1 {
		t.Fatalf("a batch of %d applied changes scanned the upload queue %d times, want 1", applied, scans)
	}
}
