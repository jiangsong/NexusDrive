//go:build !windows

package fusefs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"cloudfs/internal/vfs"
)

// TestLookupAfterReaddirIsAnsweredFromTheListing is the cost of `ls -l` over a
// large directory.
//
// The kernel reads the directory and then asks about every entry in it. Where
// it offers CAP_READDIRPLUS it asks once, with readdirplus, and dirHandle
// answers each entry out of the listing it is holding (dir_lookup). Where it
// does not — macFUSE speaks FUSE 7.19 — it sends a separate LOOKUP per entry,
// and without CAP_PARALLEL_DIROPS those are serialised: 200 round trips, each
// one a metadata query, for a directory that was listed a millisecond ago.
//
// Either way the number that must stay near zero is `lookup`: entries reaching
// the VFS one at a time. Which counter carries them instead says which
// mechanism this kernel used.
func TestLookupAfterReaddirIsAnsweredFromTheListing(t *testing.T) {
	e := newMountWithTimeout(t, "", 30*time.Second)
	const files = 200
	for i := 0; i < files; i++ {
		e.fake.Seed(fmt.Sprintf("big/f%03d.txt", i), []byte("x"))
	}
	dir := filepath.Join(e.dir, "big")

	before := e.mnt.OpStats().Ops
	// The shape of `ls -l`: one readdir, then a stat of every entry.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != files {
		t.Fatalf("listed %d entries, want %d", len(ents), files)
	}
	for _, en := range ents {
		if _, err := os.Lstat(filepath.Join(dir, en.Name())); err != nil {
			t.Fatal(err)
		}
	}
	after := e.mnt.OpStats().Ops
	delta := map[string]int64{}
	for k, v := range after {
		if v-before[k] != 0 {
			delta[k] = v - before[k]
		}
	}
	t.Logf("cold ls -l over %d entries (readdirplus offered: %v): %v",
		files, e.mnt.ReaddirplusOffered(), delta)

	// A handful is the allowance for the directory itself and for a name the
	// kernel asks about outside the listing burst; 200 is the regression.
	if delta["lookup"] > 5 {
		t.Errorf("%d of %d entries reached the VFS as individual lookups; the listing was already in hand",
			delta["lookup"], files)
	}
	served := delta["dir_lookup"] + delta["entry_cache"]
	if served < files-5 {
		t.Errorf("only %d of %d entries were answered from the listing", served, files)
	}
	if e.mnt.ReaddirplusOffered() {
		if delta["entry_cache"] != 0 {
			t.Errorf("this kernel answers readdirplus; the compensating entry cache should be off, but served %d", delta["entry_cache"])
		}
	} else if delta["dir_lookup"] != 0 {
		t.Errorf("this kernel never sends readdirplus, so dirHandle.Lookup cannot have served %d entries", delta["dir_lookup"])
	}
}

