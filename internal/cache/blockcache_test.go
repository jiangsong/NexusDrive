package cache

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTest(t *testing.T, opt Options) (*Cache, *clock) {
	t.Helper()
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	if opt.Dir == "" {
		opt.Dir = t.TempDir()
	}
	if opt.BlockSize == 0 {
		opt.BlockSize = 16
	}
	opt.Now = c.now
	if opt.FreeSpace == nil {
		opt.FreeSpace = func(string) (int64, error) { return 1 << 40, nil }
	}
	ca, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	return ca, c
}

var key = FileKey{Remote: "ali", RemoteID: "file-1", Version: "v1"}

func TestPutGetMiss(t *testing.T) {
	c, _ := newTest(t, Options{})
	if _, ok := c.Get(key, 0); ok {
		t.Fatal("empty cache should miss")
	}
	if err := c.Put(key, 0, []byte("0123456789abcdef"), 40); err != nil {
		t.Fatal(err)
	}
	got, ok := c.Get(key, 0)
	if !ok || string(got) != "0123456789abcdef" {
		t.Fatalf("get = %q, %v", got, ok)
	}
	s := c.Stats()
	if s.Hits != 1 || s.Misses != 1 || s.Blocks != 1 || s.Bytes != 16 {
		t.Fatalf("stats = %+v", s)
	}
	if r := s.HitRatio(); r != 0.5 {
		t.Fatalf("hit ratio = %v", r)
	}
}

func TestBlockGeometry(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16})
	if c.BlockCount(0) != 0 || c.BlockCount(1) != 1 || c.BlockCount(16) != 1 || c.BlockCount(17) != 2 {
		t.Fatal("block count wrong")
	}
	if off, n := c.BlockRange(2, 40); off != 32 || n != 8 {
		t.Fatalf("last partial block = %d,%d", off, n)
	}
	if c.BlockIndex(31) != 1 || c.BlockIndex(32) != 2 {
		t.Fatal("block index wrong")
	}
}

func TestPresenceAndHydrate(t *testing.T) {
	// The delay is what this test is not about: production waits for a
	// genuinely quiet mount, which no test wants to sit through.
	c, _ := newTest(t, Options{BlockSize: 16, HydrateAfter: time.Millisecond})
	const size = 40
	data := []byte("0123456789abcdefghijklmnopqrstuvwxyz1234")
	if len(data) != size {
		t.Fatalf("fixture is %d bytes", len(data))
	}
	for idx := int64(0); idx < 2; idx++ {
		off, n := c.BlockRange(idx, size)
		if err := c.Put(key, idx, data[off:off+n], size); err != nil {
			t.Fatal(err)
		}
	}
	if have, total := c.Present(key); have != 2 || total != 3 {
		t.Fatalf("presence = %d/%d", have, total)
	}
	if c.Complete(key) {
		t.Fatal("should not be complete yet")
	}
	if _, ok := c.HydratedPath(key); ok {
		t.Fatal("should not be hydrated yet")
	}
	// Final block triggers automatic hydration.
	off, n := c.BlockRange(2, size)
	if err := c.Put(key, 2, data[off:off+n], size); err != nil {
		t.Fatal(err)
	}
	if !c.Complete(key) {
		t.Fatal("should be complete")
	}
	p, ok := waitHydrated(t, c, key)
	if !ok {
		t.Fatal("should be hydrated")
	}
	whole, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(whole, data) {
		t.Fatalf("hydrated content mismatch: %q %v", whole, err)
	}
	// Individual blocks are gone, but Get still serves from the hydrated file.
	if s := c.Stats(); s.Blocks != 0 || s.HydratedFiles != 1 {
		t.Fatalf("after hydrate stats = %+v", s)
	}
	got, ok := c.Get(key, 1)
	if !ok || string(got) != string(data[16:32]) {
		t.Fatalf("hydrated read = %q, %v", got, ok)
	}
	got, ok = c.Get(key, 2)
	if !ok || string(got) != string(data[32:]) {
		t.Fatalf("hydrated tail read = %q, %v", got, ok)
	}
}

