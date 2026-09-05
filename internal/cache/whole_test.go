package cache

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func wholeSource(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHydrateKeepsItsBudgetChargeAndReloads(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 64})
	for i := int64(0); i < 2; i++ {
		if err := c.Put(key, i, bytes.Repeat([]byte("a"), 16), 32); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Hydrate(key); err != nil {
		t.Fatal(err)
	}
	if s := c.Stats(); s.Bytes != 32 || s.WholeBytes != 32 || s.Blocks != 0 || s.ReservedBytes != 0 {
		t.Fatalf("hydrate hid/doubled charge: %+v", s)
	}
	c.Close()
	r, err := New(c.opt)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if s := r.Stats(); s.Bytes != 32 || s.WholeBytes != 32 {
		t.Fatalf("reload charge: %+v", s)
	}
	if got, ok := r.Get(key, 1); !ok || string(got) != string(bytes.Repeat([]byte("a"), 16)) {
		t.Fatal("reload changed data")
	}
}

func TestHydrateRequiresTemporaryHeadroomAndKeepsSourceOnFailure(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 32})
	for i := int64(0); i < 2; i++ {
		if err := c.Put(key, i, make([]byte, 16), 32); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Hydrate(key); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("temporary copy exceeded budget: %v", err)
	}
	if c.Stats().Bytes != 32 || c.Stats().ReservedBytes != 0 || !c.Has(key, 0) || !c.Has(key, 1) {
		t.Fatal("failed conversion lost blocks or reservation")
	}
}

func TestHardLinksShareOneChargeAndOneEviction(t *testing.T) {
	c, _ := newTest(t, Options{MaxBytes: 16, MaxBlocks: 1})
	src := wholeSource(t, "0123456789abcdef")
	other := FileKey{Remote: "other", RemoteID: "alias", Version: "v1"}
	for _, k := range []FileKey{key, other} {
		if err := c.LinkFile(k, src, 16); err != nil {
			t.Fatal(err)
		}
	}
	if s := c.Stats(); s.Bytes != 16 || s.HydratedFiles != 2 {
		t.Fatalf("hard links double counted: %+v", s)
	}
	c.SetUserPin(other, true)
	newKey := FileKey{Remote: "new", RemoteID: "x"}
	if err := c.Put(newKey, 0, []byte("x"), 20); !errors.Is(err, ErrNoSpace) {
		t.Fatal("pinned alias did not protect shared inode", err)
	}
	c.SetUserPin(other, false)
	if err := c.Put(newKey, 0, []byte("x"), 20); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.HydratedPath(key); ok {
		t.Fatal("eviction left a charged alias")
	}
	if _, ok := c.HydratedPath(other); ok {
		t.Fatal("eviction left second alias")
	}
	if b, err := os.ReadFile(src); err != nil || len(b) != 16 {
		t.Fatal("cache eviction removed external source")
	}
}

