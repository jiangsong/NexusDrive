package cache

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// The sparse layout's own geometry: small enough to run in a test, large
// enough that the block size is a multiple of any filesystem's own block so
// the disk-usage test below can compare exact byte counts.
const (
	sparseBlock = 16 << 10
	sparseMin   = 64 << 10
	sparseSize  = 8 * sparseBlock
)

var sparseKey = FileKey{Remote: "ali", RemoteID: "movie", Version: "v1"}

func sparseOpts(dir string) Options {
	return Options{Dir: dir, BlockSize: sparseBlock, WholeLayoutMin: sparseMin,
		HydrateAfter: time.Millisecond, FreeSpace: func(string) (int64, error) { return 1 << 40, nil }}
}

// blockBytes is block idx of a deterministic file of sparseSize bytes.
func blockBytes(idx int64) []byte {
	b := make([]byte, sparseBlock)
	for i := range b {
		b[i] = byte('a' + int(idx))
	}
	return b
}

// fillBlocks caches the given blocks the way a cold sequential read does and
// waits for the write-behind workers to put them on disk.
func fillBlocks(t *testing.T, c *Cache, idx ...int64) {
	t.Helper()
	for _, i := range idx {
		if err := c.PutAsync(sparseKey, i, blockBytes(i), sparseSize); err != nil {
			t.Fatalf("put block %d: %v", i, err)
		}
	}
	waitFlushed(t, c)
}

// partFiles reports what the cache left in hydrated/ for the test's key.
func partFiles(t *testing.T, c *Cache) (part, bitmap, whole bool) {
	t.Helper()
	fh := sparseKey.hash()
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	return exists(c.partPath(fh)), exists(c.partBitmapPath(fh)), exists(c.hydratedPath(fh))
}