func TestEvictionByBytes(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 48})
	blk := bytes.Repeat([]byte("x"), 16)
	// A large logical size keeps hydration (which would drop blocks) away.
	for i := int64(0); i < 3; i++ {
		if err := c.Put(key, i, blk, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	if s := c.Stats(); s.Blocks != 3 || s.Bytes != 48 {
		t.Fatalf("before eviction = %+v", s)
	}
	if err := c.Put(key, 3, blk, 1<<20); err != nil {
		t.Fatal(err)
	}
	s := c.Stats()
	if s.Blocks != 3 || s.Bytes != 48 || s.Evictions != 1 {
		t.Fatalf("after eviction = %+v", s)
	}
}

func TestTwoQueueKeepsHotBlocks(t *testing.T) {
	c, cl := newTest(t, Options{BlockSize: 16, MaxBytes: 32})
	blk := bytes.Repeat([]byte("x"), 16)
	c.Put(key, 0, blk, 1<<20)
	cl.advance(time.Second)
	c.Put(key, 1, blk, 1<<20)
	// Touch block 0 twice so it is promoted out of probation.
	c.Get(key, 0)
	c.Get(key, 0)
	cl.advance(time.Second)
	// Admitting a third block must evict the cold one (index 1), not the hot one.
	if err := c.Put(key, 2, blk, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get(key, 0); !ok {
		t.Fatal("hot block was evicted; 2Q should have kept it")
	}
	if _, ok := c.Get(key, 1); ok {
		t.Fatal("cold block should have been evicted")
	}
}

func TestEvictionByAge(t *testing.T) {
	c, cl := newTest(t, Options{BlockSize: 16, MaxAge: time.Minute})
	blk := bytes.Repeat([]byte("x"), 16)
	c.Put(key, 0, blk, 1<<20)
	cl.advance(2 * time.Minute)
	c.Put(key, 1, blk, 1<<20)
	if _, ok := c.Get(key, 0); ok {
		t.Fatal("block older than MaxAge should be evicted")
	}
	if _, ok := c.Get(key, 1); !ok {
		t.Fatal("fresh block should survive")
	}
}

func TestMinFreeBackpressure(t *testing.T) {
	free := int64(100)
	c, _ := newTest(t, Options{BlockSize: 16, MinFree: 90, FreeSpace: func(string) (int64, error) { return free, nil }})
	blk := bytes.Repeat([]byte("x"), 16)
	// 100 - 16 = 84 < 90, and there is nothing to evict: refuse.
	err := c.Put(key, 0, blk, 1<<20)
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("expected ErrNoSpace, got %v", err)
	}
	free = 1 << 30
	if err := c.Put(key, 0, blk, 1<<20); err != nil {
		t.Fatalf("with free space the put should succeed: %v", err)
	}
}

func TestPinExemptFromEviction(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 32})
	blk := bytes.Repeat([]byte("x"), 16)
	c.Put(key, 0, blk, 1<<20)
	c.Pin(key, true)
	other := FileKey{Remote: "ali", RemoteID: "file-2", Version: "v1"}
	c.Put(other, 0, blk, 1<<20)
	c.Put(other, 1, blk, 1<<20)
	if _, ok := c.Get(key, 0); !ok {
		t.Fatal("pinned block must not be evicted")
	}
	if s := c.Stats(); s.PinnedBlocks != 1 {
		t.Fatalf("stats = %+v", s)
	}
	// With everything pinned and no room, Put must refuse rather than thrash.
	c.Pin(other, true)
	if err := c.Put(other, 2, blk, 1<<20); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("all-pinned cache should refuse: %v", err)
	}
}

func TestForgetAndVersionChange(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16})
	blk := bytes.Repeat([]byte("x"), 16)
	c.Put(key, 0, blk, 32)
	c.Forget(key)
	if _, ok := c.Get(key, 0); ok {
		t.Fatal("forgotten block should be gone")
	}
	if s := c.Stats(); s.Blocks != 0 || s.Bytes != 0 {
		t.Fatalf("stats after forget = %+v", s)
	}
	// A new version is a different key, so old blocks never alias.
	v2 := key
	v2.Version = "v2"
	c.Put(key, 0, []byte("aaaaaaaaaaaaaaaa"), 1<<20)
	c.Put(v2, 0, []byte("bbbbbbbbbbbbbbbb"), 1<<20)
	a, _ := c.Get(key, 0)
	b, _ := c.Get(v2, 0)
	if string(a) == string(b) {
		t.Fatal("versions must not share blocks")
	}
}

