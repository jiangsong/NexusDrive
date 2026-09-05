package cache

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// TestPutAsyncServesFromMemoryUntilWritten: a block put with write-behind
// is readable at once, lands on disk shortly after, and the file hydrates
// once every block is on disk — never while some are still in memory.
func TestPutAsyncServesFromMemoryUntilWritten(t *testing.T) {
	dir := t.TempDir()
	const block = 64 << 10
	c, err := newClosingCache(t, Options{Dir: dir, BlockSize: block, HydrateAfter: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v"}
	b0 := bytes.Repeat([]byte("a"), block)
	b1 := bytes.Repeat([]byte("b"), block/2)
	if err := c.PutAsync(k, 0, b0, int64(len(b0)+len(b1))); err != nil {
		t.Fatal(err)
	}
	if !c.Has(k, 0) {
		t.Fatal("block not present right after PutAsync")
	}
	buf := make([]byte, 16)
	if n, ok := c.ReadAt(k, 0, 100, buf); !ok || n != 16 || !bytes.Equal(buf, b0[100:116]) {
		t.Fatalf("read through memory: %d %v %q", n, ok, buf)
	}
	if err := c.PutAsync(k, 1, b1, int64(len(b0)+len(b1))); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := c.HydratedPath(k); ok && c.WriteBehindPending() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if p := c.WriteBehindPending(); p != 0 {
		t.Fatalf("%d bytes still pending after 5 s", p)
	}
	p, ok := c.HydratedPath(k)
	if !ok {
		t.Fatal("the complete file was not hydrated after its blocks were written")
	}
	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, append(append([]byte{}, b0...), b1...)) {
		t.Fatalf("hydrated content wrong (%v, %d bytes)", err, len(got))
	}
	if n, ok := c.ReadAt(k, 1, 0, buf); !ok || !bytes.Equal(buf[:n], b1[:16]) {
		t.Fatalf("read after flush: %d %v", n, ok)
	}
}

// TestForgetWhilePendingDropsTheMemory: forgetting a file whose blocks are
// still in memory releases the accounting, and the later flush is a no-op.
func TestForgetWhilePendingDropsTheMemory(t *testing.T) {
	c, err := newClosingCache(t, Options{Dir: t.TempDir(), BlockSize: 4096, HydrateAfter: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	k := FileKey{Remote: "r", RemoteID: "gone", Version: "v"}
	for i := 0; i < 8; i++ {
		if err := c.PutAsync(k, int64(i), bytes.Repeat([]byte{byte(i)}, 4096), 8*4096); err != nil {
			t.Fatal(err)
		}
	}
	c.Forget(k)
	if p := c.WriteBehindPending(); p != 0 {
		t.Fatalf("%d bytes still accounted after Forget", p)
	}
	time.Sleep(50 * time.Millisecond)
	if c.Has(k, 0) {
		t.Fatal("a forgotten block came back after its flush")
	}
}

// newClosingCache is New plus a cleanup that stops the write-behind workers
// before the test's directory is removed.
func newClosingCache(t *testing.T, o Options) (*Cache, error) {
	t.Helper()
	c, err := New(o)
	if err == nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, err
}
