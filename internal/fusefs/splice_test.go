//go:build !windows

package fusefs

import (
	"context"
	"testing"
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