func TestAdoptFile(t *testing.T) {
	dir := t.TempDir()
	c, _ := newTest(t, Options{Dir: dir, BlockSize: 16})
	src := filepath.Join(t.TempDir(), "written.bin")
	data := []byte("locally written content")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.AdoptFile(key, src, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("adopt should move the file")
	}
	p, ok := c.HydratedPath(key)
	if !ok {
		t.Fatal("adopted file should be hydrated")
	}
	got, _ := os.ReadFile(p)
	if !bytes.Equal(got, data) {
		t.Fatalf("adopted content = %q", got)
	}
	// Reads of the adopted file come from the cache, not the provider.
	blk, ok := c.Get(key, 0)
	if !ok || string(blk) != string(data[:16]) {
		t.Fatalf("read after adopt = %q, %v", blk, ok)
	}
}

func TestReloadAfterRestart(t *testing.T) {
	dir := t.TempDir()
	c, _ := newTest(t, Options{Dir: dir, BlockSize: 16})
	c.Put(key, 0, bytes.Repeat([]byte("y"), 16), 1<<20)
	c.Put(key, 5, bytes.Repeat([]byte("z"), 16), 1<<20)

	c2, _ := newTest(t, Options{Dir: dir, BlockSize: 16})
	s := c2.Stats()
	if s.Blocks != 2 || s.Bytes != 32 {
		t.Fatalf("reloaded stats = %+v", s)
	}
	got, ok := c2.Get(key, 5)
	if !ok || got[0] != 'z' {
		t.Fatalf("reloaded read = %q, %v", got, ok)
	}
}

func TestGetSurvivesMissingFile(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16})
	c.Put(key, 0, bytes.Repeat([]byte("q"), 16), 1<<20)
	// Simulate an external deletion of the block file.
	os.Remove(c.blockPath(key.hash(), 0))
	if _, ok := c.Get(key, 0); ok {
		t.Fatal("missing block file should miss")
	}
	if s := c.Stats(); s.Blocks != 0 {
		t.Fatalf("index should drop the vanished block: %+v", s)
	}
}

func TestConcurrentPutGet(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 1 << 20})
	blk := bytes.Repeat([]byte("c"), 16)
	done := make(chan error, 8)
	for w := 0; w < 8; w++ {
		go func(w int) {
			k := FileKey{Remote: "r", RemoteID: string(rune('a' + w)), Version: "v"}
			for i := int64(0); i < 20; i++ {
				if err := c.Put(k, i, blk, 1<<20); err != nil {
					done <- err
					return
				}
				c.Get(k, i)
			}
			done <- nil
		}(w)
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// TestReadAtServesPartOfABlock is the property that keeps a small read cheap:
// asking for 4 KiB must not require materialising the whole block.
func TestReadAtServesPartOfABlock(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 4096})
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v1"}
	block := make([]byte, 4096)
	for i := range block {
		block[i] = byte(i % 251)
	}
	if err := c.Put(k, 0, block, 4096); err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, 100)
	n, ok := c.ReadAt(k, 0, 1000, dst)
	if !ok || n != 100 {
		t.Fatalf("ReadAt = %d, %v", n, ok)
	}
	if !bytes.Equal(dst, block[1000:1100]) {
		t.Fatal("ReadAt returned the wrong window of the block")
	}
	// A destination larger than what is left in the block is truncated, not
	// an error.
	big := make([]byte, 8192)
	n, ok = c.ReadAt(k, 0, 4000, big)
	if !ok || n != 96 {
		t.Fatalf("tail read = %d, %v; want 96", n, ok)
	}
	// Reading past the end of the block is not a miss.
	n, ok = c.ReadAt(k, 0, 4096, dst)
	if !ok || n != 0 {
		t.Fatalf("past-end read = %d, %v; want 0, true", n, ok)
	}
	// A block that was never stored is a miss.
	if _, ok := c.ReadAt(k, 7, 0, dst); ok {
		t.Fatal("an absent block must report a miss")
	}
}