// TestSparseLayoutSkipsBlockFiles: above WholeLayoutMin a fetched block goes
// straight into the file the cache will publish, so the blocks directory
// never holds a byte of it and no hydration copy is ever queued.
func TestSparseLayoutSkipsBlockFiles(t *testing.T) {
	c, err := newClosingCache(t, sparseOpts(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	fillBlocks(t, c, 0, 1, 2)
	noBlockFilesOnDisk(t, c)
	part, bitmap, whole := partFiles(t, c)
	if !part || !bitmap || whole {
		t.Fatalf("half-filled file on disk: part=%v bitmap=%v whole=%v", part, bitmap, whole)
	}
	if have, total := c.Present(sparseKey); have != 3 || total != 8 {
		t.Fatalf("Present = %d/%d, want 3/8", have, total)
	}
	if st := c.Stats(); st.SparseBytes != 3*sparseBlock || st.Bytes != 3*sparseBlock {
		t.Fatalf("stats = %+v, want 3 blocks charged", st)
	}
	for _, idx := range []int64{0, 1, 2} {
		got, ok := c.Get(sparseKey, idx)
		if !ok || !bytes.Equal(got, blockBytes(idx)) {
			t.Fatalf("block %d from the sparse file = %d bytes, ok=%v", idx, len(got), ok)
		}
	}
	if _, ok := c.Get(sparseKey, 3); ok {
		t.Fatal("a block that was never fetched must miss, not read as the file's zeros")
	}
	if c.Has(sparseKey, 3) || c.HasRange(sparseKey, 3, 0, 16) {
		t.Fatal("a hole in the sparse file was reported as present")
	}
}

// TestSparseLayoutPublishesOnLastBlock: the file the reader filled is the
// file the cache publishes — a rename, not a copy.
func TestSparseLayoutPublishesOnLastBlock(t *testing.T) {
	c, err := newClosingCache(t, sparseOpts(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	fillBlocks(t, c, 0, 1, 2, 3, 4, 5, 6)
	if _, err := c.OpenWhole(sparseKey); err == nil {
		t.Fatal("an incomplete file must not open as a complete cache object")
	}
	fillBlocks(t, c, 7)

	part, bitmap, whole := partFiles(t, c)
	if part || bitmap || !whole {
		t.Fatalf("after the last block: part=%v bitmap=%v whole=%v", part, bitmap, whole)
	}
	f, err := c.OpenWhole(sparseKey)
	if err != nil {
		t.Fatalf("OpenWhole after completion: %v", err)
	}
	defer f.Close()
	got := make([]byte, sparseSize)
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	for idx := int64(0); idx < 8; idx++ {
		if !bytes.Equal(got[idx*sparseBlock:(idx+1)*sparseBlock], blockBytes(idx)) {
			t.Fatalf("published file has the wrong bytes at block %d", idx)
		}
	}
	if !c.Complete(sparseKey) {
		t.Fatal("a published file must report itself complete")
	}
	if st := c.Stats(); st.HydratedFiles != 1 || st.SparseBytes != 0 || st.WholeBytes != sparseSize {
		t.Fatalf("stats after publication = %+v", st)
	}
	if p, ok := c.HydratedPath(sparseKey); !ok || p != c.hydratedPath(sparseKey.hash()) {
		t.Fatalf("HydratedPath = %q, %v", p, ok)
	}
	noBlockFilesOnDisk(t, c)
}

// TestSparseLayoutSurvivesRestart: a sparse file whose bitmap is valid keeps
// serving the blocks it holds, and only the rest are fetched again.
func TestSparseLayoutSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	c, err := newClosingCache(t, sparseOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	fillBlocks(t, c, 0, 1, 5)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	c2, err := newClosingCache(t, sparseOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	if have, total := c2.Present(sparseKey); have != 3 || total != 8 {
		t.Fatalf("after the restart Present = %d/%d, want 3/8", have, total)
	}
	if st := c2.Stats(); st.SparseBytes != 3*sparseBlock {
		t.Fatalf("after the restart stats = %+v, want 3 blocks charged", st)
	}
	for _, idx := range []int64{0, 1, 5} {
		got, ok := c2.Get(sparseKey, idx)
		if !ok || !bytes.Equal(got, blockBytes(idx)) {
			t.Fatalf("block %d after the restart = %d bytes, ok=%v", idx, len(got), ok)
		}
	}
	for _, idx := range []int64{2, 3, 4, 6, 7} {
		if c2.Has(sparseKey, idx) {
			t.Fatalf("block %d was never written but survived the restart", idx)
		}
	}
	// The blocks the restart lost are the only ones refetched, and the file
	// still completes into the same sparse file.
	fillBlocks(t, c2, 2, 3, 4, 6, 7)
	if _, err := c2.OpenWhole(sparseKey); err != nil {
		t.Fatalf("the refilled file did not publish: %v", err)
	}
	noBlockFilesOnDisk(t, c2)
}

// TestSparseLayoutDropsUnusableBitmap: a sparse file cannot tell a hole from
// a block of zeros, so one whose bitmap is missing or was written for another
// block size is discarded rather than read.
func TestSparseLayoutDropsUnusableBitmap(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(t *testing.T, c *Cache)
	}{
		{"missing", func(t *testing.T, c *Cache) {
			if err := os.Remove(c.partBitmapPath(sparseKey.hash())); err != nil {
				t.Fatal(err)
			}
		}},
		{"another block size", func(t *testing.T, c *Cache) {
			p := c.partBitmapPath(sparseKey.hash())
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			swapped := strings.Replace(string(raw), "block=16384", "block=4096", 1)
			if swapped == string(raw) {
				t.Fatalf("bitmap %q does not name its block size", raw)
			}
			if err := os.WriteFile(p, []byte(swapped), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"unreadable", func(t *testing.T, c *Cache) {
			if err := os.WriteFile(c.partBitmapPath(sparseKey.hash()), []byte("part=1 block=\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c, err := newClosingCache(t, sparseOpts(dir))
			if err != nil {
				t.Fatal(err)
			}
			fillBlocks(t, c, 0, 1)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			tc.corrupt(t, c)

			c2, err := newClosingCache(t, sparseOpts(dir))
			if err != nil {
				t.Fatal(err)
			}
			if have, _ := c2.Present(sparseKey); have != 0 {
				t.Fatalf("an untrusted sparse file kept %d blocks", have)
			}
			if c2.Has(sparseKey, 0) {
				t.Fatal("an untrusted sparse file still served a block")
			}
			if st := c2.Stats(); st.SparseBytes != 0 || st.Bytes != 0 {
				t.Fatalf("an untrusted sparse file stayed charged: %+v", st)
			}
			part, bitmap, _ := partFiles(t, c2)
			if part || bitmap {
				t.Fatalf("an untrusted sparse file was left on disk: part=%v bitmap=%v", part, bitmap)
			}
		})
	}
}

// TestSparseLayoutPublishesACompleteReloadedPart: a crash between the last
// block and the rename leaves a full bitmap. The next start finishes the job
// rather than leaving a complete file nothing can pass through.
func TestSparseLayoutPublishesACompleteReloadedPart(t *testing.T) {
	dir := t.TempDir()
	c, err := newClosingCache(t, sparseOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	fillBlocks(t, c, 0, 1, 2, 3, 4, 5, 6)
	// Write the last block straight into the sparse file and claim it, which
	// is the state a crash just before the publishing rename leaves behind.
	fh := sparseKey.hash()
	p := c.lookupPart(fh)
	if p == nil {
		t.Fatal("the file is not in the sparse layout")
	}
	f, err := c.partFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(blockBytes(7), 7*sparseBlock); err != nil {
		t.Fatal(err)
	}
	p.bits.set(7)
	if err := c.writePartBitmap(fh, p.size, p.bits); err != nil {
		t.Fatal(err)
	}
	f.Sync()

	c2, err := newClosingCache(t, sparseOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.OpenWhole(sparseKey); err != nil {
		t.Fatalf("a complete sparse file did not publish on reload: %v", err)
	}
	if part, bitmap, whole := partFiles(t, c2); part || bitmap || !whole {
		t.Fatalf("after the reload: part=%v bitmap=%v whole=%v", part, bitmap, whole)
	}
}

// TestSparsePartIsEvictable: a half-written sparse file occupies the disk and
// must be able to give it back, whole, like any other cached object.
func TestSparsePartIsEvictable(t *testing.T) {
	opt := sparseOpts(t.TempDir())
	opt.MaxBytes = 3 * sparseBlock
	c, err := newClosingCache(t, opt)
	if err != nil {
		t.Fatal(err)
	}
	fillBlocks(t, c, 0, 1, 2)
	other := FileKey{Remote: "ali", RemoteID: "other", Version: "v1"}
	// A small file needs room the budget no longer has; the only victim is
	// the sparse file, and it goes as one object rather than block by block.
	if err := c.Put(other, 0, bytes.Repeat([]byte("x"), sparseBlock), sparseBlock); err != nil {
		t.Fatal(err)
	}
	if have, _ := c.Present(sparseKey); have != 0 {
		t.Fatalf("the evicted sparse file still claims %d blocks", have)
	}
	if c.Has(sparseKey, 0) {
		t.Fatal("the evicted sparse file still served a block")
	}
	if part, bitmap, _ := partFiles(t, c); part || bitmap {
		t.Fatalf("eviction left the sparse file on disk: part=%v bitmap=%v", part, bitmap)
	}
	if st := c.Stats(); st.SparseBytes != 0 || st.Bytes != sparseBlock {
		t.Fatalf("stats after eviction = %+v", st)
	}
}

// TestSparsePartIsPinnable: a pinned file is not a victim, half-written or not.
func TestSparsePartIsPinnable(t *testing.T) {
	opt := sparseOpts(t.TempDir())
	opt.MaxBytes = 3 * sparseBlock
	c, err := newClosingCache(t, opt)
	if err != nil {
		t.Fatal(err)
	}
	c.SetUserPin(sparseKey, true)
	fillBlocks(t, c, 0, 1, 2)
	other := FileKey{Remote: "ali", RemoteID: "other", Version: "v1"}
	if err := c.Put(other, 0, bytes.Repeat([]byte("x"), sparseBlock), sparseBlock); err != ErrNoSpace {
		t.Fatalf("admission over a pinned sparse file = %v, want ErrNoSpace", err)
	}
	if have, _ := c.Present(sparseKey); have != 3 {
		t.Fatalf("the pinned sparse file lost blocks: %d of 3", have)
	}
}

// TestSparseLayoutAdoptsABlockFilledInPieces: a large file read by a mix of
// random and sequential access still completes in one place. The block a
// random reader filled sub-block by sub-block joins the sparse file on
// promotion instead of waiting for a hydration with nothing to merge.
func TestSparseLayoutAdoptsABlockFilledInPieces(t *testing.T) {
	opt := sparseOpts(t.TempDir())
	opt.SubBlockSize = 4 << 10
	c, err := newClosingCache(t, opt)
	if err != nil {
		t.Fatal(err)
	}
	block := blockBytes(3)
	for off := int64(0); off < sparseBlock; off += 4 << 10 {
		if err := c.PutRange(sparseKey, 3, off, block[off:off+(4<<10)], sparseSize); err != nil {
			t.Fatal(err)
		}
	}
	waitFlushed(t, c)
	noBlockFilesOnDisk(t, c)
	got, ok := c.Get(sparseKey, 3)
	if !ok || !bytes.Equal(got, block) {
		t.Fatalf("the adopted block = %d bytes, ok=%v", len(got), ok)
	}
	fillBlocks(t, c, 0, 1, 2, 4, 5, 6, 7)
	if _, err := c.OpenWhole(sparseKey); err != nil {
		t.Fatalf("the file did not publish after adopting a block: %v", err)
	}
}

// TestSmallFilesKeepTheBlockLayout: below the threshold nothing changes —
// block files, then one hydration copy once the mount goes quiet.
func TestSmallFilesKeepTheBlockLayout(t *testing.T) {
	// A long HydrateAfter keeps the janitor out of the first half: with the
	// millisecond default of sparseOpts it can merge the blocks between
	// waitFlushed and the stat below, and the block files it then removes are
	// exactly what this half asserts. The second half fires it by hand.
	opt := sparseOpts(t.TempDir())
	opt.HydrateAfter = time.Hour
	c, err := newClosingCache(t, opt)
	if err != nil {
		t.Fatal(err)
	}
	small := FileKey{Remote: "ali", RemoteID: "small", Version: "v1"}
	const size = 2 * sparseBlock // below WholeLayoutMin
	for idx := int64(0); idx < 2; idx++ {
		if err := c.PutAsync(small, idx, blockBytes(idx), size); err != nil {
			t.Fatal(err)
		}
	}
	waitFlushed(t, c)
	if _, err := os.Stat(c.blockPath(small.hash(), 0)); err != nil {
		t.Fatalf("a small file must still be cached as block files: %v", err)
	}
	if _, err := os.Stat(c.partPath(small.hash())); err == nil {
		t.Fatal("a small file must not get a sparse whole file")
	}
	c.hydrateDue() // what the janitor does once the mount has gone quiet
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := c.HydratedPath(small); ok {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, ok := c.HydratedPath(small); !ok {
		t.Fatal("a small file is still merged by the hydration janitor")
	}
}

// TestSparseLayoutForgetRemovesTheFile: dropping a version must take its
// half-written file and its claim with it.
func TestSparseLayoutForgetRemovesTheFile(t *testing.T) {
	c, err := newClosingCache(t, sparseOpts(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	fillBlocks(t, c, 0, 1)
	if err := c.ForgetChecked(sparseKey); err != nil {
		t.Fatal(err)
	}
	if part, bitmap, _ := partFiles(t, c); part || bitmap {
		t.Fatalf("Forget left the sparse file behind: part=%v bitmap=%v", part, bitmap)
	}
	if st := c.Stats(); st.SparseBytes != 0 || st.Bytes != 0 {
		t.Fatalf("Forget left the sparse file charged: %+v", st)
	}
	if c.Has(sparseKey, 0) {
		t.Fatal("a forgotten sparse file still serves blocks")
	}
}

// TestSparseLayoutSettles: SettleFile is the pin barrier, and it must see a
// sparse file the same way it sees block files. It returns once the bytes are
// on disk, which for this layout is just before the publishing rename — so
// the file is complete and readable at once, and whole a moment later.
func TestSparseLayoutSettles(t *testing.T) {
	c, err := newClosingCache(t, sparseOpts(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	for idx := int64(0); idx < 8; idx++ {
		if err := c.PutAsync(sparseKey, idx, blockBytes(idx), sparseSize); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.SettleFile(ctx, sparseKey, sparseSize); err != nil {
		t.Fatalf("SettleFile: %v", err)
	}
	if !c.Complete(sparseKey) {
		t.Fatal("a settled file must report itself complete")
	}
	for idx := int64(0); idx < 8; idx++ {
		got, ok := c.Get(sparseKey, idx)
		if !ok || !bytes.Equal(got, blockBytes(idx)) {
			t.Fatalf("block %d of a settled file = %d bytes, ok=%v", idx, len(got), ok)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := c.HydratedPath(sparseKey); ok {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := c.OpenWhole(sparseKey); err != nil {
		t.Fatalf("a settled file did not publish: %v", err)
	}
}
