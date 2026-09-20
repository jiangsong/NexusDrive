package cache

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// An object that outlives a retirement must be able to keep a descriptor
// again.
//
// dead says "the descriptor this entry holds has been given up; the last read
// using it closes it". It describes one descriptor, not the entry, and an
// entry can be retired and then read again: a hard link or a dedup keeps the
// object alive while one of its names is forgotten. If the flag survives into
// the next descriptor, every later read closes its descriptor the instant the
// read ends — the cache opens the file, installs it, and throws it away, for
// as long as the object lives — and the entry it leaves in readOpenWhole holds
// no descriptor at all, so the victim search can choose it, free nothing, and
// let the cap drift.
func TestRetiredObjectKeepsADescriptorAgain(t *testing.T) {
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
		t.Fatal("first read missed")
	}

	// Retire the descriptor with a read in flight, which is what defers the
	// close to that read and sets the flag. Forgetting one name of a shared
	// object reaches the same place; doing it by hand is what makes the
	// ordering — retire during the read, not before it — deterministic.
	c.mu.Lock()
	fs := c.files[first.hash()]
	if fs == nil || fs.whole == nil {
		c.mu.Unlock()
		t.Fatal("the linked file did not publish a hydrated object")
	}
	o := fs.whole
	o.readers++
	o.read.refs++
	c.retireWholeReadLocked(o)
	if !o.read.dead {
		c.mu.Unlock()
		t.Fatal("retiring a descriptor under an in-flight read did not defer its close")
	}
	c.mu.Unlock()
	c.releaseWholeRead(o)

	// The object is still published, so reads go on. Each one must find the
	// descriptor the one before it left.
	for i := 0; i < 4; i++ {
		if _, ok := c.ReadAt(second, 0, 0, dst); !ok {
			t.Fatalf("read %d after the retirement missed", i)
		}
		c.mu.Lock()
		kept := o.read.f != nil
		c.mu.Unlock()
		if !kept {
			t.Fatalf("read %d closed its own descriptor: the retired flag outlived the descriptor it described", i)
		}
	}
	c.mu.Lock()
	held := len(c.readOpenWhole)
	c.mu.Unlock()
	if held != 1 {
		t.Fatalf("hydrated descriptors held = %d, want 1", held)
	}
	// One open for the first read, one for the read after the retirement, and
	// nothing after that.
	if log.n != 2 {
		t.Fatalf("opens = %d, want 2: reads after a retirement stopped sharing a descriptor", log.n)
	}
}