func TestHasDoesNotCountOrRead(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 4096})
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v1"}
	if err := c.Put(k, 0, make([]byte, 4096), 4096); err != nil {
		t.Fatal(err)
	}
	hits, misses := c.Stats().Hits, c.Stats().Misses
	if !c.Has(k, 0) {
		t.Fatal("Has should see a stored block")
	}
	if c.Has(k, 1) {
		t.Fatal("Has should not see an absent block")
	}
	if s := c.Stats(); s.Hits != hits || s.Misses != misses {
		t.Fatal("Has is a presence test and must not move the hit counters")
	}
}

func TestReadAtFallsBackToHydratedFile(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 8})
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v1"}
	body := []byte("0123456789abcdefXYZ")
	for idx := int64(0); idx*8 < int64(len(body)); idx++ {
		end := (idx + 1) * 8
		if end > int64(len(body)) {
			end = int64(len(body))
		}
		if err := c.Put(k, idx, body[idx*8:end], int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}
	if have, total := c.Present(k); have != total {
		t.Fatalf("expected a complete file, have %d of %d", have, total)
	}
	// Once hydrated the individual block files may be gone; reads must still
	// be served, now out of the merged file.
	dst := make([]byte, 4)
	n, ok := c.ReadAt(k, 1, 2, dst)
	if !ok || n != 4 {
		t.Fatalf("hydrated read = %d, %v", n, ok)
	}
	if string(dst) != string(body[10:14]) {
		t.Fatalf("hydrated read = %q, want %q", dst, body[10:14])
	}
}

func TestPutRangeServesPartsAndPromotesWhenFull(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 64, SubBlockSize: 16, HydrateAfter: time.Millisecond})
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v1"}
	block := make([]byte, 64)
	for i := range block {
		block[i] = byte(i)
	}
	// The second sub-block only.
	if err := c.PutRange(k, 0, 16, block[16:32], 64); err != nil {
		t.Fatal(err)
	}
	if c.Has(k, 0) {
		t.Fatal("a partial block must not count as whole")
	}
	if !c.HasRange(k, 0, 16, 16) || !c.HasRange(k, 0, 20, 4) {
		t.Fatal("the stored sub-block should be servable")
	}
	if c.HasRange(k, 0, 0, 16) || c.HasRange(k, 0, 8, 16) {
		t.Fatal("ranges outside the stored sub-block must miss")
	}
	dst := make([]byte, 4)
	n, ok := c.ReadAt(k, 0, 20, dst)
	if !ok || n != 4 || !bytes.Equal(dst, block[20:24]) {
		t.Fatalf("ReadAt from a partial block = %d, %v, %v", n, ok, dst)
	}
	if _, ok := c.ReadAt(k, 0, 0, dst); ok {
		t.Fatal("ReadAt of a missing sub-block must miss")
	}
	if st := c.Stats(); st.Bytes != 16 {
		t.Fatalf("partial block accounted %d bytes, want 16", st.Bytes)
	}
	// Unaligned ranges are refused so the bitmap stays truthful.
	if err := c.PutRange(k, 0, 3, block[3:19], 64); err == nil {
		t.Fatal("an unaligned range must be rejected")
	}
	// Fill the rest: once written behind, the block becomes whole and the
	// sidecar goes away.
	for _, off := range []int64{0, 32, 48} {
		if err := c.PutRange(k, 0, off, block[off:off+16], 64); err != nil {
			t.Fatal(err)
		}
	}
	waitFlushed(t, c)
	if !c.Has(k, 0) {
		t.Fatal("a block with every sub-block present should be whole")
	}
	if _, err := os.Stat(c.sidecarPath(k.hash(), 0)); !os.IsNotExist(err) {
		t.Fatal("the sidecar should be removed on promotion")
	}
	got, ok := c.Get(k, 0)
	if !ok || !bytes.Equal(got, block) {
		t.Fatal("promoted block content is wrong")
	}
	// A file of exactly one block is now complete and hydrates once idle.
	if _, ok := waitHydrated(t, c, k); !ok {
		t.Fatal("a complete file should hydrate after promotion")
	}
}

