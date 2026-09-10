package meta

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// A change feed that only updates an entry's attributes removes no name from
// its parent, so an older snapshot of that parent is still a truthful listing.
// Refusing it — which the hard fence did — is what made an ordinary read fail
// while the feed was applying a backlog. It is published, the directory is
// left incomplete, and the attributes the feed wrote survive: a snapshot that
// began before the write must not put the old values back.
func TestARemoteAttributeUpdatePublishesThroughAConcurrentListing(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	entry, err := s.Upsert(ctx, file(RootIno, "entry", 1))
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.Get(ctx, entry.Ino)
	if err != nil {
		t.Fatal(err)
	}

	listing := beginListingTest(t, s, RootIno)
	if err := listing.Append(ctx, []Node{old}); err != nil {
		t.Fatal(err)
	}

	updated := old
	updated.Version, updated.Size = "v2", 42
	if _, applied, err := s.ApplyRemoteNode(ctx, old, &updated, nil); err != nil || !applied {
		t.Fatalf("attribute update: applied=%v err=%v", applied, err)
	}

	// No protect predicate: the guarantee has to hold at this layer, not by
	// agreement with whichever caller happens to pass one.
	if err := listing.Commit(ctx, time.Minute, nil); err != nil {
		t.Fatalf("an attribute update refused a concurrent listing: %v", err)
	}
	if !listing.PublishedStale() {
		t.Fatal("the directory was published as complete although a change landed mid-listing")
	}
	got, err := s.Lookup(ctx, RootIno, "entry")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "v2" || got.Size != 42 {
		t.Fatalf("the older listing overwrote the change feed's attributes: version=%q size=%d", got.Version, got.Size)
	}
}

// A listing that started after the update is newer than it, so it owns the
// answer: the protection must not become permanent.
func TestAListingStartedAfterTheUpdateStillOwnsTheEntry(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	entry, err := s.Upsert(ctx, file(RootIno, "entry", 1))
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.Get(ctx, entry.Ino)
	if err != nil {
		t.Fatal(err)
	}
	// One listing has to have run before, or there is no generation row and
	// the ordering being tested does not exist yet.
	first := beginListingTest(t, s, RootIno)
	if err := first.Append(ctx, []Node{old}); err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(ctx, time.Minute, nil); err != nil {
		t.Fatal(err)
	}

	updated := old
	updated.Version, updated.Size = "v2", 42
	if _, applied, err := s.ApplyRemoteNode(ctx, old, &updated, nil); err != nil || !applied {
		t.Fatalf("attribute update: applied=%v err=%v", applied, err)
	}

	current, err := s.Lookup(ctx, RootIno, "entry")
	if err != nil {
		t.Fatal(err)
	}
	later := beginListingTest(t, s, RootIno)
	backend := current
	backend.Version, backend.Size = "v3", 99
	if err := later.Append(ctx, []Node{backend}); err != nil {
		t.Fatal(err)
	}
	if err := later.Commit(ctx, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Lookup(ctx, RootIno, "entry")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "v3" || got.Size != 99 {
		t.Fatalf("a listing that began after the update did not win: version=%q size=%d", got.Version, got.Size)
	}
}

// An existing database gains applied_gen at zero, which means "no change feed
// has written this node", so every listing still owns every entry it lists.
func TestAppliedGenerationMigratesAnExistingDatabase(t *testing.T) {
	p := filepath.Join(t.TempDir(), "v10.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:10] {
		if _, err := db.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version=10;
INSERT INTO nodes(ino,parent_ino,name,kind,size) VALUES(1,1,'',1,0),(2,1,'kept',0,5);
INSERT INTO directory_refresh_generation(ino,generation,stale_generation) VALUES(1,7,0);`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var applied int64
	if err := s.DB().QueryRow(`SELECT applied_gen FROM nodes WHERE ino=2`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatalf("migrated node carries applied_gen=%d, want 0", applied)
	}
	// A listing over the migrated tree still updates what it lists.
	ctx := context.Background()
	current, err := s.Get(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	listing := beginListingTest(t, s, RootIno)
	current.Version, current.Size = "fresh", 11
	if err := listing.Append(ctx, []Node{current}); err != nil {
		t.Fatal(err)
	}
	if err := listing.Commit(ctx, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "fresh" || got.Size != 11 {
		t.Fatalf("migrated node was not updated by a listing: version=%q size=%d", got.Version, got.Size)
	}
}
