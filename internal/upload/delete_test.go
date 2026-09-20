package upload

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/journal"
	"cloudfs/test/fakeprovider"
)

// queueDelete commits a queued removal of a backend entry and hands back
// the row. kind is KindDelete or KindRmdir.
func (f *fixture) queueDelete(t *testing.T, kind journal.Kind, parent, name, id string) journal.Upload {
	t.Helper()
	u := journal.Upload{ID: journal.NewID(), Kind: kind, Remote: "ali", RemoteParentID: parent, Name: name, RemoteID: id}
	if err := f.j.Commit(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

// TestDeleteRowRemovesTheFile: a queued delete is one Delete on the backend
// — no listing, no session, no blob — and the row is done afterwards.
func TestDeleteRowRemovesTheFile(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.fake.Seed("a.txt", []byte("bytes"))
	lists := f.fake.Calls("List")
	u := f.queueDelete(t, journal.KindDelete, fakeprovider.RootID, "a.txt", e.ID)
	if n, err := f.up.DrainAll(ctx); err != nil || n != 1 {
		t.Fatalf("drain = %d, %v", n, err)
	}
	if f.fake.Calls("Delete") != 1 || f.fake.Calls("List") != lists {
		t.Fatalf("delete=%d lists=%d", f.fake.Calls("Delete"), f.fake.Calls("List")-lists)
	}
	if _, ok := f.fake.IDOf("a.txt"); ok {
		t.Fatalf("file still on the backend: %v", f.fake.Tree())
	}
	if len(f.ok) != 1 || f.ok[0].UploadID != u.ID {
		t.Fatalf("results = %+v", f.ok)
	}
	if row, err := f.j.Get(ctx, u.ID); err != nil || row.State != journal.StateDone {
		t.Fatalf("row after success: %+v %v", row, err)
	}
}

// TestDeleteRowOfAGoneFileIsDone: the backend no longer has the id —
// another client removed it, or the first attempt's reply was lost. There
// is nothing left to do, and nothing for an operator to look at.
func TestDeleteRowOfAGoneFileIsDone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.queueDelete(t, journal.KindDelete, fakeprovider.RootID, "gone.txt", "no-such-id")
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.dead) != 0 {
		t.Fatalf("dead-lettered: %v", f.dead)
	}
	if row, err := f.j.Get(ctx, u.ID); err != nil || row.State != journal.StateDone {
		t.Fatalf("row: %+v %v", row, err)
	}
}

// TestRmdirRowRefusesANonEmptyDirectory: the directory was empty here when
// it was removed, but the backend has something in it now — another
// client's file. It is not this row's to remove: the row dead-letters and
// the directory stays.
func TestRmdirRowRefusesANonEmptyDirectory(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, err := f.fake.Mkdir(ctx, fakeprovider.RootID, "d")
	if err != nil {
		t.Fatal(err)
	}
	f.fake.Seed("d/theirs.txt", []byte("not ours"))
	f.queueDelete(t, journal.KindRmdir, fakeprovider.RootID, "d", d.ID)
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if f.fake.Calls("Delete") != 0 {
		t.Fatalf("the backend was asked to delete a directory with someone else's file in it")
	}
	if len(f.dead) != 1 || !strings.Contains(f.dead[0].Error(), "not empty") {
		t.Fatalf("dead = %v, want the directory reported as not empty", f.dead)
	}
	if _, ok := f.fake.IDOf("d/theirs.txt"); !ok {
		t.Fatalf("their file is gone: %v", f.fake.Tree())
	}
}

// TestRmdirRowRemovesAnEmptyDirectory: one listing to confirm, one delete.
func TestRmdirRowRemovesAnEmptyDirectory(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, err := f.fake.Mkdir(ctx, fakeprovider.RootID, "d")
	if err != nil {
		t.Fatal(err)
	}
	lists := f.fake.Calls("List")
	f.queueDelete(t, journal.KindRmdir, fakeprovider.RootID, "d", d.ID)
	if n, err := f.up.DrainAll(ctx); err != nil || n != 1 {
		t.Fatalf("drain = %d, %v", n, err)
	}
	if f.fake.Calls("Delete") != 1 || f.fake.Calls("List")-lists != 1 {
		t.Fatalf("delete=%d lists=%d", f.fake.Calls("Delete"), f.fake.Calls("List")-lists)
	}
	if _, ok := f.fake.IDOf("d"); ok {
		t.Fatalf("directory still on the backend: %v", f.fake.Tree())
	}
}