func TestPartialBlocksSurviveReload(t *testing.T) {
	dir := t.TempDir()
	c, _ := newTest(t, Options{Dir: dir, BlockSize: 64, SubBlockSize: 16})
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v1"}
	data := bytes.Repeat([]byte{7}, 16)
	if err := c.PutRange(k, 1, 48, data, 128); err != nil {
		t.Fatal(err)
	}
	waitFlushed(t, c)
	c2, _ := newTest(t, Options{Dir: dir, BlockSize: 64, SubBlockSize: 16})
	if c2.Has(k, 1) {
		t.Fatal("after reload a partial block must not be mistaken for a whole one")
	}
	if !c2.HasRange(k, 1, 48, 16) {
		t.Fatal("after reload the stored sub-block should still be servable")
	}
	if c2.HasRange(k, 1, 0, 16) {
		t.Fatal("after reload a missing sub-block must still miss")
	}
	if st := c2.Stats(); st.Bytes != 16 || st.Blocks != 1 {
		t.Fatalf("reloaded stats = %+v", st)
	}
}

func TestEvictionRemovesPartialSidecars(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 64, SubBlockSize: 16, MaxBytes: 32})
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v1"}
	data := bytes.Repeat([]byte{1}, 16)
	for idx := int64(0); idx < 4; idx++ {
		if err := c.PutRange(k, idx, 0, data, 256); err != nil {
			t.Fatal(err)
		}
	}
	if st := c.Stats(); st.Bytes > 32 {
		t.Fatalf("cache exceeded its budget: %d bytes", st.Bytes)
	}
	// Whatever was evicted left no sidecar behind.
	for idx := int64(0); idx < 4; idx++ {
		_, blockErr := os.Stat(c.blockPath(k.hash(), idx))
		_, sideErr := os.Stat(c.sidecarPath(k.hash(), idx))
		if os.IsNotExist(blockErr) && !os.IsNotExist(sideErr) {
			t.Fatalf("block %d was evicted but its sidecar survived", idx)
		}
	}
}

func TestSubRangeAlignment(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 64, SubBlockSize: 16})
	cases := []struct{ off, n, blockLen, wantOff, wantLen int64 }{
		{20, 4, 64, 16, 16},
		{15, 2, 64, 0, 32},
		{60, 4, 64, 48, 16},
		{50, 20, 58, 48, 10}, // tail block shorter than a sub-block multiple
	}
	for _, tc := range cases {
		off, n := c.SubRange(tc.off, tc.n, tc.blockLen)
		if off != tc.wantOff || n != tc.wantLen {
			t.Errorf("SubRange(%d,%d,%d) = %d,%d want %d,%d", tc.off, tc.n, tc.blockLen, off, n, tc.wantOff, tc.wantLen)
		}
	}
}

// waitHydrated polls for the deferred hydration of a complete file.
func waitHydrated(t *testing.T, c *Cache, k FileKey) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p, ok := c.HydratedPath(k); ok {
			return p, true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return c.HydratedPath(k)
}

// claimsLag reports whether any partial block holds more than its sidecar
// claims. The sweep is what clears it.
func (c *Cache) claimsLag() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lagging > 0
}

// waitFlushed waits for the write-behind queue to drain and for the on-disk
// claims to catch up with it. Both are what a reload sees, and the sweep that
// updates the claims is the production one: nothing here forces it.
func waitFlushed(t *testing.T, c *Cache) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for c.WriteBehindPending() != 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if p := c.WriteBehindPending(); p != 0 {
		t.Fatalf("%d bytes still pending after 5 s", p)
	}
	// The data is on disk; the claim that names it may still be inside its
	// write window. Push it out the way a shutdown does, so what a reload
	// would see is what these tests check.
	c.sweepAllClaims()
	if c.claimsLag() {
		t.Fatal("on-disk claims still lag the cached data after a full sweep")
	}
}

