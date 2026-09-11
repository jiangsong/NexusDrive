// Package perf establishes the performance baselines from docs/DESIGN.md
// section 8: how many provider calls a cold and a warm traversal cost, how
// sequential reads behave, and that a warm working directory is local.
//
// These are correctness-of-caching tests expressed as call counts, not
// wall-clock benchmarks: a regression that silently doubles provider traffic
// is the failure they exist to catch.
package perf

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

type harness struct {
	fs   *vfs.FS
	fake *fakeprovider.Fake
	up   *upload.Uploader
}

func newHarness(t *testing.T, blockSize int64, readAhead, prefetchDepth int) *harness {
	return newHarnessOpt(t, blockSize, readAhead, prefetchDepth, 0)
}

// newHarnessOpt is newHarness with an explicit ReadaheadRequest, so a test
// that cares about readahead coalescing does not depend on the mount
// provider's default capabilities to derive it.
func newHarnessOpt(t *testing.T, blockSize int64, readAhead, prefetchDepth int, readaheadRequest int64) *harness {
	t.Helper()
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: blockSize})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })

	fake := fakeprovider.New("ali")
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca,
		AttrTTL: time.Hour, DefaultDirTTL: time.Hour, NegativeTTL: time.Minute,
		ReadAheadBlocks: readAhead, PrefetchDepth: prefetchDepth,
		ReadaheadRequest: readaheadRequest,
		Mounts: []vfs.Mount{{
			Prefix: "/", Remote: "ali", RootID: fakeprovider.RootID,
			Provider: fake, Mode: config.ModeWriteback, DirTTL: time.Hour,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	up, err := upload.New(upload.Options{
		Journal:   j,
		Providers: func(string) (provider.Provider, bool) { return fake, true },
		Policy:    retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: time.Millisecond}, MaxAttempts: 2},
		Hooks:     fsys.UploadHooks(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fsys.SetWriteBackend(j, up)
	return &harness{fs: fsys, fake: fake, up: up}
}

// seedTree builds dirs × filesPerDir files.
func seedTree(f *fakeprovider.Fake, dirs, filesPerDir int) int {
	total := 0
	for d := 0; d < dirs; d++ {
		for i := 0; i < filesPerDir; i++ {
			f.Seed(fmt.Sprintf("proj/dir%03d/file%03d.txt", d, i), []byte("content"))
			total++
		}
	}
	return total
}

// TestWarmTraversalIsFree is the headline claim: after one traversal, walking
// the same tree again costs zero provider calls.
func TestWarmTraversalIsFree(t *testing.T) {
	h := newHarness(t, 4096, 0, 0)
	const dirs, perDir = 20, 25
	want := seedTree(h.fake, dirs, perDir)

	// Cold walk.
	cold := walk(t, h.fs, "/")
	if cold.files != want {
		t.Fatalf("cold walk saw %d files, want %d", cold.files, want)
	}
	coldCalls := h.fake.TotalCalls()
	if coldCalls == 0 {
		t.Fatal("a cold walk must talk to the provider")
	}

	// Warm walk.
	warm := walk(t, h.fs, "/")
	if warm.files != want {
		t.Fatalf("warm walk saw %d files, want %d", warm.files, want)
	}
	if extra := h.fake.TotalCalls() - coldCalls; extra != 0 {
		t.Fatalf("a warm walk made %d provider calls, want 0", extra)
	}
	t.Logf("cold walk of %d dirs / %d files cost %d provider calls; warm walk cost 0",
		dirs+2, want, coldCalls)
}

type walkResult struct{ dirs, files int }

func walk(t *testing.T, fsys *vfs.FS, path string) walkResult {
	t.Helper()
	var res walkResult
	var rec func(string)
	rec = func(p string) {
		entries, err := fsys.ReadDirPath(context.Background(), p)
		if err != nil {
			t.Fatalf("readdir %s: %v", p, err)
		}
		res.dirs++
		for _, e := range entries {
			child := p
			if child == "/" {
				child = "/" + e.Name
			} else {
				child = p + "/" + e.Name
			}
			if e.IsDir {
				rec(child)
			} else {
				res.files++
			}
		}
	}
	rec(path)
	return res
}

// TestColdTraversalCostIsProportional checks the listing cost scales with the
// number of directories, not with the number of files: a regression that
// statted every file individually would show up here.
func TestColdTraversalCostIsProportional(t *testing.T) {
	h := newHarness(t, 4096, 0, 0)
	const dirs, perDir = 10, 40
	seedTree(h.fake, dirs, perDir)

	walk(t, h.fs, "/")
	lists := h.fake.Calls("List")
	stats := h.fake.Calls("Stat")

	// The fake pages two entries at a time, so a directory of 40 files needs
	// 20 List calls. What matters is that no per-file Stat happened.
	if stats != 0 {
		t.Fatalf("traversal issued %d per-file Stat calls; listings should carry attributes", stats)
	}
	// dirs directories of perDir files, plus /proj and /.
	minimum := dirs * (perDir / 2)
	if lists < minimum {
		t.Fatalf("only %d List calls for %d paged entries", lists, dirs*perDir)
	}
	t.Logf("cold traversal of %d files: %d List calls, %d Stat calls", dirs*perDir, lists, stats)
}

// TestSequentialReadUsesWholeBlocks checks a 1 MiB sequential read costs one
// provider call per block and not one per read() syscall.
func TestSequentialReadUsesWholeBlocks(t *testing.T) {
	const blockSize = 64 << 10
	h := newHarness(t, blockSize, 0, 0)
	ctx := context.Background()
	size := 1 << 20
	h.fake.Seed("big.bin", bytes.Repeat([]byte("x"), size))
	if _, err := h.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	before := h.fake.Calls("ReadRange")

	// Read in 4 KiB pieces, the way an application would.
	node, err := h.fs.Meta().Resolve(ctx, "/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := h.fs.Open(ctx, node.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.fs.Release(ctx, handle)
	buf := make([]byte, 4096)
	for off := int64(0); off < int64(size); off += 4096 {
		if _, err := h.fs.Read(ctx, handle, buf, off); err != nil {
			t.Fatal(err)
		}
	}
	fetches := h.fake.Calls("ReadRange") - before
	wantBlocks := size / blockSize
	// The first reads of a handle are fetched as sub-blocks until the run
	// is recognised as sequential; after that it is one fetch per block.
	if fetches < wantBlocks || fetches > wantBlocks+3 {
		t.Fatalf("256 reads of a %d-block file caused %d fetches, want %d (+ at most 3 probing sub-blocks)", wantBlocks, fetches, wantBlocks)
	}
	t.Logf("1 MiB read in 4 KiB pieces: %d provider fetches for %d blocks", fetches, wantBlocks)
}

// TestReadAheadReducesLatencyStalls checks the read-ahead window actually
// fetches ahead, so a sequential reader does not stall on every block.
func TestReadAheadReducesLatencyStalls(t *testing.T) {
	const blockSize = 4096
	h := newHarness(t, blockSize, 8, 0)
	ctx := context.Background()
	size := blockSize * 16
	h.fake.Seed("stream.bin", bytes.Repeat([]byte("s"), size))
	if _, err := h.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	node, _ := h.fs.Meta().Resolve(ctx, "/stream.bin")
	handle, err := h.fs.Open(ctx, node.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.fs.Release(ctx, handle)

	buf := make([]byte, blockSize)
	// Three sequential blocks arm and grow the window.
	for i := int64(0); i < 3; i++ {
		if _, err := h.fs.Read(ctx, handle, buf, i*blockSize); err != nil {
			t.Fatal(err)
		}
	}
	key := cache.FileKey{Remote: node.Remote, RemoteID: node.RemoteID, Version: node.Version}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if have, _ := h.fs.Cache().Present(key); have > 3 {
			t.Logf("after 3 sequential reads, %d of 16 blocks are cached", have)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	have, total := h.fs.Cache().Present(key)
	t.Fatalf("read-ahead did not run: %d/%d blocks cached after three sequential reads", have, total)
}

// TestReadaheadCoalescesContiguousBlocks checks that a sequential read groups
// the readahead window's contiguous missing blocks into range requests sized
// by ReadaheadRequest, instead of one ReadRange call per block.
func TestReadaheadCoalescesContiguousBlocks(t *testing.T) {
	const blockSize = 64 << 10
	const readaheadRequest = 256 << 10 // 4 blocks per range request
	h := newHarnessOpt(t, blockSize, 32, 0, readaheadRequest)
	ctx := context.Background()
	const size = 2 << 20 // 2 MiB
	h.fake.Seed("big.bin", bytes.Repeat([]byte("x"), size))
	if _, err := h.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	before := h.fake.Calls("ReadRange")

	node, err := h.fs.Meta().Resolve(ctx, "/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := h.fs.Open(ctx, node.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	// Read in 4 KiB pieces, the way an application would; this leaves the
	// background prefetch runs time to land between calls, matching the
	// granularity TestSequentialReadUsesWholeBlocks uses for the same reason.
	buf := make([]byte, 4096)
	for off := int64(0); off < int64(size); off += 4096 {
		if _, err := h.fs.Read(ctx, handle, buf, off); err != nil {
			t.Fatal(err)
		}
	}
	key := cache.FileKey{Remote: node.Remote, RemoteID: node.RemoteID, Version: node.Version}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if have, total := h.fs.Cache().Present(key); have == total {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := h.fs.Release(ctx, handle); err != nil {
		t.Fatal(err)
	}
	fetches := h.fake.Calls("ReadRange") - before
	want := int(size/readaheadRequest) + 3
	if fetches > want {
		t.Fatalf("2 MiB sequential read with a %d KiB readahead request cost %d ReadRange calls, want <= %d",
			readaheadRequest>>10, fetches, want)
	}
	t.Logf("2 MiB read coalesced into %d ReadRange calls (bound %d)", fetches, want)
}

// TestShortRangeDisablesCoalescingForRemote checks that when a coalesced
// multi-block range comes back short — a server that caps request size below
// what readahead asked for — the run recovers by fetching that stretch one
// block at a time, and the remote is remembered as "never coalesce" for the
// rest of the process: a later file on the same remote costs one ReadRange
// call per block, with no further attempt at a multi-block request.
func TestShortRangeDisablesCoalescingForRemote(t *testing.T) {
	const blockSize = 64 << 10
	h := newHarnessOpt(t, blockSize, 32, 0, 256<<10) // 4 blocks per request
	ctx := context.Background()
	const size = blockSize * 16
	content := bytes.Repeat([]byte("a"), size)
	h.fake.Seed("first.bin", content)
	h.fake.Seed("second.bin", content)
	if _, err := h.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	// Every ReadRange answer is capped at one block, the way a server that
	// refuses request sizes above its own block would behave.
	h.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.ShortRead = blockSize })

	readSequential := func(name string) {
		t.Helper()
		node, err := h.fs.Meta().Resolve(ctx, "/"+name)
		if err != nil {
			t.Fatal(err)
		}
		handle, err := h.fs.Open(ctx, node.Ino, false)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, blockSize)
		for off := int64(0); off < int64(size); off += blockSize {
			if _, err := h.fs.Read(ctx, handle, buf, off); err != nil {
				t.Fatal(err)
			}
		}
		key := cache.FileKey{Remote: node.Remote, RemoteID: node.RemoteID, Version: node.Version}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if have, total := h.fs.Cache().Present(key); have == total {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err := h.fs.Release(ctx, handle); err != nil {
			t.Fatal(err)
		}
	}

	readSequential("first.bin")
	got, err := h.fs.ReadFileRange(ctx, "/first.bin", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("read-back after a short coalesced range is corrupted")
	}

	// A second file on the same remote must never retry a multi-block
	// request at all: reset the high-water mark and check that nothing
	// bigger than one block is ever asked for again. This is the precise
	// form of the assertion — a call-count bound alone would still pass if
	// the "never coalesce again" memory were removed, since a failed
	// multi-block attempt plus its single-block retries costs strictly more
	// calls than coalescing would have, not fewer.
	before := h.fake.Calls("ReadRange")
	h.fake.ResetReadRangeStats()
	readSequential("second.bin")
	if max := h.fake.MaxReadRangeLen(); max > blockSize {
		t.Fatalf("second file asked for a %d-byte range (block size is %d); coalescing should be disabled for this remote after the short range", max, blockSize)
	}
	// Also keep an exact count as a deterministic secondary check: with
	// coalescing off, every block costs exactly one ReadRange call.
	wantBlocks := size / blockSize
	if calls := h.fake.Calls("ReadRange") - before; calls != wantBlocks {
		t.Fatalf("second file cost %d ReadRange calls for %d blocks, want exactly %d (coalescing disabled means one call per block)", calls, wantBlocks, wantBlocks)
	}
}

// TestPrefetchMakesFindLocal checks background prefetching turns a recursive
// listing into a mostly local operation.
func TestPrefetchMakesFindLocal(t *testing.T) {
	h := newHarness(t, 4096, 0, 3)
	ctx := context.Background()
	for d := 0; d < 8; d++ {
		h.fake.Seed(fmt.Sprintf("proj/sub%d/inner/file.txt", d), []byte("x"))
	}
	// One listing of the top directory triggers the prefetcher.
	if _, err := h.fs.ReadDirPath(ctx, "/proj"); err != nil {
		t.Fatal(err)
	}
	// Give the background workers a moment.
	time.Sleep(300 * time.Millisecond)
	before := h.fake.TotalCalls()

	res := walk(t, h.fs, "/proj")
	after := h.fake.TotalCalls() - before
	if res.files != 8 {
		t.Fatalf("walk saw %d files, want 8", res.files)
	}
	t.Logf("after prefetch, walking %d directories cost %d provider calls", res.dirs, after)
	// Without prefetching this walk would need at least one List per
	// directory; prefetching should have covered most of them.
	if after >= res.dirs {
		t.Fatalf("prefetch saved nothing: %d calls for %d directories", after, res.dirs)
	}
}

// TestWriteThenReadIsLocal checks the uploaded blob becomes the cache entry, so
// reading back a file you just wrote never downloads it again.
func TestWriteThenReadIsLocal(t *testing.T) {
	h := newHarness(t, 4096, 0, 0)
	ctx := context.Background()
	if _, err := h.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("w"), 40000)
	if _, err := h.fs.WriteFile(ctx, "/written.bin", payload, false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	before := h.fake.Calls("ReadRange")
	got, err := h.fs.ReadFileRange(ctx, "/written.bin", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("read-back mismatch")
	}
	if fetches := h.fake.Calls("ReadRange") - before; fetches != 0 {
		t.Fatalf("reading back a just-uploaded file cost %d downloads, want 0", fetches)
	}
}

// TestSearchIsLocal checks the filename index answers without provider calls.
func TestSearchIsLocal(t *testing.T) {
	h := newHarness(t, 4096, 0, 0)
	ctx := context.Background()
	seedTree(h.fake, 5, 20)
	if _, err := h.fs.Warm(ctx, "/", -1); err != nil {
		t.Fatal(err)
	}
	before := h.fake.TotalCalls()
	results, err := h.fs.Meta().Search(ctx, "file01", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("search found nothing in a warmed tree")
	}
	if extra := h.fake.TotalCalls() - before; extra != 0 {
		t.Fatalf("search made %d provider calls, want 0", extra)
	}
	t.Logf("search over %d indexed files returned %d hits with no provider calls", 100, len(results))
}

// BenchmarkWarmLookup measures the cost of a metadata lookup that hits the
// local store, which is the common case once a directory is warm.
func BenchmarkWarmLookup(b *testing.B) {
	dir := b.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4096})
	if err != nil {
		b.Fatal(err)
	}
	fake := fakeprovider.New("ali")
	for i := 0; i < 200; i++ {
		fake.Seed(fmt.Sprintf("d/file%03d.txt", i), []byte("x"))
	}
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca, AttrTTL: time.Hour, DefaultDirTTL: time.Hour,
		Mounts: []vfs.Mount{{Prefix: "/", Remote: "ali", RootID: fakeprovider.RootID, Provider: fake}},
	})
	if err != nil {
		b.Fatal(err)
	}
	defer fsys.Close()
	ctx := context.Background()
	if _, err := fsys.Warm(ctx, "/", -1); err != nil {
		b.Fatal(err)
	}
	node, err := store.Resolve(ctx, "/d")
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := fsys.Lookup(ctx, node.Ino, "file100.txt"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCachedRead measures a read served entirely from the block cache.
func BenchmarkCachedRead(b *testing.B) {
	dir := b.TempDir()
	store, _ := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	defer store.Close()
	ca, _ := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 64 << 10})
	fake := fakeprovider.New("ali")
	fake.Seed("big.bin", bytes.Repeat([]byte("x"), 1<<20))
	fsys, _ := vfs.New(vfs.Options{
		Meta: store, Cache: ca, AttrTTL: time.Hour, DefaultDirTTL: time.Hour,
		Mounts: []vfs.Mount{{Prefix: "/", Remote: "ali", RootID: fakeprovider.RootID, Provider: fake}},
	})
	defer fsys.Close()
	ctx := context.Background()
	fsys.ReadDirPath(ctx, "/")
	if _, err := fsys.ReadFileRange(ctx, "/big.bin", 0, 1<<20); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.SetBytes(64 << 10)
	for i := 0; i < b.N; i++ {
		if _, err := fsys.ReadFileRange(ctx, "/big.bin", 0, 64<<10); err != nil {
			b.Fatal(err)
		}
	}
}

// TestCreatesDoNotRescanTheDirectory: the lookup miss before a create used to
// load every sibling just to learn that the listing was fresh, which made a
// batch of creates in one directory quadratic (2000 empty files took 39 s on
// a LAN mount). A fresh listing answers a miss without being read.
func TestCreatesDoNotRescanTheDirectory(t *testing.T) {
	h := newHarness(t, 64<<10, 0, 0)
	ctx := context.Background()
	h.fake.Seed("seed.txt", []byte("s"))
	if _, err := h.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	root, err := h.fs.Meta().Resolve(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	scans := h.fs.Meta().ChildrenScans()
	calls := h.fake.TotalCalls()
	const n = 200
	for i := 0; i < n; i++ {
		fh, err := h.fs.Create(ctx, root.Ino, fmt.Sprintf("new-%03d", i))
		if err != nil {
			t.Fatal(err)
		}
		if err := h.fs.Release(ctx, fh); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.fs.Meta().ChildrenScans() - scans; got != 0 {
		t.Fatalf("%d creates in one directory read the whole listing %d times, want 0", n, got)
	}
	if got := h.fake.TotalCalls() - calls; got != 0 {
		t.Fatalf("%d creates cost %d provider calls, want 0", n, got)
	}
}

// TestMetadataStatementBudget puts a ceiling on what one operation may cost
// the metadata store. A cloud-drive mount spends most of its time in SQLite
// rather than on the network once the listing is cached, so the budget, not
// the wall clock, is what a regression here shows up as.
func TestMetadataStatementBudget(t *testing.T) {
	h := newHarness(t, 64<<10, 0, 0)
	ctx := context.Background()
	h.fake.Seed("dir/a.txt", []byte("a"))
	h.fake.Seed("dir/b.txt", []byte("b"))
	if _, err := h.fs.ReadDirPath(ctx, "/dir"); err != nil {
		t.Fatal(err)
	}
	dir, err := h.fs.Meta().Resolve(ctx, "/dir")
	if err != nil {
		t.Fatal(err)
	}
	budget := func(name string, maxQueries, maxWrites int64, f func()) {
		q0, w0 := h.fs.Meta().QueryStats()
		f()
		q1, w1 := h.fs.Meta().QueryStats()
		if q1-q0 > maxQueries || w1-w0 > maxWrites {
			t.Errorf("%s cost %d queries and %d write transactions, budget is %d and %d", name, q1-q0, w1-w0, maxQueries, maxWrites)
		} else {
			t.Logf("%s: %d queries, %d write transactions", name, q1-q0, w1-w0)
		}
	}
	budget("lookup hit", 1, 0, func() {
		if _, err := h.fs.Lookup(ctx, dir.Ino, "a.txt"); err != nil {
			t.Fatal(err)
		}
	})
	budget("lookup miss (fresh listing)", 5, 0, func() {
		if _, err := h.fs.Lookup(ctx, dir.Ino, "nope.txt"); err == nil {
			t.Fatal("expected a miss")
		}
	})
	budget("repeated lookup miss (negative cache)", 1, 0, func() {
		if _, err := h.fs.Lookup(ctx, dir.Ino, "nope.txt"); err == nil {
			t.Fatal("expected a miss")
		}
	})
	a, _ := h.fs.Meta().Resolve(ctx, "/dir/a.txt")
	budget("stat", 1, 0, func() {
		if _, err := h.fs.Stat(ctx, a.Ino); err != nil {
			t.Fatal(err)
		}
	})
	budget("create + write + close", 7, 2, func() {
		fh, err := h.fs.Create(ctx, dir.Ino, "new.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.fs.Write(ctx, fh, []byte("hello"), 0); err != nil {
			t.Fatal(err)
		}
		if err := h.fs.Sync(ctx, fh); err != nil {
			t.Fatal(err)
		}
		if err := h.fs.Release(ctx, fh); err != nil {
			t.Fatal(err)
		}
	})
	budget("readdir of a cached directory", 4, 0, func() {
		if _, err := h.fs.ReadDir(ctx, dir.Ino); err != nil {
			t.Fatal(err)
		}
	})
}
