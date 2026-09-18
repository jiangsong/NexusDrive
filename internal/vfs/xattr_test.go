package vfs

import (
	"context"
	"errors"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/meta"
)

// Extended attributes are local metadata the filesystem keeps for a node.
// They never reach a provider: the backends have nowhere to put them, and the
// kernel's fallback — AppleDouble "._" sidecars — would upload a junk file per
// copied file. What they buy is the Finder: copyfile(3) abandons a copy whose
// setxattr fails, so without these a mount could not be copied into at all.

func TestXattrsAreKeptForANodeAndDoNotReachTheProvider(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	root, err := e.fs.StatPath(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Create(ctx, root.Ino, "tagged.txt")
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.fs.Stat(ctx, h.Ino)
	if err != nil {
		t.Fatal(err)
	}
	before := e.fake.TotalCalls()
	if err := e.fs.SetXattr(ctx, a.Ino, "com.apple.quarantine", []byte("0081;0;Safari;")); err != nil {
		t.Fatal(err)
	}
	got, err := e.fs.Xattr(ctx, a.Ino, "com.apple.quarantine")
	if err != nil || string(got) != "0081;0;Safari;" {
		t.Fatalf("read back = %q, %v", got, err)
	}
	names, err := e.fs.XattrNames(ctx, a.Ino)
	if err != nil || len(names) != 1 || names[0] != "com.apple.quarantine" {
		t.Fatalf("names = %v, %v", names, err)
	}
	if after := e.fake.TotalCalls(); after != before {
		t.Fatalf("storing an extended attribute called the provider: %d -> %d", before, after)
	}
	if err := e.fs.RemoveXattr(ctx, a.Ino, "com.apple.quarantine"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Xattr(ctx, a.Ino, "com.apple.quarantine"); !errors.Is(err, meta.ErrNoXattr) {
		t.Fatalf("after remove = %v, want ErrNoXattr", err)
	}
}

// A read-only subtree refuses them like any other write: the point of the
// mode is that nothing in it changes.
func TestXattrWritesRefusedOnAReadOnlyMount(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOpt{mode: config.ModeReadonly})
	e.fake.Seed("/file.txt", []byte("x"))
	a, err := e.fs.StatPath(ctx, "/ali/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.SetXattr(ctx, a.Ino, "user.tag", []byte("x")); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("set on a read-only mount = %v, want ErrReadOnly", err)
	}
	if err := e.fs.RemoveXattr(ctx, a.Ino, "user.tag"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("remove on a read-only mount = %v, want ErrReadOnly", err)
	}
}
