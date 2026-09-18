//go:build darwin

package fusefs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
)

// Copying a file in the Finder is not a read and a write. It is copyfile(3),
// which also moves the file's metadata: com.apple.FinderInfo,
// com.apple.quarantine, and whatever else the source carries. A filesystem
// that cannot store extended attributes is expected to say so with ENOTSUP,
// which copyfile treats as "nothing to copy here" and skips — that is how
// copying onto a FAT volume works at all.
//
// The mount asked macFUSE for `noapplexattr`, which answers every com.apple.*
// request with EPERM instead. copyfile stops on EPERM, so the Finder refused
// the whole copy with "無法完成此操作，因為你沒有訪問一些項目的許可" — a
// permission error about a file the person owns, on a mount that reads and
// writes perfectly well from the shell.

func TestFinderStyleCopyIntoTheMountSucceeds(t *testing.T) {
	e := newMount(t, "")
	ditto, err := exec.LookPath("ditto")
	if err != nil {
		t.Skip("ditto is not installed; the Finder copy path is not exercised here")
	}
	src := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The attribute the Finder attaches to anything downloaded, and the one
	// that made every such file impossible to copy in.
	if err := unix.Setxattr(src, "com.apple.quarantine", []byte("0081;00000000;test;"), 0); err != nil {
		t.Skipf("the temporary directory does not store xattrs either: %v", err)
	}
	dst := filepath.Join(e.dir, "copied.txt")
	if out, err := exec.Command(ditto, src, dst).CombinedOutput(); err != nil {
		t.Fatalf("ditto into the mount failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "payload" {
		t.Fatalf("copied file = %q, %v", got, err)
	}
}

func TestFinderStyleCopyOutOfTheMountSucceeds(t *testing.T) {
	e := newMount(t, "")
	ditto, err := exec.LookPath("ditto")
	if err != nil {
		t.Skip("ditto is not installed; the Finder copy path is not exercised here")
	}
	src := filepath.Join(e.dir, "source.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copied.txt")
	if out, err := exec.Command(ditto, src, dst).CombinedOutput(); err != nil {
		t.Fatalf("ditto out of the mount failed: %v\n%s", err, out)
	}
}

// What a copy does, in the order copyfile(3) does it: write the file, carry
// its attributes over, read them back. Every step has to work, and none may
// answer with a permission error — that is what the Finder reports to the
// person, about a file they own.
func TestAttributesSurviveACopyAndNeverReadAsAPermissionError(t *testing.T) {
	e := newMount(t, "")
	target := filepath.Join(e.dir, "file.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	attrs := map[string][]byte{
		"com.apple.FinderInfo":                make([]byte, 32),
		"com.apple.quarantine":                []byte("0081;00000000;test;"),
		"com.apple.metadata:_kMDItemUserTags": []byte("tag"),
	}
	for name, value := range attrs {
		if err := unix.Setxattr(target, name, value, 0); err != nil {
			if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
				t.Fatalf("setxattr %s = %v; a copy tool reports this to the person as a permission problem", name, err)
			}
			t.Fatalf("setxattr %s = %v", name, err)
		}
	}
	for name, value := range attrs {
		buf := make([]byte, 256)
		n, err := unix.Getxattr(target, name, buf)
		if err != nil {
			t.Fatalf("getxattr %s = %v", name, err)
		}
		if string(buf[:n]) != string(value) {
			t.Errorf("%s read back as %q", name, buf[:n])
		}
	}
	// A missing one is missing, not forbidden: copyfile asks for attributes
	// the source did not have.
	if _, err := unix.Getxattr(target, "com.apple.ResourceFork", make([]byte, 16)); !errors.Is(err, unix.ENOATTR) {
		t.Errorf("getxattr of an absent attribute = %v, want ENOATTR", err)
	}
	names := make([]byte, 4096)
	n, err := unix.Listxattr(target, names)
	if err != nil {
		t.Fatalf("listxattr = %v", err)
	}
	for name := range attrs {
		if !strings.Contains(string(names[:n]), name) {
			t.Errorf("listxattr does not report %s", name)
		}
	}
	// Removing is removing; asking twice says so.
	if err := unix.Removexattr(target, "com.apple.quarantine"); err != nil {
		t.Fatalf("removexattr = %v", err)
	}
	if err := unix.Removexattr(target, "com.apple.quarantine"); !errors.Is(err, unix.ENOATTR) {
		t.Errorf("second removexattr = %v, want ENOATTR", err)
	}
}

// The mount option that caused it, named here so the reason cannot be lost to
// a later tidy-up: noapplexattr turns every com.apple.* request into EPERM.
func TestTheMountDoesNotAskForAppleXattrRefusals(t *testing.T) {
	var o fuse.MountOptions
	applyPlatformOptions(&o)
	for _, opt := range o.Options {
		if opt == "noapplexattr" {
			t.Fatal("noapplexattr makes every com.apple.* xattr call fail with EPERM, which the Finder shows as a permission error on the whole copy")
		}
	}
}
