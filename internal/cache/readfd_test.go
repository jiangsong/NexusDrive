package cache

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// openLog counts the read path's opens and keeps the descriptors it handed
// out, so a test can tell whether the cache gave them up again.
type openLog struct {
	n     int
	files []*os.File
}

func (o *openLog) open(name string) (*os.File, error) {
	f, err := os.Open(name)
	if err == nil {
		o.n++
		o.files = append(o.files, f)
	}
	return f, err
}

// stillOpen closes and counts the descriptors the cache never closed. Close on
// an already-closed *os.File reports an error, which is how a descriptor the
// cache did give up is told apart from one it leaked. It is destructive, so a
// test calls it once, at the end.
func (o *openLog) stillOpen() int {
	n := 0
	for _, f := range o.files {
		if f.Close() == nil {
			n++
		}
	}
	return n
}

// A cached block is an immutable file, so reading it N times should cost one
// open(2), not N. The FUSE path leases a descriptor per handle; the MCP read
// path (FS.ReadFileRange) does not, and paid an open and a close per range.
func TestRepeatedBlockReadsReuseOneDescriptor(t *testing.T) {
	var log openLog
	c, _ := newTest(t, Options{BlockSize: 16, Open: log.open})
	data := []byte("0123456789abcdef")
	// fileSize 32 leaves the file incomplete, so nothing hydrates underneath.
	if err := c.Put(key, 0, data, 32); err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, 16)
	for i := 0; i < 8; i++ {
		n, ok := c.ReadAt(key, 0, 0, dst)
		if !ok || n != 16 || !bytes.Equal(dst, data) {
			t.Fatalf("read %d: n=%d ok=%v got=%q", i, n, ok, dst)
		}
	}
	if log.n != 1 {
		t.Fatalf("opens = %d for 8 reads, want 1", log.n)
	}
}

// The hydrated path was worse than the block path: every read called
// OpenWhole, which opens the file under the cache lock.
func TestRepeatedHydratedReadsReuseOneDescriptor(t *testing.T) {
	var log openLog
	c, _ := newTest(t, Options{BlockSize: 16, Open: log.open})
	data := []byte("0123456789abcdef")
	if err := c.Put(key, 0, data, 16); err != nil {
		t.Fatal(err)
	}
	if err := c.Hydrate(key); err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, 16)
	for i := 0; i < 8; i++ {
		n, ok := c.ReadAt(key, 0, 0, dst)
		if !ok || n != 16 || !bytes.Equal(dst, data) {
			t.Fatalf("read %d: n=%d ok=%v got=%q", i, n, ok, dst)
		}
	}
	if log.n != 1 {
		t.Fatalf("opens = %d for 8 hydrated reads, want 1", log.n)
	}
}

// Every way a block file can go away has to close the descriptor naming it and
// give up its slot, and a later read of the same key must see the new bytes.
func TestRetiredBlocksReleaseTheirDescriptor(t *testing.T) {
	old, fresh := bytes.Repeat([]byte("a"), 16), bytes.Repeat([]byte("b"), 16)
	for _, tc := range []struct {
		name   string
		retire func(t *testing.T, c *Cache)
	}{
		{"forget", func(t *testing.T, c *Cache) { c.Forget(key) }},
		{"replace", func(t *testing.T, c *Cache) {}}, // the Put below replaces it
		{"evict", func(t *testing.T, c *Cache) {
			if !c.evictOne() {
				t.Fatal("nothing to evict")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log openLog
			c, _ := newTest(t, Options{BlockSize: 16, Open: log.open})
			if err := c.Put(key, 0, old, 32); err != nil {
				t.Fatal(err)
			}
			dst := make([]byte, 16)
			if _, ok := c.ReadAt(key, 0, 0, dst); !ok {
				t.Fatal("first read missed")
			}
			tc.retire(t, c)
			if err := c.Put(key, 0, fresh, 32); err != nil {
				t.Fatal(err)
			}
			if _, ok := c.ReadAt(key, 0, 0, dst); !ok || !bytes.Equal(dst, fresh) {
				t.Fatalf("stale descriptor served %q", dst)
			}
			c.mu.Lock()
			held := len(c.readOpen)
			c.mu.Unlock()
			if held != 1 {
				t.Fatalf("descriptors held = %d, want 1 (the current block)", held)
			}
			// The first block's descriptor is the one the retirement had to
			// close; the second is still legitimately held.
			if n := log.stillOpen(); n != 1 {
				t.Fatalf("descriptors left open = %d, want 1", n)
			}
		})
	}
}

// Replacing the hydrated file makes a new inode; the descriptor for the old
// one must be closed, not handed to the next read.
func TestReplacedHydratedFileReleasesItsDescriptor(t *testing.T) {
	var log openLog
	c, _ := newTest(t, Options{BlockSize: 16, Open: log.open})
	old, fresh := bytes.Repeat([]byte("a"), 16), bytes.Repeat([]byte("b"), 16)
	if err := c.Put(key, 0, old, 16); err != nil {
		t.Fatal(err)
	}
	if err := c.Hydrate(key); err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, 16)
	if _, ok := c.ReadAt(key, 0, 0, dst); !ok || !bytes.Equal(dst, old) {
		t.Fatalf("first read: %v %q", ok, dst)
	}
	if err := c.PutWhole(key, bytes.NewReader(fresh), 16); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.ReadAt(key, 0, 0, dst); !ok || !bytes.Equal(dst, fresh) {
		t.Fatalf("stale hydrated descriptor served %q", dst)
	}
	c.Forget(key)
	c.mu.Lock()
	held := len(c.readOpenWhole)
	c.mu.Unlock()
	if held != 0 {
		t.Fatalf("hydrated descriptors held after forget = %d, want 0", held)
	}
	if n := log.stillOpen(); n != 0 {
		t.Fatalf("descriptors left open = %d, want 0", n)
	}
}

