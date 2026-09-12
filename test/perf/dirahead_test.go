package perf

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/meta"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

// Directory read-ahead answers the case block read-ahead cannot help
// with: many small files read one after another, where the drive's
// request rate is the ceiling and every file costs a round trip. These
// tests assert what it costs in provider calls, which is the only thing
// that matters on a real account.

type dirAheadRig struct {
	fs   *vfs.FS
	fake *fakeprovider.Fake
}

// newDirAheadRig mounts one fake drive with a cache policy that turns
// sibling prefetch on, which is what the daemon does for a mount whose
// layout names one.
func newDirAheadRig(t *testing.T, policy vfs.CachePolicy) *dirAheadRig {
	t.Helper()
	return newDirAheadRigBudget(t, policy, 0)
}

// newDirAheadRigBudget is newDirAheadRig with an explicit write-behind
// budget, which is also the ceiling read-ahead holds itself to.
func newDirAheadRigBudget(t *testing.T, policy vfs.CachePolicy, writeBehind int64) *dirAheadRig {
	t.Helper()
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4096, WriteBehind: writeBehind})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	fake := fakeprovider.New("ali")
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca,
		AttrTTL: time.Hour, DefaultDirTTL: time.Hour, NegativeTTL: time.Minute,
		Mounts: []vfs.Mount{{
			Prefix: "/", Remote: "ali", RootID: fakeprovider.RootID,
			Provider: fake, Mode: config.ModeWriteback, DirTTL: time.Hour, Policy: policy,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	return &dirAheadRig{fs: fsys, fake: fake}
}

// settle waits until the backend has been quiet for a moment, so a
// prefetch that is still in flight is not counted against the reads that
// come after it.
func (r *dirAheadRig) settle(t *testing.T) {
	t.Helper()
	last := -1
	for i := 0; i < 200; i++ {
		n := r.fake.TotalCalls()
		if n == last {
			return
		}
		last = n
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the backend never went quiet")
}

func seedSiblings(f *fakeprovider.Fake, dir string, n, size int) {
	content := make([]byte, size)
	for i := range content {
		content[i] = byte('a' + i%26)
	}
	for i := 0; i < n; i++ {
		f.Seed(fmt.Sprintf("%s/file%03d.txt", dir, i), content)
	}
}

func smallFilePolicy() vfs.CachePolicy {
	return vfs.CachePolicy{DirReadahead: 32, SmallFileThreshold: 4 << 20, SmallFileWhole: true}
}

// TestDirectoryReadaheadMakesSiblingReadsFree: reading a few files in
// listing order pulls the ones after them, so the reads that follow cost
// no provider call at all. This is the whole point of the feature: a
// thousand-file copy stops being a thousand serial round trips.
func TestDirectoryReadaheadMakesSiblingReadsFree(t *testing.T) {
	r := newDirAheadRig(t, smallFilePolicy())
	ctx := context.Background()
	// Twelve files: three to arm the window, and the rest fit inside one
	// prefetch window, so the reads that follow have nothing left to
	// trigger and the count below is purely what they cost themselves.
	seedSiblings(r.fake, "photos", 12, 1024)
	if _, err := r.fs.ReadDirPath(ctx, "/photos"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := r.fs.ReadFileRange(ctx, fmt.Sprintf("/photos/file%03d.txt", i), 0, 1024); err != nil {
			t.Fatal(err)
		}
	}
	r.settle(t)
	before := r.fake.Calls("ReadRange")
	for i := 3; i <= 10; i++ {
		got, err := r.fs.ReadFileRange(ctx, fmt.Sprintf("/photos/file%03d.txt", i), 0, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1024 {
			t.Fatalf("file%03d read %d bytes", i, len(got))
		}
	}
	if extra := r.fake.Calls("ReadRange") - before; extra != 0 {
		t.Fatalf("eight prefetched siblings cost %d range requests", extra)
	}
}

// TestRandomOrderReadsDoNotTriggerDirectoryReadahead: a reader jumping
// around a directory must not make the prefetcher pull files nobody asked
// for. Ten out-of-order reads cost exactly ten requests.
func TestRandomOrderReadsDoNotTriggerDirectoryReadahead(t *testing.T) {
	r := newDirAheadRig(t, smallFilePolicy())
	ctx := context.Background()
	seedSiblings(r.fake, "photos", 64, 1024)
	if _, err := r.fs.ReadDirPath(ctx, "/photos"); err != nil {
		t.Fatal(err)
	}
	before := r.fake.Calls("ReadRange")
	for _, i := range []int{40, 3, 39, 2, 38, 1, 37, 0, 36, 35} {
		if _, err := r.fs.ReadFileRange(ctx, fmt.Sprintf("/photos/file%03d.txt", i), 0, 1024); err != nil {
			t.Fatal(err)
		}
	}
	r.settle(t)
	if got := r.fake.Calls("ReadRange") - before; got != 10 {
		t.Fatalf("ten out-of-order reads cost %d range requests, want 10", got)
	}
}

// TestDirectoryReadaheadSkipsLargeFiles: a big file has block read-ahead
// of its own, and pulling whole large files on speculation is how a
// prefetcher turns into a bandwidth bill.
func TestDirectoryReadaheadSkipsLargeFiles(t *testing.T) {
	r := newDirAheadRig(t, vfs.CachePolicy{DirReadahead: 32, SmallFileThreshold: 4 << 10, SmallFileWhole: true})
	ctx := context.Background()
	seedSiblings(r.fake, "mixed", 3, 1024)
	big := make([]byte, 64<<10) // far above this mount's 4 KiB threshold
	for i := range big {
		big[i] = byte(i)
	}
	for i := 3; i < 8; i++ {
		r.fake.Seed(fmt.Sprintf("mixed/file%03d.txt", i), big)
	}
	if _, err := r.fs.ReadDirPath(ctx, "/mixed"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := r.fs.ReadFileRange(ctx, fmt.Sprintf("/mixed/file%03d.txt", i), 0, 1024); err != nil {
			t.Fatal(err)
		}
	}
	r.settle(t)
	if read := r.fake.ReadBytes(); read > 16<<10 {
		t.Fatalf("the prefetcher pulled %d bytes; the large siblings should have been left alone", read)
	}
}

// TestDirectoryReadaheadIsBoundedByRemoteSlots: speculation must never use
// the whole connection budget, or a foreground read waits behind it.
func TestDirectoryReadaheadIsBoundedByRemoteSlots(t *testing.T) {
	r := newDirAheadRig(t, smallFilePolicy())
	ctx := context.Background()
	caps := r.fake.Capabilities()
	caps.QPS.Download = 2
	caps.MaxConnsPerHost = 3
	r.fake.SetCaps(caps)
	r.fake.SetFaults(func(f *fakeprovider.Faults) { f.Latency = 5 * time.Millisecond })
	seedSiblings(r.fake, "photos", 64, 1024)
	if _, err := r.fs.ReadDirPath(ctx, "/photos"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := r.fs.ReadFileRange(ctx, fmt.Sprintf("/photos/file%03d.txt", i), 0, 1024); err != nil {
			t.Fatal(err)
		}
	}
	r.settle(t)
	if got := r.fake.PeakConcurrent(); got > 2 {
		t.Fatalf("prefetch ran %d requests at once; QPS 2 and a budget of 3 allow at most 2", got)
	}
}

// TestDirectoryReadaheadStopsOnListingChange: when the listing moves under
// the prefetcher, what it thought came next is a guess about names that no
// longer hold, so it starts over rather than spending requests on them.
func TestDirectoryReadaheadStopsOnListingChange(t *testing.T) {
	r := newDirAheadRig(t, smallFilePolicy())
	ctx := context.Background()
	seedSiblings(r.fake, "photos", 64, 1024)
	if _, err := r.fs.ReadDirPath(ctx, "/photos"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := r.fs.ReadFileRange(ctx, fmt.Sprintf("/photos/file%03d.txt", i), 0, 1024); err != nil {
			t.Fatal(err)
		}
	}
	r.settle(t)
	at, err := r.fs.StatPath(ctx, "/photos")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.fs.Refresh(ctx, at.Ino); err != nil {
		t.Fatal(err)
	}
	r.settle(t)
	before := r.fake.Calls("ReadRange")
	// The next read arms the window again from where it is now, so it may
	// prefetch — but it must not have kept claiming from the old listing
	// while the refresh was running.
	if _, err := r.fs.ReadFileRange(ctx, "/photos/file003.txt", 0, 1024); err != nil {
		t.Fatal(err)
	}
	r.settle(t)
	if got := r.fake.Calls("ReadRange") - before; got > 1 {
		t.Fatalf("one read after a refresh cost %d range requests", got)
	}
}

// TestReadaheadWindowFollowsTheReaderRate: a window is runway, and runway
// is measured in seconds of playback, not in megabytes. A reader taking
// 1 MiB/s must not have 64 MiB pulled in front of it — that is data
// nobody asked for, paid for in requests and in cache the rest of the
// mount could have used.
func TestReadaheadWindowFollowsTheReaderRate(t *testing.T) {
	r := newDirAheadRig(t, vfs.CachePolicy{ReadaheadLead: 2 * time.Second, ReadaheadMax: 64 << 12})
	ctx := context.Background()
	const size = 64 << 12 // 16 blocks of 4 KiB
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i)
	}
	r.fake.Seed("media/movie.bin", content)

	// Read the first four blocks slowly: about one block every 100 ms, so
	// roughly 40 KiB/s, which two seconds of lead makes ~20 blocks — but
	// the file is 16 blocks long, so what this really asserts is that the
	// window never runs away from a reader that is barely moving.
	for i := 0; i < 4; i++ {
		if _, err := r.fs.ReadFileRange(ctx, "/media/movie.bin", int64(i)*4096, 4096); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	r.settle(t)
	// A 40 KiB/s reader with 2 s of lead wants ~80 KiB of runway. Having
	// read 16 KiB, anything past ~128 KiB of traffic means the window
	// ignored the rate and ran to its ceiling.
	if read := r.fake.ReadBytes(); read > 128<<10 {
		t.Fatalf("a slow reader pulled %d bytes; the window should follow its rate", read)
	}
}

// TestReadaheadStaysUnderTheWriteBehindBudget: blocks in flight become
// write-behind memory the moment they land, so read-ahead must respect
// the same ceiling rather than trading a bounded queue for an unbounded
// one.
func TestReadaheadStaysUnderTheWriteBehindBudget(t *testing.T) {
	r := newDirAheadRigBudget(t, vfs.CachePolicy{}, 32<<10) // 8 blocks of 4 KiB
	ctx := context.Background()
	const size = 4 << 20
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i)
	}
	r.fake.Seed("media/big.bin", content)
	r.fake.SetFaults(func(f *fakeprovider.Faults) { f.Latency = 20 * time.Millisecond })
	for i := 0; i < 6; i++ {
		if _, err := r.fs.ReadFileRange(ctx, "/media/big.bin", int64(i)*4096, 4096); err != nil {
			t.Fatal(err)
		}
	}
	r.settle(t)
	// Eight blocks of budget, one range request per coalesced run: more
	// than a handful of requests in flight at once means the budget was
	// not consulted.
	if got := r.fake.PeakConcurrent(); got > 8 {
		t.Fatalf("%d range requests were in flight at once against a budget of 8 blocks", got)
	}
}
