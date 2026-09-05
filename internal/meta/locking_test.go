package meta

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/provider"
)

// TestWritesWaitForAnotherProcessInsteadOfFailing: `cloudfs status` opens the
// same store beside the daemon. A deferred transaction that reads first and
// writes second fails with SQLITE_BUSY_SNAPSHOT the moment the other process
// commits in between — the busy handler never retries that — and the daemon
// turned it into EIO on close(). Write transactions must take the lock up
// front so they queue behind the other writer instead.
func TestWritesWaitForAnotherProcessInsteadOfFailing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	a, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	if _, err := a.Upsert(ctx, Node{ParentIno: RootIno, Name: "f", Kind: provider.KindFile}); err != nil {
		t.Fatal(err)
	}

	// "Process" a holds a write transaction open; b tries to update the
	// same row meanwhile.
	release := make(chan struct{})
	held := make(chan struct{})
	go func() {
		_ = a.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`UPDATE nodes SET size = 1 WHERE parent_ino = ? AND name = 'f'`, RootIno); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	done := make(chan error, 1)
	go func() {
		_, err := b.Upsert(ctx, Node{ParentIno: RootIno, Name: "f", Kind: provider.KindFile, Size: 2})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a write beside another process's transaction failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the write never completed")
	}
	n, err := b.Lookup(ctx, RootIno, "f")
	if err != nil {
		t.Fatal(err)
	}
	if n.Size != 2 {
		t.Fatalf("size = %d, want the later write (2) to have landed", n.Size)
	}
}

// TestRefreshQueuesOnlyChangedNamesForTheIndex: a directory refresh used to
// re-queue every child for the FTS index, and the indexer deleted and
// re-inserted each one — a full scan of the FTS table per delete. Under
// the race detector a stress test with refreshes every 3 ms never
// finished; in production it was CPU spent on every TTL refresh. Only
// names the index does not already hold are queued.
func TestRefreshQueuesOnlyChangedNamesForTheIndex(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	listing := []Node{
		{Name: "a.txt", Kind: provider.KindFile, Remote: "r", RemoteID: "a"},
		{Name: "b.txt", Kind: provider.KindFile, Remote: "r", RemoteID: "b"},
	}
	if err := s.PutDir(ctx, RootIno, listing, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	pending := func() int {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM name_index_pending`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := pending(); got != 2 {
		t.Fatalf("first listing queued %d names, want 2", got)
	}
	if err := s.FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pending(); got != 0 {
		t.Fatalf("%d names still pending after the indexer ran", got)
	}
	// The same listing again: nothing to index.
	if err := s.PutDir(ctx, RootIno, listing, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	if got := pending(); got != 0 {
		t.Fatalf("an unchanged listing queued %d names for the index, want 0", got)
	}
	// A new name is queued; a renamed one is re-indexed under its new name.
	listing = append(listing, Node{Name: "c.txt", Kind: provider.KindFile, Remote: "r", RemoteID: "c"})
	if err := s.PutDir(ctx, RootIno, listing, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	if got := pending(); got != 1 {
		t.Fatalf("one new name queued %d, want 1", got)
	}
	if err := s.FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := s.Lookup(ctx, RootIno, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, a.Ino, RootIno, "renamed.txt"); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(ctx, "renamed", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Ino != a.Ino {
		t.Fatalf("search after rename: %+v", hits)
	}
	if hits, _ := s.Search(ctx, "a.txt", 10); len(hits) != 0 {
		t.Fatalf("the old name is still in the index: %+v", hits)
	}
}

// TestUnchangedListingStillRefreshesFetchTime: an unchanged entry is no
// longer rewritten on refresh, but its fetch time must still move, or the
// attribute TTL would expire on entries the listing just confirmed.
func TestUnchangedListingStillRefreshesFetchTime(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	listing := []Node{{Name: "a.txt", Kind: provider.KindFile, Remote: "r", RemoteID: "a", Version: "v1", Size: 3}}
	if err := s.PutDir(ctx, RootIno, listing, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	first, err := s.Lookup(ctx, RootIno, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	c.advance(90 * time.Second)
	if err := s.PutDir(ctx, RootIno, listing, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	again, err := s.Lookup(ctx, RootIno, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if again.Ino != first.Ino {
		t.Fatalf("the inode changed across an unchanged refresh: %d -> %d", first.Ino, again.Ino)
	}
	if !again.FetchedAt.After(first.FetchedAt) {
		t.Fatalf("fetched_at did not move on an unchanged refresh: %v -> %v", first.FetchedAt, again.FetchedAt)
	}
	if again.Version != "v1" || again.Size != 3 {
		t.Fatalf("entry fields changed: %+v", again)
	}
}
