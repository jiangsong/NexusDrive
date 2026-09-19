package upload

import (
	"context"
	"errors"
	"testing"
	"time"

	"cloudfs/internal/journal"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

// queueMkdir commits a queued directory creation and hands back the row.
func (f *fixture) queueMkdir(t *testing.T, parent, name string) journal.Upload {
	t.Helper()
	f.inos++
	u := journal.Upload{ID: journal.NewID(), Kind: journal.KindMkdir, Remote: "ali", RemoteParentID: parent, Name: name, Ino: 100 + f.inos}
	if err := f.j.Commit(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

// TestMkdirRowCreatesTheDirectory: a queued directory creation is one Mkdir
// on the backend — no session, no parts, no blob — and reports the entry the
// backend made so the tree can adopt its id.
func TestMkdirRowCreatesTheDirectory(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.queueMkdir(t, fakeprovider.RootID, "d")
	if n, err := f.up.DrainAll(ctx); err != nil || n != 1 {
		t.Fatalf("drain = %d, %v", n, err)
	}
	if f.fake.Calls("Mkdir") != 1 || f.fake.Calls("BeginUpload") != 0 || f.fake.Calls("PutFile") != 0 {
		t.Fatalf("mkdir=%d begin=%d put=%d", f.fake.Calls("Mkdir"), f.fake.Calls("BeginUpload"), f.fake.Calls("PutFile"))
	}
	if len(f.ok) != 1 || f.ok[0].UploadID != u.ID || f.ok[0].Entry.Kind != provider.KindDir || f.ok[0].Entry.ID == "" || f.ok[0].Entry.Name != "d" {
		t.Fatalf("result = %+v", f.ok)
	}
	if row, err := f.j.Get(ctx, u.ID); err != nil || row.State != journal.StateDone {
		t.Fatalf("row after success: %+v %v", row, err)
	}
}

// TestMkdirRowExistsAdoptsByListing: the first attempt timed out after the
// backend had made the directory, so the retry hears "exists". That is a
// success whose entry the backend has to be asked for by name.
func TestMkdirRowExistsAdoptsByListing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	existing, err := f.fake.Mkdir(ctx, fakeprovider.RootID, "d")
	if err != nil {
		t.Fatal(err)
	}
	lists := f.fake.Calls("List")
	f.queueMkdir(t, fakeprovider.RootID, "d")
	if n, err := f.up.DrainAll(ctx); err != nil || n != 1 {
		t.Fatalf("drain = %d, %v", n, err)
	}
	if len(f.dead) != 0 {
		t.Fatalf("an existing directory dead-lettered the creation: %v", f.dead)
	}
	if len(f.ok) != 1 || f.ok[0].Entry.ID != existing.ID || !f.ok[0].Merged {
		t.Fatalf("result = %+v, want the existing entry %s marked merged", f.ok, existing.ID)
	}
	if got := f.fake.Calls("List") - lists; got != 1 {
		t.Fatalf("existing directory found with %d listings, want 1", got)
	}
}

// TestMkdirRowExistsAsAFileIsPermanent: the name is taken by a file. No retry
// changes that; it goes to the dead letter for a person to look at.
func TestMkdirRowExistsAsAFileIsPermanent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.fake.Seed("d", []byte("a file"))
	f.queueMkdir(t, fakeprovider.RootID, "d")
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.dead) != 1 || !errors.Is(f.dead[0], provider.ErrExists) {
		t.Fatalf("dead = %v", f.dead)
	}
	if len(f.ok) != 0 {
		t.Fatalf("a file was adopted as the directory: %+v", f.ok)
	}
}

// TestTombstonedMergedMkdirDoesNotDelete: the directory was removed locally
// while its creation was in flight, and the creation found the name already
// there. That directory may be someone else's, with their files in it; the
// tombstone must not delete it. A directory this row made itself is deleted,
// as a tombstoned file is.
func TestTombstonedMergedMkdirDoesNotDelete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	existing, err := f.fake.Mkdir(ctx, fakeprovider.RootID, "theirs")
	if err != nil {
		t.Fatal(err)
	}
	f.fake.Seed("theirs/keep.txt", []byte("x"))
	merged := f.queueMkdir(t, fakeprovider.RootID, "theirs")
	own := f.queueMkdir(t, fakeprovider.RootID, "mine")
	claimed, err := f.j.Claim(ctx, "ali", 2)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	for _, u := range []journal.Upload{merged, own} {
		if err := f.j.Tombstone(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range claimed {
		f.up.process(ctx, c)
	}
	entries, _, _ := f.fake.List(ctx, fakeprovider.RootID, "")
	names := map[string]string{}
	for _, e := range entries {
		names[e.Name] = e.ID
	}
	if names["theirs"] != existing.ID {
		t.Fatalf("a tombstoned creation deleted a directory it did not make: %v", names)
	}
	if _, ok := names["mine"]; ok {
		t.Fatalf("a tombstoned creation left its own directory behind: %v", names)
	}
	if len(f.ok) != 0 {
		t.Fatalf("tombstoned creations reported success: %+v", f.ok)
	}
}

// TestFlushReportsRowsBlockedByADeadDirectory: a file queued under a
// directory whose creation was given up on stays pending, and a flush that
// waited for it would wait until its deadline. The dead directory is what
// the flush reports, at once.
func TestFlushReportsRowsBlockedByADeadDirectory(t *testing.T) {
	f := liveFixture(t, nil)
	ctx := context.Background()
	dir := f.queueMkdir(t, fakeprovider.RootID, "d")
	child := f.queue(t, "under.txt", []byte("keep"), "")
	if err := f.j.RetargetParent(ctx, child.ID, journal.LocalIDPrefix+dir.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.j.Fail(ctx, dir.ID, errors.New("denied")); err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := f.up.Flush(wait)
	if !errors.Is(err, ErrDeadLetters) || st.Dead != 1 || st.Blocked != 1 || st.Pending != 1 {
		t.Fatalf("flush = %+v, %v", st, err)
	}
}
