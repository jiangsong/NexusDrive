package cache

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingReader blocks on its first Read call until resume is closed, after
// closing started so a test can wait for the read to have actually begun.
// It is used to hold PutWhole in the middle of its stream, with neither lock
// held, so a concurrent operation can be driven while it waits.
type blockingReader struct {
	data    []byte
	started chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (r *blockingReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.resume
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

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

// noTempFilesInHydrated fails the test if anything at all remains under the
// cache's hydrated/ directory. Use it only where nothing should have been
// published either — a published hydrated file is named after the plain
// file hash, with no leading dot, and belongs there.
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

// noOrphanTempsInHydrated fails the test if an unpublished temporary (its
// name always starts with ".") remains under the cache's hydrated/
// directory, while allowing whatever legitimately got published there.
func noOrphanTempsInHydrated(t *testing.T, c *Cache) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(c.Dir(), "hydrated"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("leaked temp file under hydrated/: %s", e.Name())
		}
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
	if s := c.Stats(); s.ReservedBytes != 0 {
		t.Fatalf("reservation not released after a successful install: %+v", s)
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
	if s := c.Stats(); s.ReservedBytes != 0 {
		t.Fatalf("reservation not released after ErrNoSpace: %+v", s)
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
	if s := c.Stats(); s.ReservedBytes != 0 {
		t.Fatalf("reservation not released after a short-reader error: %+v", s)
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

// TestPutWholePreservesInternalPinAcrossInstall pins through the internal
// fs.pinned path (Pin), not through the persistent, independent userPinned
// path SetUserPin sets (which TestPutWholePreservesPin already covers and
// which installTemp never touches at all). installTemp's
// "fs.pinned = fs.pinned || pinned" would still pass a test that only
// checked userPinned even if that OR degraded to a plain assignment
// (fs.pinned = pinned): with no prior userPinned, plain assignment and OR
// agree once PutWhole's own pinned argument is false. This test pins
// fs.pinned directly, replaces the content through PutWhole, and then
// checks eviction immunity for a competing key still comes from it -
// verified to fail under a plain-assignment regression by temporarily
// editing installTemp to fs.pinned = pinned and re-running (see task-6
// fix report for that RED/GREEN evidence).
func TestPutWholePreservesInternalPinAcrossInstall(t *testing.T) {
	// MaxBytes(2) covers the brief moment where the old 1-byte pinned block
	// and the new 1-byte replacement both count against the budget (neither
	// Put's nor PutWhole's admission credits a same-key replacement against
	// what it is about to remove), but leaves no room to spare afterward.
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 2})
	old, replacement := []byte("o"), []byte("r")
	if err := c.Put(key, 0, old, int64(len(old))); err != nil {
		t.Fatal(err)
	}
	c.Pin(key, true)
	if err := c.PutWhole(key, bytes.NewReader(replacement), int64(len(replacement))); err != nil {
		t.Fatal(err)
	}
	if !c.IsPinned(key) {
		t.Fatal("PutWhole lost the internal pin")
	}
	// Only 1 of the 2-byte budget remains; a different key needing more
	// than that must be refused rather than evicting the pinned object.
	other := FileKey{Remote: "r", RemoteID: "other"}
	if err := c.Put(other, 0, []byte("xy"), 20); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("expected ErrNoSpace, pinned whole object was evicted instead: %v", err)
	}
	if s := c.Stats(); s.Bytes != int64(len(replacement)) {
		t.Fatalf("pinned whole object was evicted: %+v", s)
	}
}

// TestPutWholeContentIsReadableViaOpenWhole checks the acceptance criterion
// that OpenWhole succeeds against a PutWhole-installed key, since that is
// what FUSE passthrough/splice depends on.
func TestPutWholeContentIsReadableViaOpenWhole(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 1 << 20})
	full := []byte("read this whole file through OpenWhole")
	size := int64(len(full))
	if err := c.PutWhole(key, bytes.NewReader(full), size); err != nil {
		t.Fatal(err)
	}
	f, err := c.OpenWhole(key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("OpenWhole content = %q, want %q", got, full)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestPutWholeDoesNotBlockUnrelatedKeysDuringStream is the primary
// regression test for the admitMu fix: PutWhole must not hold the
// cache-wide admission lock across the caller's (potentially slow, network)
// Read calls, or every other file's Put/Hydrate/PutWhole/installWhole would
// stall behind one fetch. Key A's stream is held open on a blocking reader;
// key B's unrelated Put must complete while A is still blocked.
func TestPutWholeDoesNotBlockUnrelatedKeysDuringStream(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 1 << 20})
	full := bytes.Repeat([]byte("q"), 16)
	size := int64(len(full))
	keyA := FileKey{Remote: "r", RemoteID: "a"}
	keyB := FileKey{Remote: "r", RemoteID: "b"}

	br := &blockingReader{data: append([]byte{}, full...), started: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- c.PutWhole(keyA, br, size) }()

	select {
	case <-br.started:
	case <-time.After(5 * time.Second):
		t.Fatal("PutWhole never started reading")
	}

	putDone := make(chan error, 1)
	go func() { putDone <- c.Put(keyB, 0, full, size) }()

	select {
	case err := <-putDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Put for an unrelated key was blocked behind PutWhole's in-flight stream")
	}

	close(br.resume)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PutWhole did not finish after its reader was unblocked")
	}
}

