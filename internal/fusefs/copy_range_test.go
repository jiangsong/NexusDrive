//go:build !windows

package fusefs

import (
	"context"
	"syscall"
	"testing"

	"cloudfs/internal/journal"
)

// TestCopyFileRangeCopiesHydratedSourceIntoDestinationStaging exercises the
// in-mount fast path end to end without a real kernel: a hydrated source
// handle's cache fd is copied straight into a destination write handle's
// staging file, and the destination is readable back with the right
// content once committed.
func TestCopyFileRangeCopiesHydratedSourceIntoDestinationStaging(t *testing.T) {
	e := newBackingFixtureWithPolicy(t, false) // "f" == "content" (7 bytes)
	ctx := context.Background()
	j, err := journal.Open(journal.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	e.root.opt.FS.SetWriteBackend(j, nil)

	srcAttr, err := e.root.opt.FS.StatPath(ctx, "/f")
	if err != nil {
		t.Fatal(err)
	}
	srcHandle, err := e.root.opt.FS.Open(ctx, srcAttr.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	srcFile := &file{root: e.root, handle: srcHandle}
	defer srcFile.Release(ctx)

	rootAttr, err := e.root.opt.FS.StatPath(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	dstHandle, err := e.root.opt.FS.Create(ctx, rootAttr.Ino, "dest.bin")
	if err != nil {
		t.Fatal(err)
	}
	dstFile := &file{root: e.root, handle: dstHandle}

	n := &node{root: e.root, ino: srcAttr.Ino}
	written, errno := n.CopyFileRange(ctx, srcFile, 0, nil, dstFile, 0, 7, 0)
	if errno != 0 {
		t.Fatalf("CopyFileRange errno: %v", errno)
	}
	if written != 7 {
		t.Fatalf("wrote %d bytes, want 7", written)
	}
	if err := e.root.opt.FS.Sync(ctx, dstHandle); err != nil {
		t.Fatalf("commit destination: %v", err)
	}
	if err := e.root.opt.FS.Release(ctx, dstHandle); err != nil {
		t.Fatalf("release destination: %v", err)
	}

	readAttr, err := e.root.opt.FS.StatPath(ctx, "/dest.bin")
	if err != nil {
		t.Fatal(err)
	}
	if readAttr.Size != 7 {
		t.Fatalf("committed size = %d, want 7", readAttr.Size)
	}
	readHandle, err := e.root.opt.FS.Open(ctx, readAttr.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.root.opt.FS.Release(ctx, readHandle)
	buf := make([]byte, 7)
	rn, err := e.root.opt.FS.Read(ctx, readHandle, buf, 0)
	if err != nil || rn != 7 || string(buf) != "content" {
		t.Fatalf("read back = %q (%d, %v), want %q", buf[:rn], rn, err, "content")
	}
}

// TestCopyFileRangeFallsBackWhenDestinationIsNotAWriteHandle checks that a
// destination handle opened for reading gets ENOTSUP, sending the kernel
// back to a normal read/write pair, instead of the fast path silently
// misusing a read handle.
func TestCopyFileRangeFallsBackWhenDestinationIsNotAWriteHandle(t *testing.T) {
	e := newBackingFixtureWithPolicy(t, false)
	ctx := context.Background()
	srcAttr, err := e.root.opt.FS.StatPath(ctx, "/f")
	if err != nil {
		t.Fatal(err)
	}
	srcHandle, err := e.root.opt.FS.Open(ctx, srcAttr.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	srcFile := &file{root: e.root, handle: srcHandle}
	defer srcFile.Release(ctx)

	dstHandle, err := e.root.opt.FS.Open(ctx, srcAttr.Ino, false) // read-only, not a write handle
	if err != nil {
		t.Fatal(err)
	}
	dstFile := &file{root: e.root, handle: dstHandle}
	defer dstFile.Release(ctx)

	n := &node{root: e.root, ino: srcAttr.Ino}
	if _, errno := n.CopyFileRange(ctx, srcFile, 0, nil, dstFile, 0, 7, 0); errno != syscall.ENOTSUP {
		t.Fatalf("errno = %v, want ENOTSUP", errno)
	}
}

// TestCopyFileRangeFallsBackWithoutASourceLease checks that a source with no
// cache lease available (here: an unwired adapter) also gets ENOTSUP.
func TestCopyFileRangeFallsBackWithoutASourceLease(t *testing.T) {
	e := newBackingFixture(t)
	ctx := context.Background()
	j, err := journal.Open(journal.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	e.root.opt.FS.SetWriteBackend(j, nil)

	r := New(Options{FS: e.root.opt.FS}) // unwired: no backing registry available
	srcAttr, err := r.opt.FS.StatPath(ctx, "/f")
	if err != nil {
		t.Fatal(err)
	}
	srcHandle, err := r.opt.FS.Open(ctx, srcAttr.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	srcFile := &file{root: r, handle: srcHandle}
	defer srcFile.Release(ctx)

	rootAttr, err := r.opt.FS.StatPath(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	dstHandle, err := r.opt.FS.Create(ctx, rootAttr.Ino, "dest2.bin")
	if err != nil {
		t.Fatal(err)
	}
	dstFile := &file{root: r, handle: dstHandle}
	defer dstFile.Release(ctx)

	n := &node{root: r, ino: srcAttr.Ino}
	if _, errno := n.CopyFileRange(ctx, srcFile, 0, nil, dstFile, 0, 7, 0); errno != syscall.ENOTSUP {
		t.Fatalf("errno = %v, want ENOTSUP", errno)
	}
}
