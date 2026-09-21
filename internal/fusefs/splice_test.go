//go:build !windows

package fusefs

import (
	"context"
	"testing"

	"cloudfs/internal/journal"
	"cloudfs/internal/vfs"
)

// seekable mirrors go-fuse's unexported seekableResult interface structurally,
// so a test can confirm a fuse.ReadResult really is a zero-copy
// fuse.ReadResultFd without needing to name that unexported type.
type seekable interface {
	Seekable() (fd uintptr, off int64, sz int)
}

// TestSpliceReadReturnsAZeroCopyResultForAHydratedFile checks that a read
// against a fully cached file returns fuse.ReadResultFd, clipped to the
// requested range, and that the fd it names really does serve the right
// bytes.
func TestSpliceReadReturnsAZeroCopyResultForAHydratedFile(t *testing.T) {
	e := newBackingFixture(t) // "f" == "content" (7 bytes), fully hydrated
	ctx := context.Background()
	a, err := e.root.opt.FS.StatPath(ctx, "/f")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.root.opt.FS.Open(ctx, a.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	f := &file{root: e.root, handle: h}
	defer f.Release(ctx)

	dest := make([]byte, 4)
	res, errno := f.Read(ctx, dest, 1)
	if errno != 0 {
		t.Fatalf("Read errno: %v", errno)
	}
	sr, ok := res.(seekable)
	if !ok {
		t.Fatalf("Read did not splice, got %T", res)
	}
	if _, off, sz := sr.Seekable(); off != 1 || sz != 4 {
		t.Fatalf("splice range = (off=%d sz=%d), want (1,4)", off, sz)
	}
	got, status := res.Bytes(make([]byte, 4))
	if status != 0 {
		t.Fatalf("Bytes status: %v", status)
	}
	if string(got) != "onte" { // "content"[1:5]
		t.Fatalf("splice content = %q, want %q", got, "onte")
	}
}

// TestSpliceReadClipsToEndOfFile checks that a read reaching past the end of
// the file is clipped to what remains, not to the caller's full buffer.
func TestSpliceReadClipsToEndOfFile(t *testing.T) {
	e := newBackingFixture(t) // "f" == "content" (7 bytes)
	ctx := context.Background()
	a, err := e.root.opt.FS.StatPath(ctx, "/f")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.root.opt.FS.Open(ctx, a.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	f := &file{root: e.root, handle: h}
	defer f.Release(ctx)

	dest := make([]byte, 16)
	res, errno := f.Read(ctx, dest, 5)
	if errno != 0 {
		t.Fatalf("Read errno: %v", errno)
	}
	sr, ok := res.(seekable)
	if !ok {
		t.Fatalf("Read did not splice, got %T", res)
	}
	if _, off, sz := sr.Seekable(); off != 5 || sz != 2 {
		t.Fatalf("splice range = (off=%d sz=%d), want (5,2)", off, sz)
	}
}

// TestSpliceReadFallsBackWithoutALease checks that Read still works, through
// the ordinary FS.Read path, when no lease can be offered (here: an
// adapter that never wired its backing registry to anything).
func TestSpliceReadFallsBackWithoutALease(t *testing.T) {
	e := newBackingFixture(t)
	ctx := context.Background()
	r := New(Options{FS: e.root.opt.FS}) // unwired: rawFS was never called on it
	a, err := r.opt.FS.StatPath(ctx, "/f")
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.opt.FS.Open(ctx, a.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	f := &file{root: r, handle: h}
	defer f.Release(ctx)

	dest := make([]byte, 4)
	res, errno := f.Read(ctx, dest, 0)
	if errno != 0 {
		t.Fatalf("Read errno: %v", errno)
	}
	if _, ok := res.(seekable); ok {
		t.Fatal("spliced without any lease available")
	}
	b, status := res.Bytes(dest)
	if status != 0 || string(b) != "cont" {
		t.Fatalf("fallback read = %q (%v), want %q", b, status, "cont")
	}
}

// TestSpliceAndPassthroughShareOneLease is the "released exactly once" case:
// a splice read may create the lease before the kernel ever calls
// PassthroughFd, and PassthroughFd must reuse that same lease (even with the
// experimental opt-in off) rather than opening a second, unmanaged one; a
// single Release must then tear down the one lease that exists.
func TestSpliceAndPassthroughShareOneLease(t *testing.T) {
	e := newBackingFixtureWithPolicy(t, false) // experimental passthrough opt-in OFF
	ctx := context.Background()
	a, err := e.root.opt.FS.StatPath(ctx, "/f")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.root.opt.FS.Open(ctx, a.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	f := &file{root: e.root, handle: h}

	dest := make([]byte, 4)
	res, errno := f.Read(ctx, dest, 0)
	if errno != 0 {
		t.Fatalf("Read errno: %v", errno)
	}
	if _, ok := res.(seekable); !ok {
		t.Fatalf("expected splice to lease and read via fd, got %T", res)
	}
	res.Done() // the server has spliced it; the lease is no longer pinned
	if e.cache.Stats().LeasedBytes != 7 {
		t.Fatalf("expected a single 7-byte lease after splice, got %+v", e.cache.Stats())
	}

	if _, ok := f.PassthroughFd(); !ok {
		t.Fatal("PassthroughFd did not reuse the splice lease")
	}
	if e.cache.Stats().LeasedBytes != 7 {
		t.Fatalf("PassthroughFd opened a second lease: %+v", e.cache.Stats())
	}

	f.Release(ctx)
	if e.cache.Stats().LeasedBytes != 0 {
		t.Fatal("release did not tear down the shared lease")
	}
}

// TestKernelReadIsObservedAsFromKernel: the read-heat observer the daemon
// installs (vfs.SetReadObserver) must see a read the kernel makes as a
// kernel read, whichever way the handler serves it — through FS.Read, or
// as a zero-copy splice that never enters FS.Read. Without this a real
// mount recorded no kernel heat at all (found by the 2026-09-17 smoke
// run): the handler passed go-fuse's bare context, so the daemon's
// readerKind saw neither a kernel nor an origin mark and dropped it.
func TestKernelReadIsObservedAsFromKernel(t *testing.T) {
	e := newBackingFixture(t) // "f" == "content", fully hydrated: splices
	ctx := context.Background()
	type seen struct {
		ino    uint64
		kernel bool
	}
	var got []seen
	e.root.opt.FS.SetReadObserver(func(ctx context.Context, ino uint64) {
		got = append(got, seen{ino, vfs.IsFromKernel(ctx)})
	})
	a, err := e.root.opt.FS.StatPath(ctx, "/f")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.root.opt.FS.Open(ctx, a.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	f := &file{root: e.root, handle: h}
	defer f.Release(ctx)
	res, errno := f.Read(ctx, make([]byte, 4), 0)
	if errno != 0 {
		t.Fatalf("Read errno: %v", errno)
	}
	if _, ok := res.(seekable); !ok {
		t.Fatalf("the fixture no longer splices (got %T); the test needs both paths", res)
	}
	if len(got) != 1 || got[0].ino != a.Ino || !got[0].kernel {
		t.Fatalf("spliced kernel read observed as %+v", got)
	}
	// A read the handler serves through FS.Read (no lease: a fresh handle
	// on a file the fixture did not hydrate) is a kernel read too.
	got = nil
	e.fake.Seed("g", []byte("second"))
	rootAttr, err := e.root.opt.FS.StatPath(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.root.opt.FS.Refresh(ctx, rootAttr.Ino); err != nil {
		t.Fatal(err)
	}
	b, err := e.root.opt.FS.StatPath(ctx, "/g")
	if err != nil {
		t.Fatal(err)
	}
	h2, err := e.root.opt.FS.Open(ctx, b.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	f2 := &file{root: e.root, handle: h2}
	defer f2.Release(ctx)
	if _, errno := f2.Read(ctx, make([]byte, 6), 0); errno != 0 {
		t.Fatalf("Read errno: %v", errno)
	}
	if len(got) != 1 || got[0].ino != b.Ino || !got[0].kernel {
		t.Fatalf("plain kernel read observed as %+v", got)
	}
}

// TestSpliceReadStopsWhenASiblingWriterHasBytes: the splice lease is a
// descriptor on the immutable cache entry for the version the read handle
// was opened against. Once another descriptor writes to the file, that
// entry is no longer the file's content — neither while the bytes sit in
// the writer's staging file nor after its close commits them under a new
// identity — so a read that kept splicing from it would hand the kernel
// the version before the rewrite.
func TestSpliceReadStopsWhenASiblingWriterHasBytes(t *testing.T) {
	e := newBackingFixture(t) // "f" == "content" (7 bytes), fully hydrated
	ctx := context.Background()
	j, err := journal.Open(journal.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	e.root.opt.FS.SetWriteBackend(j, nil)
	a, err := e.root.opt.FS.StatPath(ctx, "/f")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.root.opt.FS.Open(ctx, a.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	f := &file{root: e.root, handle: h}
	defer f.Release(ctx)

	read := func(want string, splice bool) {
		t.Helper()
		dest := make([]byte, 16)
		res, errno := f.Read(ctx, dest, 0)
		if errno != 0 {
			t.Fatalf("Read errno: %v", errno)
		}
		if _, ok := res.(seekable); ok != splice {
			t.Fatalf("spliced=%v, want %v (read %q)", ok, splice, want)
		}
		got, status := res.Bytes(dest)
		if status != 0 || string(got) != want {
			t.Fatalf("read = %q (%v), want %q", got, status, want)
		}
		res.Done()
	}
	read("content", true)

	w, err := e.root.opt.FS.Open(ctx, a.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.root.opt.FS.Write(ctx, w, []byte("CHANGED!"), 0); err != nil {
		t.Fatal(err)
	}
	read("CHANGED!", false)
	if err := e.root.opt.FS.Release(ctx, w); err != nil {
		t.Fatal(err)
	}
	read("CHANGED!", false)
}

// TestSpliceResultOutlivesADropOfItsLease: go-fuse splices a ReadResultFd
// after the Read handler has returned, and it dispatches requests
// concurrently. A second read on the same handle that finds the lease
// superseded withdraws it (dropOffer); it must not close the descriptor
// the first read's result still names. The descriptor closes once that
// result is Done.
func TestSpliceResultOutlivesADropOfItsLease(t *testing.T) {
	e := newBackingFixture(t) // "f" == "content" (7 bytes), fully hydrated
	ctx := context.Background()
	a, err := e.root.opt.FS.StatPath(ctx, "/f")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.root.opt.FS.Open(ctx, a.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	f := &file{root: e.root, handle: h}
	defer f.Release(ctx)

	first, errno := f.Read(ctx, make([]byte, 7), 0)
	if errno != 0 {
		t.Fatalf("Read errno: %v", errno)
	}
	if _, ok := first.(seekable); !ok {
		t.Fatalf("first read did not splice, got %T", first)
	}
	f.passMu.Lock()
	lease := f.pass
	f.passMu.Unlock()
	if lease == nil {
		t.Fatal("no lease after a spliced read")
	}

	// What a concurrent read does on finding the lease superseded, before
	// the server has consumed the first result.
	f.dropOffer()
	f.passMu.Lock()
	dropped := f.pass == nil
	f.passMu.Unlock()
	if !dropped {
		t.Fatal("dropOffer left the lease on the handle")
	}
	if _, ok := e.root.backings.fd(lease); !ok {
		t.Fatal("lease descriptor closed while the first result was in flight")
	}
	if e.cache.Stats().LeasedBytes != 7 {
		t.Fatalf("lease released while in flight: %+v", e.cache.Stats())
	}
	// The consumer splices now, after both handlers have returned.
	got, status := first.Bytes(make([]byte, 7))
	if status != 0 || string(got) != "content" {
		t.Fatalf("in-flight result read %q (%v), want %q", got, status, "content")
	}
	first.Done()
	if _, ok := e.root.backings.fd(lease); ok {
		t.Fatal("lease descriptor still open after its last result was done")
	}
	if e.cache.Stats().LeasedBytes != 0 {
		t.Fatalf("lease not released after Done: %+v", e.cache.Stats())
	}
}