// Hydration used to wait for the whole mount to fall idle. It now waits for
// the file it is merging to fall idle instead; hydrate_test.go holds that
// contract, and this is only the end-to-end check that the janitor's own timer
// still gets a quiet file merged.
func TestHydrationRunsOnceTheFileIsQuiet(t *testing.T) {
	c, clk := newTest(t, Options{BlockSize: 16, HydrateAfter: time.Millisecond})
	t.Cleanup(func() { c.Close() })
	var busy atomic.Bool
	busy.Store(true)
	c.SetBusy(busy.Load)
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v"}
	if err := c.Put(k, 0, bytes.Repeat([]byte("a"), 16), 32); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(k, 1, bytes.Repeat([]byte("b"), 16), 32); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Second)
	if _, ok := waitHydrated(t, c, k); !ok {
		t.Fatal("a quiet file never hydrated")
	}
}

// TestReloadedPartialBlocksDoNotReadAsEmpty: a restart knows the block files
// but not how long the file is. Reporting a hit of zero bytes for a partial
// block hands the reader an early EOF, which an application sees as a file
// that lost its contents.
func TestReloadedPartialBlocksDoNotReadAsEmpty(t *testing.T) {
	dir := t.TempDir()
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v"}
	c, err := newClosingCache(t, Options{Dir: dir, BlockSize: 64 << 10, SubBlockSize: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("s"), 16<<10)
	if err := c.PutRange(k, 0, 0, data, 4*64<<10); err != nil {
		t.Fatal(err)
	}
	waitFlushed(t, c)

	c2, err := newClosingCache(t, Options{Dir: dir, BlockSize: 64 << 10, SubBlockSize: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, ok := c2.ReadAt(k, 0, 0, buf)
	if ok && n == 0 {
		t.Fatal("a reloaded partial block reported a hit of zero bytes: the reader sees an empty file")
	}
}

// TestPromotingOverAPartialBlockReleasesIt: a whole block landing on top of a
// partial one is the ordinary promotion path. Overwriting the map entry
// without releasing the old one leaks its open descriptor and leaves its
// memory counted against write-behind for ever, which silently turns off both
// the partial-block descriptor cache and write-behind itself.
func TestPromotingOverAPartialBlockReleasesIt(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 64, SubBlockSize: 16})
	t.Cleanup(func() { c.Close() })
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v"}
	block := bytes.Repeat([]byte("w"), 64)
	for i := 0; i < 5; i++ {
		key := FileKey{Remote: k.Remote, RemoteID: fmt.Sprintf("id%d", i), Version: "v"}
		if err := c.PutRange(key, 0, 0, block[:16], 64); err != nil {
			t.Fatal(err)
		}
		waitFlushed(t, c)
		if err := c.Put(key, 0, block, 64); err != nil {
			t.Fatal(err)
		}
	}
	c.mu.Lock()
	open, pending, lagging := c.openPartials, c.wb.pending, c.lagging
	c.mu.Unlock()
	if open != 0 || pending != 0 || lagging != 0 {
		t.Fatalf("after 5 promotions: openPartials=%d wb.pending=%d lagging=%d, want zeros", open, pending, lagging)
	}
}

// TestCacheKnowsItsFilesAfterAReload: a restart can hash a key into a
// directory name but not read one back out of it. Without the identity beside
// the blocks, Keys returns nothing, so `cache drop` reports files it never
// touched and a benchmark's cold pass measures a warm cache.
func TestCacheKnowsItsFilesAfterAReload(t *testing.T) {
	dir := t.TempDir()
	k := FileKey{Remote: "r", RemoteID: "id-42", Version: "v7"}
	c, err := newClosingCache(t, Options{Dir: dir, BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put(k, 0, bytes.Repeat([]byte("z"), 64), 128); err != nil {
		t.Fatal(err)
	}
	if got := c.Keys(); len(got) != 1 || got[0] != k {
		t.Fatalf("before reload Keys() = %+v, want %+v", got, k)
	}
	c2, err := newClosingCache(t, Options{Dir: dir, BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	got := c2.Keys()
	if len(got) != 1 || got[0] != k {
		t.Fatalf("after reload Keys() = %+v, want %+v", got, k)
	}
	c2.Forget(k)
	if n := len(c2.Keys()); n != 0 {
		t.Fatalf("%d keys left after forgetting the only file", n)
	}
}
