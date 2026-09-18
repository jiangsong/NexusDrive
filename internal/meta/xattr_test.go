package meta

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Extended attributes are local metadata, held here and never uploaded.
//
// macOS puts them on everything it copies — com.apple.quarantine on anything
// downloaded, FinderInfo, Finder tags — and copyfile(3), which is what the
// Finder, cp -p and ditto all copy with, abandons the whole copy when it
// cannot write one. Refusing them made the mount unusable from the Finder
// while it worked perfectly from a shell. Storing them beside the node keeps
// them off the cloud drive, where they would mean a junk file or a metadata
// write per copied file and would surface on every other device.

func TestXattrRoundTrip(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	n, err := s.Insert(ctx, file(1, "photo.jpg", 10))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetXattr(ctx, n.Ino, "com.apple.quarantine", []byte("0081;00000000;Safari;")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetXattr(ctx, n.Ino, "com.apple.FinderInfo", make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Xattr(ctx, n.Ino, "com.apple.quarantine")
	if err != nil || string(got) != "0081;00000000;Safari;" {
		t.Fatalf("read back = %q, %v", got, err)
	}
	names, err := s.XattrNames(ctx, n.Ino)
	if err != nil || len(names) != 2 || names[0] != "com.apple.FinderInfo" || names[1] != "com.apple.quarantine" {
		t.Fatalf("names = %v, %v; want both, sorted", names, err)
	}
	// Replacing one keeps the other.
	if err := s.SetXattr(ctx, n.Ino, "com.apple.quarantine", []byte("replaced")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Xattr(ctx, n.Ino, "com.apple.quarantine"); string(got) != "replaced" {
		t.Fatalf("after replace = %q", got)
	}
	if err := s.RemoveXattr(ctx, n.Ino, "com.apple.quarantine"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Xattr(ctx, n.Ino, "com.apple.quarantine"); !errors.Is(err, ErrNoXattr) {
		t.Fatalf("read after remove = %v, want ErrNoXattr", err)
	}
	if err := s.RemoveXattr(ctx, n.Ino, "com.apple.quarantine"); !errors.Is(err, ErrNoXattr) {
		t.Fatalf("second remove = %v, want ErrNoXattr", err)
	}
}

// A file that is gone takes its attributes with it. Left behind, they would be
// handed to whatever inode number the autoincrement eventually reissued.
func TestXattrsGoWithTheNode(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	n, err := s.Insert(ctx, file(1, "gone.txt", 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetXattr(ctx, n.Ino, "user.tag", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, n.Ino); err != nil {
		t.Fatal(err)
	}
	names, err := s.XattrNames(ctx, n.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Fatalf("attributes outlived the node: %v", names)
	}
}

// The store is a local database, not a blob store. A resource fork arriving as
// an extended attribute must be refused rather than swallowed whole.
func TestXattrRefusesOversizedValuesAndNames(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	n, err := s.Insert(ctx, file(1, "big.txt", 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetXattr(ctx, n.Ino, "user.big", make([]byte, MaxXattrValue+1)); !errors.Is(err, ErrXattrTooBig) {
		t.Fatalf("oversized value = %v, want ErrXattrTooBig", err)
	}
	if err := s.SetXattr(ctx, n.Ino, strings.Repeat("n", 256), []byte("v")); !errors.Is(err, ErrXattrTooBig) {
		t.Fatalf("oversized name = %v, want ErrXattrTooBig", err)
	}
}