// TestListingCacheDoesNotOutliveAChangeMadeThroughTheVFS is the safety side of
// answering a lookup from a listing. The entry cache sits below the kernel's
// own dentry cache; it must never be the layer that keeps a name alive after
// the VFS has retired it.
//
// The change is made through the VFS rather than through the mount on purpose:
// that is the path where the kernel does not know anything happened, so both
// caches depend entirely on the invalidation hook (vfs invalidateEntryFrom →
// Mount.InvalidateEntryFunc → Root.notifyEntry). If the entry cache were not
// wired into it, the kernel would drop its dentry, ask us, and be told the
// stale answer.
func TestListingCacheDoesNotOutliveAChangeMadeThroughTheVFS(t *testing.T) {
	e := newMountWithTimeout(t, "", 30*time.Second)
	e.fake.Seed("d/keep.txt", []byte("k"))
	e.fake.Seed("d/gone.txt", []byte("g"))
	dir := filepath.Join(e.dir, "d")

	names := func() []string {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, en := range ents {
			out = append(out, en.Name())
		}
		sort.Strings(out)
		return out
	}
	if got := names(); len(got) != 2 {
		t.Fatalf("initial listing = %v", got)
	}
	// Fill the entry cache the way `ls -l` does.
	for _, n := range names() {
		if _, err := os.Lstat(filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	// Every assertion about the cache below is judged at this instant, when
	// the listing is certainly fresh. That takes the TTL out of the test: an
	// entry that disappears does so because something retired it, not
	// because the clock moved on.
	fresh := time.Now()

	ctx := context.Background()
	at, err := e.fs.StatPath(ctx, "/d")
	if err != nil {
		t.Fatal(err)
	}
	cache := e.mnt.root.entries.Load()
	if cache != nil {
		if _, ok := cache.lookup(at.Ino, "gone.txt", fresh); !ok {
			t.Fatal("the listing was never cached, so this guards nothing")
		}
	}
	// Not through the mount: this is what an MCP or control-API write looks
	// like, and the only thing that retires the name is the hook.
	if err := e.fs.Remove(ctx, at.Ino, "gone.txt", false); err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Create(ctx, at.Ino, "new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Sync(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}

	// The notifications are sent from their own goroutines, so the mount
	// catches up rather than being instantaneous.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, gone := os.Lstat(filepath.Join(dir, "gone.txt"))
		_, added := os.Lstat(filepath.Join(dir, "new.txt"))
		if os.IsNotExist(gone) && added == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after a VFS-side change the mount still reports gone.txt=%v new.txt=%v", gone, added)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// And the file that did not change still reads.
	if _, err := os.Lstat(filepath.Join(dir, "keep.txt")); err != nil {
		t.Fatalf("an untouched entry was lost: %v", err)
	}
	if cache == nil {
		// This kernel answers readdirplus, so there is no second cache to
		// guard; the checks above are the whole story.
		return
	}
	// The removed name is gone from the listing cache as a fact, not as a
	// consequence of the TTL: judged at the instant the listing was still
	// fresh, it must no longer answer.
	for {
		if _, ok := cache.lookup(at.Ino, "gone.txt", fresh); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a name removed through the VFS still answers from the listing cache; the invalidation hook is not wired to it")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A content change reaches it the same way, through the inode rather
	// than the name: the size in a cached entry is an answer to a lookup.
	if _, ok := cache.lookup(at.Ino, "keep.txt", fresh); !ok {
		// The kernel may have asked about keep.txt again and refreshed it
		// out from under us; re-listing puts it back.
		names()
		if _, err := os.Lstat(filepath.Join(dir, "keep.txt")); err != nil {
			t.Fatal(err)
		}
	}
	keep, err := e.fs.StatPath(ctx, "/d/keep.txt")
	if err != nil {
		t.Fatal(err)
	}
	e.mnt.InvalidateFunc()(keep.Ino)
	for {
		if _, ok := cache.lookup(at.Ino, "keep.txt", fresh); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("an inode invalidated through the VFS still answers from the listing cache with its old attributes")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The unit tests below pin the cache's own rules without a mount, so they run
// on every platform whatever the kernel offers.

func attrs(n int, firstIno uint64) []vfs.Attr {
	out := make([]vfs.Attr, n)
	for i := range out {
		out[i] = vfs.Attr{Name: fmt.Sprintf("f%04d", i), Ino: firstIno + uint64(i), Size: int64(i)}
	}
	return out
}

func TestEntryCacheAnswersAndExpires(t *testing.T) {
	c := newEntryCache(time.Minute)
	now := time.Now()
	c.putListing(7, attrs(3, 100), now)

	if at, ok := c.lookup(7, "f0001", now); !ok || at.Ino != 101 {
		t.Fatalf("lookup of a listed name = %+v, %v", at, ok)
	}
	if _, ok := c.lookup(7, "absent", now); ok {
		t.Fatal("a name that was not in the listing must not be answered")
	}
	if _, ok := c.lookup(8, "f0001", now); ok {
		t.Fatal("a name must not be answered for the wrong directory")
	}
	if _, ok := c.lookup(7, "f0001", now.Add(entryCacheTTL+time.Millisecond)); ok {
		t.Fatal("an entry answered past its TTL")
	}
	if c.size() != 0 {
		t.Fatalf("the expired listing was not reclaimed: %d entries held", c.size())
	}
}

// The TTL never exceeds the kernel's own entry timeout: this layer is under
// that one, and a lookup must not be answered from here after the kernel would
// have come back to ask.
func TestEntryCacheTTLIsBoundedByTheKernelEntryTimeout(t *testing.T) {
	if got := newEntryCache(100 * time.Millisecond).ttl; got != 100*time.Millisecond {
		t.Errorf("ttl with a 100ms entry timeout = %v", got)
	}
	if got := newEntryCache(time.Hour).ttl; got != entryCacheTTL {
		t.Errorf("ttl with an hour-long entry timeout = %v, want the %v ceiling", got, entryCacheTTL)
	}
}

func TestEntryCacheDropsWhatChanged(t *testing.T) {
	c := newEntryCache(time.Minute)
	now := time.Now()
	c.putListing(7, attrs(3, 100), now)

	c.dropEntry(7, "f0000")
	if _, ok := c.lookup(7, "f0000", now); ok {
		t.Error("a dropped name was still answered")
	}
	if _, ok := c.lookup(7, "f0001", now); !ok {
		t.Error("dropping one name dropped its neighbours")
	}
	// Dropping by inode reaches the name the inode is cached under, which is
	// what a content or attribute change knows.
	c.dropIno(101)
	if _, ok := c.lookup(7, "f0001", now); ok {
		t.Error("an entry survived its inode being invalidated")
	}
	// And a directory's own inode drops the listing it holds.
	c.putListing(9, attrs(3, 200), now)
	c.dropIno(9)
	if _, ok := c.lookup(9, "f0000", now); ok {
		t.Error("a listing survived its directory being invalidated")
	}
	c.clear()
	if _, ok := c.lookup(7, "f0002", now); ok {
		t.Error("an entry survived clear")
	}
	if c.size() != 0 {
		t.Errorf("clear left %d entries", c.size())
	}
}

// A walk of a very large tree must not grow the cache without limit: it is
// bounded by both the number of directories and the total number of entries,
// and the oldest listing goes first.
func TestEntryCacheIsBounded(t *testing.T) {
	c := newEntryCache(time.Minute)
	now := time.Now()
	for d := 0; d < entryCacheDirs*4; d++ {
		c.putListing(uint64(1000+d), attrs(200, uint64(1_000_000+d*1000)), now)
		if got := len(c.byDir); got > entryCacheDirs {
			t.Fatalf("holding %d listings, limit is %d", got, entryCacheDirs)
		}
		if got := c.size(); got > entryCacheEntries {
			t.Fatalf("holding %d entries, limit is %d", got, entryCacheEntries)
		}
	}
	// The newest listing is the one a walk is about to ask about.
	if _, ok := c.lookup(uint64(1000+entryCacheDirs*4-1), "f0000", now); !ok {
		t.Error("the most recent listing was evicted")
	}
	if _, ok := c.lookup(1000, "f0000", now); ok {
		t.Error("the oldest listing outlived the bound")
	}
	if got := len(c.byChild); got != c.size() {
		t.Errorf("the inode index holds %d entries against %d cached", got, c.size())
	}
	// An inode that turns up under a new name — it was renamed into another
	// cached directory — leaves no entry the inode index cannot reach, or a
	// later invalidation of that inode would miss it.
	c.clear()
	c.putListing(1, []vfs.Attr{{Name: "before", Ino: 42}}, now)
	c.putListing(2, []vfs.Attr{{Name: "after", Ino: 42}}, now)
	if _, ok := c.lookup(1, "before", now); ok {
		t.Error("the old name survived the inode moving")
	}
	c.dropIno(42)
	if _, ok := c.lookup(2, "after", now); ok {
		t.Error("invalidating the inode missed the name it is now cached under")
	}
	if c.size() != 0 || len(c.byChild) != 0 {
		t.Errorf("%d entries and %d index rows left behind", c.size(), len(c.byChild))
	}

	// A listing too large to hold is dropped rather than evicting everything.
	c.putListing(6, attrs(2, 8_000_000), now)
	c.putListing(5, attrs(entryCacheEntries+1, 9_000_000), now)
	if _, ok := c.lookup(5, "f0000", now); ok {
		t.Error("an oversized listing was cached")
	}
	if c.size() == 0 {
		t.Error("an oversized listing emptied the cache")
	}
}

// A nil cache is the Linux case: every call is a no-op and nothing is ever
// answered from it.
func TestNilEntryCacheIsInert(t *testing.T) {
	var c *entryCache
	c.putListing(1, attrs(2, 10), time.Now())
	c.dropEntry(1, "f0000")
	c.dropIno(10)
	c.clear()
	if _, ok := c.lookup(1, "f0000", time.Now()); ok {
		t.Fatal("a disabled cache answered a lookup")
	}
	if c.size() != 0 {
		t.Fatal("a disabled cache holds entries")
	}
}