// TestWriteWaitsForOlderDeleteOfTheSameName: rm then cp of the same name.
// Drive puts a file by name and patches the one already there, so sending
// the write first would put the new bytes on the file the delete then
// removes. The delete goes first; the write is not even claimable until it
// has.
func TestWriteWaitsForOlderDeleteOfTheSameName(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	old := f.fake.Seed("a.txt", []byte("old"))
	del := f.queueDelete(t, journal.KindDelete, fakeprovider.RootID, "a.txt", old.ID)
	f.queue(t, "a.txt", []byte("new"), "")
	claimed, err := f.j.Claim(ctx, "ali", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != del.ID {
		t.Fatalf("claimable with the delete out: %+v, want only the delete", claimed)
	}
	f.up.RunOne(ctx, claimed[0])
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	got, ok := f.fake.Content("a.txt")
	if !ok || string(got) != "new" {
		t.Fatalf("backend has %q, %v; want the new content", got, ok)
	}
	if f.fake.Calls("Delete") != 1 || f.fake.Calls("PutFile")+f.fake.Calls("BeginUpload") != 1 {
		t.Fatalf("delete=%d put=%d begin=%d", f.fake.Calls("Delete"), f.fake.Calls("PutFile"), f.fake.Calls("BeginUpload"))
	}
}

// TestRmdirWaitsForDeletesQueuedUnderIt: rm -rf queues the children before
// the directory. The directory's row is not claimable while any of them is
// still out; once they are gone it is, and the listing it confirms with is
// empty.
func TestRmdirWaitsForDeletesQueuedUnderIt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, err := f.fake.Mkdir(ctx, fakeprovider.RootID, "d")
	if err != nil {
		t.Fatal(err)
	}
	child := f.fake.Seed("d/f.txt", []byte("x"))
	cdel := f.queueDelete(t, journal.KindDelete, d.ID, "f.txt", child.ID)
	ddel := f.queueDelete(t, journal.KindRmdir, fakeprovider.RootID, "d", d.ID)
	claimed, err := f.j.Claim(ctx, "ali", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != cdel.ID {
		t.Fatalf("claimable: %+v, want only the child's delete", claimed)
	}
	if _, err := f.j.ClaimID(ctx, ddel.ID); err == nil {
		t.Fatalf("the directory's delete was claimable before its child's ran")
	}
	f.up.RunOne(ctx, claimed[0])
	if n, err := f.up.DrainAll(ctx); err != nil || n != 1 {
		t.Fatalf("drain after the child: %d %v", n, err)
	}
	if len(f.dead) != 0 {
		t.Fatalf("dead: %v", f.dead)
	}
	if _, ok := f.fake.IDOf("d"); ok {
		t.Fatalf("directory still on the backend: %v", f.fake.Tree())
	}
}

// TestConflictCheckListsAParentOncePerBatch: every rewrite lists its parent
// to see whether the remote moved. A copy of a tree rewrites whole
// directories at a time, and one listing serves all of them: the
// siblings' versions are what the check needs, and they do not change
// because a neighbour was sent.
func TestConflictCheckListsAParentOncePerBatch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const n = 8
	var versions []string
	for i := 0; i < n; i++ {
		e := f.fake.Seed("f"+string(rune('a'+i))+".txt", []byte("old"))
		versions = append(versions, e.Version)
	}
	lists := f.fake.Calls("List")
	for i := 0; i < n; i++ {
		f.queue(t, "f"+string(rune('a'+i))+".txt", []byte("new"), versions[i])
	}
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	pages := (n + fakeprovider.ListPageSize - 1) / fakeprovider.ListPageSize
	if got := f.fake.Calls("List") - lists; got != pages {
		t.Fatalf("%d rewrites in one directory listed it %d times, want one listing (%d pages)", n, got, pages)
	}
	for i := 0; i < n; i++ {
		if len(f.ok) <= i || f.ok[i].ConflictName != "" {
			t.Fatalf("result %d = %+v: our own rewrite reported as a conflict", i, f.ok)
		}
	}
	if f.fake.Calls("PutFile")+f.fake.Calls("BeginUpload") != n {
		t.Fatalf("put=%d begin=%d", f.fake.Calls("PutFile"), f.fake.Calls("BeginUpload"))
	}
}

// TestConflictCheckSeesOurOwnPreviousUpload: two rewrites of one file. The
// first lands and changes the version; the second must compare against
// that, not against the listing taken before the first went out — or our
// own write would be a conflict copy.
func TestConflictCheckSeesOurOwnPreviousUpload(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.fake.Seed("a.txt", []byte("v0"))
	remoteVersion := e.Version
	f.up.opt.Hooks.RemoteVersion = func(context.Context, journal.Upload) (string, bool) { return remoteVersion, true }
	f.queue(t, "a.txt", []byte("v1"), e.Version)
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.ok) != 1 || f.ok[0].ConflictName != "" {
		t.Fatalf("first: %+v", f.ok)
	}
	remoteVersion = f.ok[0].Entry.Version // what OnSuccess records in the tree
	// Someone else edits meanwhile: that one must still be caught.
	f.queue(t, "a.txt", []byte("v2"), remoteVersion)
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.ok) != 2 || f.ok[1].ConflictName != "" {
		t.Fatalf("second: %+v: our own landed version mistaken for someone else's", f.ok)
	}
	remoteVersion = f.ok[1].Entry.Version
	f.fake.Seed("a.txt", []byte("theirs"))
	// The remembered listing is good for a moment only; past that the
	// check looks again and finds their edit.
	f.clk.advance(listingMemoTTL + time.Second)
	f.queue(t, "a.txt", []byte("v3"), remoteVersion)
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.ok) != 3 || f.ok[2].ConflictName == "" {
		t.Fatalf("third: %+v: a real remote edit went unnoticed", f.ok)
	}
}
