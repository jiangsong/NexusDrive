package journal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestCheckpointTruncatesTheWriteAheadLog: a busy queue writes far more to
// the WAL than the database holds — a row per commit, then per claim, per
// part, per success — and a WAL that is only ever appended to grows past
// the database many times over. After Checkpoint it is back under the
// limit, and the rows are all still there.
func TestCheckpointTruncatesTheWriteAheadLog(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx := context.Background()
	if err := j.Commit(ctx, Upload{ID: NewID(), Kind: KindDelete, Remote: "r", RemoteParentID: "dir", Name: "first", RemoteID: "id-first"}); err != nil {
		t.Fatal(err)
	}
	// A reader that never finishes stands in for the workers, whose queries
	// overlap without a gap: while its snapshot is in use the automatic
	// checkpoints cannot reset the log, and the churn below is appended to
	// it — the way the log grew to a gigabyte beside a 15 MB database.
	reader, err := j.db.QueryContext(ctx, `SELECT id FROM uploads`)
	if err != nil {
		t.Fatal(err)
	}
	if !reader.Next() {
		t.Fatal("no first row")
	}
	const rows = 3000
	var ids []string
	for i := 0; i < rows; i++ {
		u := Upload{ID: NewID(), Kind: KindDelete, Remote: "r", RemoteParentID: "dir", Name: fmt.Sprintf("f%d", i), RemoteID: fmt.Sprintf("id%d", i),
			LastError: string(make([]byte, 4096))}
		if err := j.Commit(ctx, u); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, u.ID)
	}
	for _, id := range ids {
		if err := j.Retry(ctx, id, fmt.Errorf("%s", string(make([]byte, 4096))), 0); err != nil {
			t.Fatal(err)
		}
	}
	wal := filepath.Join(dir, "journal.db-wal")
	before, err := os.Stat(wal)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() < 8<<20 {
		t.Fatalf("the WAL only reached %d bytes under a pinned reader", before.Size())
	}
	// With the reader still on its snapshot the checkpoint cannot reset the
	// log and says so, rather than pretending.
	if truncated, err := j.Checkpoint(ctx); err != nil || truncated {
		t.Fatalf("checkpoint under a reader: truncated=%v err=%v", truncated, err)
	}
	reader.Close()
	truncated, err := j.Checkpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("checkpoint reported readers in the way with none open")
	}
	after, err := os.Stat(wal)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() > walSizeLimit || after.Size() >= before.Size() {
		t.Fatalf("WAL is %d bytes after the checkpoint (was %d, limit %d)", after.Size(), before.Size(), walSizeLimit)
	}
	pending, err := j.Pending(ctx)
	if err != nil || len(pending) != rows+1 {
		t.Fatalf("rows after the checkpoint: %d %v", len(pending), err)
	}
}
