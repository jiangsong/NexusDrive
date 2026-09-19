package journal

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mkdirRow is a queued directory creation: no blob, the parent it lands in,
// and the name to give it.
func mkdirRow(parent, name string, ino uint64) Upload {
	return Upload{ID: NewID(), Kind: KindMkdir, Remote: "ali", RemoteParentID: parent, Name: name, Ino: ino}
}

// TestMkdirRowsSurviveRecoverWithoutBlob: a directory creation carries no
// bytes, so recovery must not dead-letter it for the blob it never had.
func TestMkdirRowsSurviveRecoverWithoutBlob(t *testing.T) {
	j, _, dir := openTest(t)
	ctx := context.Background()
	u := mkdirRow("root", "d", 7)
	if err := j.Commit(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Claim(ctx, "ali", 1); err != nil {
		t.Fatal(err)
	}
	j.Close()
	j2, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	rec, err := j2.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Lost) != 0 {
		t.Fatalf("a directory creation was treated as a lost blob: %+v", rec)
	}
	got, err := j2.Get(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StatePending || got.Kind != KindMkdir || got.BlobPath != "" {
		t.Fatalf("after restart: %+v", got)
	}
}

// TestClaimSkipsRowsUnderPendingDirectory: a row that lands in a directory
// the backend does not have yet is not due, whatever its retry time says —
// sending it would address a parent id the backend has never seen.
func TestClaimSkipsRowsUnderPendingDirectory(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	parent := mkdirRow("root", "d", 7)
	child := mkdirRow(LocalIDPrefix+parent.ID, "sub", 8)
	file := stage(t, j, "f", []byte("x"))
	file.RemoteParentID = LocalIDPrefix + parent.ID
	for _, u := range []Upload{parent, child, file} {
		if err := j.Commit(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := j.Claim(ctx, "ali", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != parent.ID {
		t.Fatalf("claimed rows under a pending directory: %+v", claimed)
	}
	if err := j.RetargetChildren(ctx, LocalIDPrefix+parent.ID, "real-d"); err != nil {
		t.Fatal(err)
	}
	claimed, err = j.Claim(ctx, "ali", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 {
		t.Fatalf("after the parent landed: %+v", claimed)
	}
	for _, u := range claimed {
		if u.RemoteParentID != "real-d" {
			t.Fatalf("child still addresses the pending parent: %+v", u)
		}
	}
}

// TestRetargetChildrenFollowsAllStates: every row that is not finished
// follows its parent to the real id — a cancelled one too, or resuming it
// later addresses a parent nobody has.
func TestRetargetChildrenFollowsAllStates(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	parent := mkdirRow("root", "d", 7)
	pending := stage(t, j, "a", []byte("a"))
	pending.RemoteParentID = LocalIDPrefix + parent.ID
	cancelled := stage(t, j, "b", []byte("b"))
	cancelled.RemoteParentID = LocalIDPrefix + parent.ID
	done := stage(t, j, "c", []byte("c"))
	done.RemoteParentID = LocalIDPrefix + parent.ID
	for _, u := range []Upload{parent, pending, cancelled, done} {
		if err := j.Commit(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := j.db.Exec(`UPDATE uploads SET state = ? WHERE id = ?`, string(StateCancelled), cancelled.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := j.db.Exec(`UPDATE uploads SET state = ? WHERE id = ?`, string(StateDone), done.ID); err != nil {
		t.Fatal(err)
	}
	if err := j.RetargetChildren(ctx, LocalIDPrefix+parent.ID, "real-d"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{pending.ID, cancelled.ID} {
		got, _ := j.Get(ctx, id)
		if got.RemoteParentID != "real-d" {
			t.Fatalf("unfinished child not retargeted: %+v", got)
		}
	}
	got, _ := j.Get(ctx, done.ID)
	if got.RemoteParentID != LocalIDPrefix+parent.ID {
		t.Fatalf("finished child is history and must be left alone: %+v", got)
	}
}

// TestSchemaV13AddsKindDefaultFile: a queue written before directory
// creations existed reads every row as a file, and a read-only opener of an
// old database does the same without a migration.
func TestSchemaV13AddsKindDefaultFile(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	old := journalSchema
	old = strings.Replace(old, ",\n  kind             TEXT NOT NULL DEFAULT 'file'", "", 1)
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO uploads (id, remote, remote_parent_id, name, blob_path, size, state, created_at)
		VALUES ('u1', 'ali', 'root', 'f', '/nowhere', 1, 'pending', ?)`, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 13`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := ro.Get(context.Background(), "u1")
	ro.Close()
	if err != nil || u.Kind != KindFile {
		t.Fatalf("read-only open of a v13 queue: %+v %v", u, err)
	}
	j, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	u, err = j.Get(context.Background(), "u1")
	if err != nil || u.Kind != KindFile {
		t.Fatalf("migrated v13 queue: %+v %v", u, err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("row lost in migration")
	}
}

// TestStatsCountRowsBlockedByADeadDirectory: rows queued under a directory
// whose creation has been given up on can never run. They stay pending —
// their bytes are the only copy — but the count says why the queue is not
// moving, transitively through pending directories in between.
func TestStatsCountRowsBlockedByADeadDirectory(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	dead := mkdirRow("root", "dead", 7)
	mid := mkdirRow(LocalIDPrefix+dead.ID, "mid", 8)
	leaf := stage(t, j, "leaf", []byte("x"))
	leaf.RemoteParentID = LocalIDPrefix + mid.ID
	orphan := stage(t, j, "orphan", []byte("y"))
	orphan.RemoteParentID = LocalIDPrefix + "no-such-row"
	fine := stage(t, j, "fine", []byte("z"))
	for _, u := range []Upload{dead, mid, leaf, orphan, fine} {
		if err := j.Commit(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	st, err := j.Stats(ctx)
	if err != nil || st.Blocked != 1 {
		t.Fatalf("before the directory dies: %+v %v (want only the orphan blocked)", st, err)
	}
	if err := j.Fail(ctx, dead.ID, errors.New("refused")); err != nil {
		t.Fatal(err)
	}
	st, err = j.Stats(ctx)
	if err != nil || st.Pending != 4 || st.Blocked != 3 || st.Dead != 1 {
		t.Fatalf("after the directory dies: %+v %v", st, err)
	}
	// Dropping the dead directory while rows still wait on it would strand
	// them silently; it is refused.
	if err := j.Drop(ctx, dead.ID); !errors.Is(err, ErrChildrenQueued) {
		t.Fatalf("drop of a directory with queued children: %v", err)
	}
	if _, err := j.Get(ctx, dead.ID); err != nil {
		t.Fatalf("refused drop removed the row: %v", err)
	}
}

// TestMkdirRowCannotBeCancelled: a cancelled file keeps its bytes for later
// resume; a cancelled directory creation would leave the directory local
// forever with nothing to resume. Cancellation is refused.
func TestMkdirRowCannotBeCancelled(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	u := mkdirRow("root", "d", 7)
	if err := j.Commit(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RequestCancel(ctx, u.ID); !errors.Is(err, ErrCannotCancel) {
		t.Fatalf("cancel of a directory creation: %v", err)
	}
	if got, _ := j.Get(ctx, u.ID); got.State != StatePending {
		t.Fatalf("row after refused cancel: %+v", got)
	}
}