func TestOpenWholeRetainsSpaceAfterInvalidation(t *testing.T) {
	c, _ := newTest(t, Options{MaxBytes: 16})
	if err := c.LinkFile(key, wholeSource(t, "0123456789abcdef"), 16); err != nil {
		t.Fatal(err)
	}
	fd, err := c.OpenWhole(key)
	if err != nil {
		t.Fatal(err)
	}
	c.Forget(key)
	if s := c.Stats(); s.Bytes != 16 || s.LeasedBytes != 16 || s.HydratedFiles != 0 {
		t.Fatalf("unlinked open inode was falsely reclaimed: %+v", s)
	}
	other := FileKey{Remote: "r", RemoteID: "other"}
	if err := c.Put(other, 0, []byte("new"), 20); !errors.Is(err, ErrNoSpace) {
		t.Fatal("open retired inode did not consume capacity", err)
	}
	b, err := io.ReadAll(fd)
	if err != nil || string(b) != "0123456789abcdef" {
		t.Fatal("invalidation broke open reader", err)
	}
	if err := fd.Close(); err != nil {
		t.Fatal(err)
	}
	fd.Close()
	if c.Stats().Bytes != 0 {
		t.Fatal("lease close did not release charge")
	}
	if err := c.Put(other, 0, []byte("new"), 20); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicWholeReplacementDoesNotModifyOldReader(t *testing.T) {
	c, _ := newTest(t, Options{MaxBytes: 64})
	if err := c.LinkFile(key, wholeSource(t, "old bytes"), 9); err != nil {
		t.Fatal(err)
	}
	fd, err := c.OpenWhole(key)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if err := c.LinkFile(key, wholeSource(t, "new bytes"), 9); err != nil {
		t.Fatal(err)
	}
	old, _ := io.ReadAll(fd)
	now, ok := c.Get(key, 0)
	if string(old) != "old bytes" || !ok || string(now) != "new bytes" || c.Stats().Bytes != 18 {
		t.Fatalf("replacement changed leased inode: old=%q new=%q stats=%+v", old, now, c.Stats())
	}
	fd.Close()
	if c.Stats().Bytes != 9 {
		t.Fatal("old charge remained after lease close")
	}
}

func TestWholeAgeEvictionAndPins(t *testing.T) {
	c, clk := newTest(t, Options{MaxAge: time.Minute})
	if err := c.LinkFile(key, wholeSource(t, "old bytes"), 9); err != nil {
		t.Fatal(err)
	}
	c.SetUserPin(key, true)
	clk.advance(2 * time.Minute)
	if err := c.GC(); err != nil {
		t.Fatal(err)
	}
	if c.Stats().Bytes != 9 {
		t.Fatal("GC evicted pinned complete file")
	}
	c.SetUserPin(key, false)
	if err := c.GC(); err != nil {
		t.Fatal(err)
	}
	if c.Stats().Bytes != 0 {
		t.Fatal("age GC ignored complete file")
	}
}

func TestFailedAdoptionPreservesSourceAndExistingContent(t *testing.T) {
	c, _ := newTest(t, Options{MaxBytes: 9})
	if err := c.LinkFile(key, wholeSource(t, "old bytes"), 9); err != nil {
		t.Fatal(err)
	}
	c.SetUserPin(key, true)
	src := wholeSource(t, "new bytes")
	if err := c.AdoptFile(key, src, 9); !errors.Is(err, ErrNoSpace) {
		t.Fatal("expected bounded admission failure", err)
	}
	if got, err := os.ReadFile(src); err != nil || string(got) != "new bytes" {
		t.Fatal("failed adoption consumed source")
	}
	if got, ok := c.Get(key, 0); !ok || string(got) != "old bytes" {
		t.Fatal("failed adoption removed old content")
	}
}

func TestPinnedDurableReferenceIsVisibleEvenWhenOverBudget(t *testing.T) {
	c, _ := newTest(t, Options{MaxBytes: 4})
	if err := c.LinkPinnedFile(key, wholeSource(t, "committed"), 9); err != nil {
		t.Fatal(err)
	}
	if s := c.Stats(); s.Bytes != 9 || s.WholeBytes != 9 {
		t.Fatalf("durable reference hidden from budget: %+v", s)
	}
	other := FileKey{Remote: "r", RemoteID: "landed"}
	path, ok := c.HydratedPath(key)
	if !ok {
		t.Fatal("durable bytes missing")
	}
	if err := c.LinkFile(other, path, 9); err != nil {
		t.Fatal("zero-allocation upload identity transition failed", err)
	}
	c.Forget(key)
	if err := c.GC(); err != nil {
		t.Fatal(err)
	}
	if c.Stats().Bytes != 0 {
		t.Fatal("completed upload did not become reclaimable")
	}
}

func TestConcurrentAdmissionsRespectTheCombinedBudget(t *testing.T) {
	c, _ := newTest(t, Options{MaxBytes: 64, MaxBlocks: 4})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := FileKey{Remote: "r", RemoteID: string(rune('a' + i))}
			for n := 0; n < 20; n++ {
				if err := c.PutAsync(k, 0, make([]byte, 16), 1024); err != nil && !errors.Is(err, ErrNoSpace) {
					t.Error(err)
				}
				if s := c.Stats(); s.Bytes+s.ReservedBytes > 64 {
					t.Errorf("budget exceeded: %+v", s)
				}
			}
		}(i)
	}
	wg.Wait()
	c.Close()
	r, err := New(c.opt)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if s := r.Stats(); s.Bytes > 64 {
		t.Fatalf("retired background writes left unaccounted payload after restart: %+v", s)
	}
}

func TestConcurrentPartialAdmissionsStayBoundedAfterRestart(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 64, SubBlockSize: 16, MaxBytes: 64, MaxBlocks: 4})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := FileKey{Remote: "r", RemoteID: string(rune('a' + i))}
			for n := 0; n < 12; n++ {
				if err := c.PutRange(k, 0, 0, make([]byte, 16), 1024); err != nil && !errors.Is(err, ErrNoSpace) {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
	c.Close()
	r, err := New(c.opt)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if s := r.Stats(); s.Bytes > 64 || s.Blocks > 4 {
		t.Fatalf("partial writes resurrected after eviction: %+v", s)
	}
}

func TestOldAsyncFlushCannotOverwriteReplacement(t *testing.T) {
	// Retired generations may still be writing their temporary copies. Leave
	// headroom for each worker plus the current payload; tight-budget refusal
	// is covered separately by TestRetiredFlushKeepsItsReservation.
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 16 * (writeBehindWorkers + 1)})
	a, b := bytes.Repeat([]byte("a"), 16), bytes.Repeat([]byte("b"), 16)
	for i := 0; i < 200; i++ {
		if err := c.PutAsync(key, 0, a, 1024); err != nil {
			t.Fatal(err)
		}
		if err := c.Put(key, 0, b, 1024); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()
	if got, ok := c.Get(key, 0); !ok || !bytes.Equal(got, b) {
		t.Fatal("old flush replaced newer data", string(got))
	}
	r, err := New(c.opt)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, ok := r.Get(key, 0); !ok || !bytes.Equal(got, b) {
		t.Fatal("old flush survived restart", string(got))
	}
}
