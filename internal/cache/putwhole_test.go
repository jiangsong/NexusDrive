package cache

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// noBlockFilesOnDisk fails the test if any block payload or sidecar file
// remains anywhere under the cache's blocks/ directory. The per-file ".key"
// identity sidecar is not a block: LinkFile/AdoptFile leave it in place for a
// fully hydrated file too (reload's index depends on it), so it is excluded.
func noBlockFilesOnDisk(t *testing.T, c *Cache) {
	t.Helper()
	blocksDir := filepath.Join(c.Dir(), "blocks")
	err := filepath.WalkDir(blocksDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasSuffix(d.Name(), ".key") {
			t.Fatalf("leftover block file %s", p)
		}
		return err
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// noTempFilesInHydrated fails the test if an unpublished temporary remains
// under the cache's hydrated/ directory.
func noTempFilesInHydrated(t *testing.T, c *Cache) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(c.Dir(), "hydrated"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("leaked temp file(s) under hydrated/: %v", entries)
	}
}

func TestPutWholeInstallsAllBlocksWithNoBlockFiles(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 1 << 20})
	full := []byte("0123456789abcdef0123456789ABCDEF") // 32 bytes, 2 blocks
	size := int64(len(full))
	if err := c.PutWhole(key, bytes.NewReader(full), size); err != nil {
		t.Fatal(err)
	}
	if !c.Has(key, 0) || !c.Has(key, 1) {
		t.Fatal("PutWhole did not make blocks present")
	}
	for i := int64(0); i < 2; i++ {
		got := make([]byte, 16)
		n, ok := c.ReadAt(key, i, 0, got)
		want := full[i*16 : i*16+16]
		if !ok || n != 16 || !bytes.Equal(got, want) {
			t.Fatalf("block %d = %q ok=%v, want %q", i, got[:n], ok, want)
		}
	}
	if !c.Complete(key) {
		t.Fatal("PutWhole did not mark the file complete")
	}
	if s := c.Stats(); s.Blocks != 0 {
		t.Fatalf("PutWhole left block-cache entries: %+v", s)
	}
	noBlockFilesOnDisk(t, c)
}

func TestPutWholeChargesBytesOnce(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 1 << 20})
	full := bytes.Repeat([]byte("z"), 32)
	size := int64(len(full))
	if err := c.PutWhole(key, bytes.NewReader(full), size); err != nil {
		t.Fatal(err)
	}
	if s := c.Stats(); s.Bytes != size || s.WholeBytes != size {
		t.Fatalf("charged more than once: %+v", s)
	}
}

func TestPutWholeReplacesPartialBlocksWithoutDoubleCharge(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 1 << 20})
	full := bytes.Repeat([]byte("y"), 32)
	size := int64(len(full))
	// An incomplete partial block set for the key, installed first.
	if err := c.Put(key, 0, full[:16], size); err != nil {
		t.Fatal(err)
	}
	if err := c.PutWhole(key, bytes.NewReader(full), size); err != nil {
		t.Fatal(err)
	}
	if s := c.Stats(); s.Blocks != 0 || s.Bytes != size {
		t.Fatalf("partial blocks not cleanly replaced: %+v", s)
	}
	noBlockFilesOnDisk(t, c)
	got := make([]byte, 16)
	if n, ok := c.ReadAt(key, 1, 0, got); !ok || n != 16 || !bytes.Equal(got, full[16:32]) {
		t.Fatalf("second block after replace = %q ok=%v", got[:n], ok)
	}
}

func TestPutWholeReturnsErrNoSpaceOverBudget(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 8})
	data := bytes.Repeat([]byte("x"), 16)
	if err := c.PutWhole(key, bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("expected ErrNoSpace, got %v", err)
	}
	if _, ok := c.HydratedPath(key); ok {
		t.Fatal("over-budget PutWhole installed something")
	}
	if s := c.Stats(); s.Bytes != 0 {
		t.Fatalf("over-budget PutWhole left a charge: %+v", s)
	}
	noTempFilesInHydrated(t, c)
}

func TestPutWholeShortReaderInstallsNothing(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 1 << 20})
	short := bytes.NewReader([]byte("short"))
	if err := c.PutWhole(key, short, 16); err == nil {
		t.Fatal("expected an error for a short reader")
	}
	if _, ok := c.HydratedPath(key); ok {
		t.Fatal("short reader installed something")
	}
	if s := c.Stats(); s.Bytes != 0 {
		t.Fatalf("short reader left a charge: %+v", s)
	}
	noTempFilesInHydrated(t, c)
}

func TestPutWholePreservesPin(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 1 << 20})
	full := bytes.Repeat([]byte("p"), 16)
	if err := c.Put(key, 0, full, int64(len(full))); err != nil {
		t.Fatal(err)
	}
	c.SetUserPin(key, true)
	if err := c.PutWhole(key, bytes.NewReader(full), int64(len(full))); err != nil {
		t.Fatal(err)
	}
	if !c.IsPinned(key) {
		t.Fatal("PutWhole lost the pin")
	}
}

// TestPutWholeConcurrentWithPutStaysConsistent runs PutWhole and Put for the
// same key concurrently under -race. Both hold admitMu for their whole call,
// so they must not interleave; this checks that holds under real contention
// and that the settled state after the race is never double-charged.
func TestPutWholeConcurrentWithPutStaysConsistent(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 1 << 20})
	full := bytes.Repeat([]byte("abcdefgh"), 8) // 64 bytes, 4 blocks of 16
	size := int64(len(full))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for n := 0; n < 200; n++ {
			for i := int64(0); i < 4; i++ {
				off := i * 16
				if err := c.Put(key, i, full[off:off+16], size); err != nil {
					t.Error(err)
				}
			}
		}
	}()
	go func() {
		defer wg.Done()
		for n := 0; n < 200; n++ {
			if err := c.PutWhole(key, bytes.NewReader(full), size); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()

	// Settle to a known state, then check it is not corrupted or charged
	// more than once for the same content.
	if err := c.PutWhole(key, bytes.NewReader(full), size); err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 4; i++ {
		got := make([]byte, 16)
		n, ok := c.ReadAt(key, i, 0, got)
		want := full[i*16 : i*16+16]
		if !ok || n != 16 || !bytes.Equal(got, want) {
			t.Fatalf("block %d after race = %q ok=%v, want %q", i, got[:n], ok, want)
		}
	}
	if s := c.Stats(); s.Bytes != size {
		t.Fatalf("race left an inconsistent charge: %+v", s)
	}
}
