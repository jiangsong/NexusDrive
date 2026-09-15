//go:build linux || darwin

package cache

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// diskUse measures what a workload really wrote to the local disk: for every
// inode the cache ever created under its directory, the most bytes that inode
// ever held. Inodes rather than paths, because publishing a cached file is a
// rename — the same bytes under a new name are not a second write, while a
// block file plus the hydrated file that copies it are two.
//
// Each inode is charged the smaller of its allocated blocks and its apparent
// length: allocation rounds a block file up to the filesystem's own block,
// and a sparse file's apparent length says nothing about what was written.
type diskUse struct{ seen map[uint64]int64 }

func newDiskUse() *diskUse { return &diskUse{seen: map[uint64]int64{}} }

// sample walks the cache directory and records what it finds. Bookkeeping
// sidecars are skipped: they are tens of bytes, and what is being measured is
// the file content.
func (d *diskUse) sample(t *testing.T, c *Cache) {
	t.Helper()
	err := filepath.WalkDir(c.Dir(), func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil // a file the cache removed mid-walk is not an error
		}
		name := e.Name()
		// Bookkeeping, not file content: the per-file identity, the
		// whole-file bitmap, and a partial block's sidecar — which shares
		// the ".part" suffix with the sparse file but lives under blocks/.
		inBlocks := strings.Contains(p, string(os.PathSeparator)+"blocks"+string(os.PathSeparator))
		if strings.HasSuffix(name, ".key") || strings.Contains(name, ".part.bitmap") ||
			inBlocks && strings.HasSuffix(name, ".part") {
			return nil
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		used := int64(st.Blocks) * 512
		if info.Size() < used {
			used = info.Size()
		}
		if used > d.seen[uint64(st.Ino)] {
			d.seen[uint64(st.Ino)] = used
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (d *diskUse) total() int64 {
	var n int64
	for _, v := range d.seen {
		n += v
	}
	return n
}

// coldRead fills a file block by block the way a cold sequential read does,
// sampling the disk after every block so files a later step removes are still
// counted, and waits for the cache to publish the complete file.
func coldRead(t *testing.T, c *Cache) *diskUse {
	t.Helper()
	use := newDiskUse()
	for idx := int64(0); idx < 8; idx++ {
		if err := c.PutAsync(sparseKey, idx, blockBytes(idx), sparseSize); err != nil {
			t.Fatal(err)
		}
		waitFlushed(t, c)
		use.sample(t, c)
	}
	// The caller holds the janitor off during the fill (a long HydrateAfter)
	// so the last block file is sampled before hydration removes it; now let
	// it run, the way it would once the mount has gone quiet.
	c.hydrateDue()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		use.sample(t, c)
		if _, ok := c.HydratedPath(sparseKey); ok {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	use.sample(t, c)
	if _, ok := c.HydratedPath(sparseKey); !ok {
		t.Fatal("the file never became a complete cache object")
	}
	return use
}

// TestSparseLayoutWritesTheFileOnce is the point of the layout: a cold read
// of a large file puts the file's size on the local disk, once. The control
// is the same workload with the layout switched off, which writes it twice —
// once as block files and again as the hydration copy of those blocks.
func TestSparseLayoutWritesTheFileOnce(t *testing.T) {
	// A millisecond HydrateAfter let the janitor merge and remove the last
	// block file between waitFlushed and the sample that should have counted
	// it, under-reporting the control by one block; coldRead fires the
	// janitor itself once every block has been sampled.
	sparse := sparseOpts(t.TempDir())
	sparse.HydrateAfter = time.Hour
	c, err := newClosingCache(t, sparse)
	if err != nil {
		t.Fatal(err)
	}
	if got := coldRead(t, c).total(); got != sparseSize {
		t.Fatalf("the sparse layout wrote %d bytes for a %d-byte file", got, sparseSize)
	}

	opt := sparseOpts(t.TempDir())
	opt.HydrateAfter = time.Hour
	opt.WholeLayoutMin = -1 // block files and a hydration copy, as before
	old, err := newClosingCache(t, opt)
	if err != nil {
		t.Fatal(err)
	}
	if got := coldRead(t, old).total(); got != 2*sparseSize {
		t.Fatalf("the block layout wrote %d bytes for a %d-byte file, want the %d this test exists to remove", got, sparseSize, 2*sparseSize)
	}
}