// Hard links share one object, so forgetting one name does not retire the
// object and cannot rely on the object's own release to give the descriptor
// up. The name it was opened under is gone all the same.
func TestForgettingOneHardLinkReleasesItsDescriptor(t *testing.T) {
	var log openLog
	c, _ := newTest(t, Options{BlockSize: 16, Open: log.open})
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, bytes.Repeat([]byte("a"), 16), 0o600); err != nil {
		t.Fatal(err)
	}
	first := FileKey{Remote: "r", RemoteID: "a", Version: "v"}
	second := FileKey{Remote: "r", RemoteID: "b", Version: "v"}
	for _, k := range []FileKey{first, second} {
		if err := c.LinkPinnedFile(k, src, 16); err != nil {
			t.Fatal(err)
		}
	}
	dst := make([]byte, 16)
	if _, ok := c.ReadAt(first, 0, 0, dst); !ok {
		t.Fatal("read missed")
	}
	c.Forget(first)
	c.mu.Lock()
	held := len(c.readOpenWhole)
	c.mu.Unlock()
	if held != 0 {
		t.Fatalf("descriptors held for a forgotten name = %d, want 0", held)
	}
	// The object itself survives: the other link still reads.
	if _, ok := c.ReadAt(second, 0, 0, dst); !ok {
		t.Fatal("the surviving hard link stopped reading")
	}
	if n := log.stillOpen(); n != 1 {
		t.Fatalf("descriptors left open = %d, want 1 (the surviving link)", n)
	}
}

// Descriptors are a process-wide resource: the cache may not hold one per
// cached block, and the ones it drops to stay under the cap must be closed.
func TestCachedReadDescriptorsAreBounded(t *testing.T) {
	var log openLog
	c, _ := newTest(t, Options{BlockSize: 16, Open: log.open})
	dst := make([]byte, 16)
	for i := 0; i < maxOpenReads+32; i++ {
		k := FileKey{Remote: "r", RemoteID: fmt.Sprintf("f-%d", i), Version: "v"}
		if err := c.Put(k, 0, bytes.Repeat([]byte("x"), 16), 32); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.ReadAt(k, 0, 0, dst); !ok {
			t.Fatalf("read %d missed", i)
		}
	}
	c.mu.Lock()
	held := len(c.readOpen)
	c.mu.Unlock()
	if held != maxOpenReads {
		t.Fatalf("descriptors held = %d, cap %d", held, maxOpenReads)
	}
	if n := log.stillOpen(); n != maxOpenReads {
		t.Fatalf("descriptors left open = %d, want %d", n, maxOpenReads)
	}
}

// Closing the cache must not leave descriptors behind for the directory the
// daemon — or a test — is about to remove.
func TestCloseRetiresCachedReadDescriptors(t *testing.T) {
	var log openLog
	c, _ := newTest(t, Options{BlockSize: 16, Open: log.open})
	if err := c.Put(key, 0, bytes.Repeat([]byte("a"), 16), 32); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(key, 1, bytes.Repeat([]byte("b"), 16), 32); err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, 16)
	for idx := int64(0); idx < 2; idx++ {
		if _, ok := c.ReadAt(key, idx, 0, dst); !ok {
			t.Fatalf("read %d missed", idx)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	held, wholes := len(c.readOpen), len(c.readOpenWhole)
	c.mu.Unlock()
	if held != 0 || wholes != 0 {
		t.Fatalf("descriptors after Close: %d blocks, %d hydrated", held, wholes)
	}
	if n := log.stillOpen(); n != 0 {
		t.Fatalf("descriptors left open after Close = %d", n)
	}
}