// TestPutWholeAbortsCleanlyWhenSupersededDuringStream: a competing, faster
// install for the same key lands while the first PutWhole's stream is still
// blocked. The late (stale-generation) install must not clobber the winner,
// double-charge, or leak its temp file.
func TestPutWholeAbortsCleanlyWhenSupersededDuringStream(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 1 << 20})
	original := bytes.Repeat([]byte("o"), 16)
	replacement := bytes.Repeat([]byte("r"), 16)
	size := int64(16)

	br := &blockingReader{data: append([]byte{}, original...), started: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- c.PutWhole(key, br, size) }()

	select {
	case <-br.started:
	case <-time.After(5 * time.Second):
		t.Fatal("PutWhole never started reading")
	}

	if err := c.PutWhole(key, bytes.NewReader(replacement), size); err != nil {
		t.Fatal(err)
	}

	close(br.resume)
	select {
	case err := <-done:
		if !errors.Is(err, ErrInstallSuperseded) {
			t.Fatalf("expected ErrInstallSuperseded for the stale install, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("superseded PutWhole did not return after its reader was unblocked")
	}

	got := make([]byte, 16)
	if n, ok := c.ReadAt(key, 0, 0, got); !ok || n != 16 || !bytes.Equal(got, replacement) {
		t.Fatalf("stale install clobbered the winner: got=%q ok=%v", got[:n], ok)
	}
	if s := c.Stats(); s.Bytes != size {
		t.Fatalf("stale install left a double charge: %+v", s)
	}
	if s := c.Stats(); s.ReservedBytes != 0 {
		t.Fatalf("reservation not released after a superseded install: %+v", s)
	}
	noOrphanTempsInHydrated(t, c)
}

// TestPutWholeAbortsCleanlyOnForgetDuringStream: the key is forgotten while
// the stream is still blocked. The late install must not resurrect it,
// leave a charge, or leak its temp file.
func TestPutWholeAbortsCleanlyOnForgetDuringStream(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 1 << 20})
	data := bytes.Repeat([]byte("f"), 16)
	size := int64(16)

	br := &blockingReader{data: append([]byte{}, data...), started: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- c.PutWhole(key, br, size) }()

	select {
	case <-br.started:
	case <-time.After(5 * time.Second):
		t.Fatal("PutWhole never started reading")
	}

	c.Forget(key)

	close(br.resume)
	select {
	case err := <-done:
		if !errors.Is(err, ErrInstallSuperseded) {
			t.Fatalf("expected ErrInstallSuperseded for the forgotten install, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forgotten PutWhole did not return after its reader was unblocked")
	}

	if _, ok := c.HydratedPath(key); ok {
		t.Fatal("forgotten key resurrected by a stale install")
	}
	if s := c.Stats(); s.Bytes != 0 {
		t.Fatalf("stale install after forget left a charge: %+v", s)
	}
	if s := c.Stats(); s.ReservedBytes != 0 {
		t.Fatalf("reservation not released after a forgotten install: %+v", s)
	}
	noTempFilesInHydrated(t, c)
}

// TestPutWholeReservationDoesNotLeakAcrossSuccessfulInstalls guards against
// a successful PutWhole permanently losing its admission reservation.
// installTemp itself never touches reservedBytes/reservedEntries — only
// PutWhole's own success path does, right after installTemp returns — so a
// missing (or misplaced, e.g. left only in the now-skipped defer) decrement
// there leaks size bytes and one entry of budget on every single successful
// call. With a budget that holds only one install's worth of content, that
// leak compounds until the very next call's admission check sees an already
// "reserved" (but actually free) budget and returns ErrNoSpace, even though
// each iteration is preceded by a Forget that frees the previous install's
// real charge. Each iteration also checks Stats().ReservedBytes == 0
// straight after success, which is what the leak actually violates.
func TestPutWholeReservationDoesNotLeakAcrossSuccessfulInstalls(t *testing.T) {
	c, _ := newTest(t, Options{BlockSize: 16, MaxBytes: 16})
	full := bytes.Repeat([]byte("z"), 16)
	size := int64(len(full))
	for i := 0; i < 20; i++ {
		if err := c.PutWhole(key, bytes.NewReader(full), size); err != nil {
			t.Fatalf("iteration %d: %v (stats=%+v)", i, err, c.Stats())
		}
		if s := c.Stats(); s.ReservedBytes != 0 {
			t.Fatalf("iteration %d: reservation leaked after success: %+v", i, s)
		}
		c.Forget(key)
	}
}

// TestPutWholeConcurrentWithPutStaysConsistent runs PutWhole and Put for the
// same key concurrently under -race. PutWhole no longer holds admitMu across
// its stream, so the two genuinely interleave now; a PutWhole that loses the
// race to a Put (or another PutWhole) that lands while its own is mid-flight
// is expected to report ErrInstallSuperseded rather than corrupt anything or
// clobber the winner. This checks that holds under real contention and that
// the settled state after the race is never double-charged.
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
			if err := c.PutWhole(key, bytes.NewReader(full), size); err != nil && !errors.Is(err, ErrInstallSuperseded) {
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
