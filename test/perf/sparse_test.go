//go:build linux || darwin

package perf

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"cloudfs/internal/cache"
)

// The other baselines here count provider calls. This one counts local
// writes, the cost docs/pool-v2.md §4.4 exists to remove: a cold read of a
// large file used to write the local disk twice — once as one file per block,
// and again when hydration copied those blocks into a whole file.

// localWrites measures what a workload wrote to the cache's own disk. It
// charges every inode the cache created the most bytes that inode ever held,
// counting the smaller of its allocated blocks and its apparent length.
// Inodes rather than paths, because publishing a file is a rename: the same
// bytes under a new name are not a second write, while a block file and the
// hydrated file that copies it are two.
type localWrites struct {
	dir  string
	seen map[uint64]int64
}

func newLocalWrites(dir string) *localWrites {
	return &localWrites{dir: dir, seen: map[uint64]int64{}}
}

func (w *localWrites) sample(t *testing.T) {
	t.Helper()
	err := filepath.WalkDir(w.dir, func(p string, e os.DirEntry, err error) error {
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
		if used > w.seen[uint64(st.Ino)] {
			w.seen[uint64(st.Ino)] = used
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (w *localWrites) total() int64 {
	var n int64
	for _, v := range w.seen {
		n += v
	}
	return n
}

// coldSequentialRead reads the whole seeded file through the filesystem the
// way a player does, sampling the cache directory as it goes so files a later
// step removes are still counted, and waits for the file to become one
// complete cache object.
func coldSequentialRead(t *testing.T, h *harness, size int64) *localWrites {
	t.Helper()
	ctx := context.Background()
	w := newLocalWrites(h.cache.Dir())
	h.fake.Seed("movie.bin", bytes.Repeat([]byte("v"), int(size)))
	if _, err := h.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	node, err := h.fs.Meta().Resolve(ctx, "/movie.bin")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := h.fs.Open(ctx, node.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.fs.Release(ctx, handle)
	buf := make([]byte, 64<<10)
	for off := int64(0); off < size; off += int64(len(buf)) {
		if _, err := h.fs.Read(ctx, handle, buf, off); err != nil {
			t.Fatal(err)
		}
		w.sample(t)
	}
	key := cache.FileKey{Remote: "ali", RemoteID: node.RemoteID, Version: node.Version}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		w.sample(t)
		if _, ok := h.cache.HydratedPath(key); ok {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	w.sample(t)
	if _, ok := h.cache.HydratedPath(key); !ok {
		t.Fatal("the file never became a complete cache object")
	}
	return w
}

// TestLargeFileColdReadWritesTheDiskOnce: above cache.WholeLayoutMin a cold
// read puts the file's size on the local disk once, where the block layout
// puts it there twice. The tolerance is the handful of sub-block fetches a
// handle makes before its run is recognised as sequential: those still take
// the block-file path and are copied into the sparse file when they complete.
func TestLargeFileColdReadWritesTheDiskOnce(t *testing.T) {
	const blockSize = 64 << 10
	const size = 4 << 20
	const slack = 4 * blockSize

	sparse := newHarnessCache(t, cache.Options{
		BlockSize: blockSize, WholeLayoutMin: 1 << 20, HydrateAfter: 5 * time.Millisecond,
	}, 8, 0, 0)
	got := coldSequentialRead(t, sparse, size).total()
	if got < size || got > size+slack {
		t.Fatalf("the sparse layout wrote %d bytes reading a %d-byte file, want one copy", got, size)
	}
	if read := sparse.fake.ReadBytes(); read > size+slack {
		t.Fatalf("the sparse layout read %d bytes from the provider for a %d-byte file", read, size)
	}
	t.Logf("sparse layout: %d local bytes, %d provider bytes, %d ReadRange calls",
		got, sparse.fake.ReadBytes(), sparse.fake.Calls("ReadRange"))

	// The control is the same read with the layout switched off: block files
	// first, then the hydration copy of them.
	blocks := newHarnessCache(t, cache.Options{
		BlockSize: blockSize, WholeLayoutMin: -1, HydrateAfter: 5 * time.Millisecond,
	}, 8, 0, 0)
	was := coldSequentialRead(t, blocks, size).total()
	if was < 2*size-slack {
		t.Fatalf("the block layout wrote %d bytes reading a %d-byte file; this test's control no longer shows the double write", was, size)
	}
	if was-got < size-slack {
		t.Fatalf("the sparse layout saved only %d of the %d bytes the second write costs", was-got, size)
	}
}
