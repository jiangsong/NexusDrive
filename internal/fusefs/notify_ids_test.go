package fusefs

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestInvalidationReachesTheKernelNodeWhenInosDiverge pins the kernel
// identity the invalidation callbacks must use. go-fuse numbers kernel
// nodes in lookup order, independently of the VFS inode it is told as
// StableAttr.Ino, so the two agree only while every inode is looked up in
// the order it was created. Files the kernel never sees (written through
// MCP or the control API, or listed only by the VFS) break that: here two
// files are created behind the kernel's back before it looks anything up,
// so the directory's node id is two below its inode. A change made through
// the VFS must still reach the kernel's cached listing and the dentry it
// holds for the removed name.
func TestInvalidationReachesTheKernelNodeWhenInosDiverge(t *testing.T) {
	e := newMount(t, "")
	ctx := context.Background()
	// Inodes the kernel never looks up.
	for _, p := range []string{"/behind-1.txt", "/behind-2.txt"} {
		if _, err := e.fs.WriteFile(ctx, p, []byte("x"), false); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(e.dir, "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "one.txt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.settle(t)
	at, err := e.fs.StatPath(ctx, "/d")
	if err != nil {
		t.Fatal(err)
	}
	if at.Ino <= 3 {
		t.Fatalf("the directory's inode is %d; the test needs it above the kernel's node id", at.Ino)
	}
	names := func() []string {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, en := range ents {
			out = append(out, en.Name())
		}
		sort.Strings(out)
		return out
	}
	// Two listings: the second is the kernel's cached stream.
	names()
	if got := names(); strings.Join(got, ",") != "one.txt" {
		t.Fatalf("initial listing = %v", got)
	}
	// A create behind the kernel's back.
	if _, err := e.fs.WriteFile(ctx, "/d/two.txt", []byte("2"), false); err != nil {
		t.Fatal(err)
	}
	waitNames := func(want string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			got := names()
			if strings.Join(got, ",") == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("listing = %v, want %s: the kernel's directory cache was not invalidated", got, want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitNames("one.txt,two.txt")
	// A delete behind the kernel's back: the listing and the dentry both
	// stop answering for the name.
	if _, err := os.Stat(filepath.Join(dir, "one.txt")); err != nil {
		t.Fatal(err)
	}
	parent, err := e.fs.StatPath(ctx, "/d")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Remove(ctx, parent.Ino, "one.txt", false); err != nil {
		t.Fatal(err)
	}
	waitNames("two.txt")
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := os.Stat(filepath.Join(dir, "one.txt"))
		if os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stat of a file removed through the VFS still answers: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
