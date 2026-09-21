package vfs

import (
	"bytes"
	"context"
	"testing"
)

// TestReadHandleSeesASiblingWritersStagedBytes: git's index-pack streams a
// pack into one descriptor and, before closing it, opens the same file
// read-only and preads objects back out of it. The bytes it wrote are in
// the write handle's staging file, so a read handle that consults only the
// cache under the node's committed identity answers with a stale or empty
// file — "premature end of pack file, N bytes missing". Same-inode
// visibility is what POSIX promises and what a local disk gives: a read
// through any descriptor sees what any other descriptor has written.
func TestReadHandleSeesASiblingWritersStagedBytes(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("docs/old.txt", []byte("old content"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	w, err := e.fs.Create(ctx, root.Ino, "tmp_pack")
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, w)
	payload := bytes.Repeat([]byte("p"), 10000)
	if _, err := e.fs.Write(ctx, w, payload, 0); err != nil {
		t.Fatal(err)
	}

	r, err := e.fs.Open(ctx, w.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, r)
	got := make([]byte, len(payload)+100)
	n, err := e.fs.Read(ctx, r, got, 0)
	if err != nil {
		t.Fatalf("read through the sibling handle: %v", err)
	}
	if n != len(payload) || !bytes.Equal(got[:n], payload) {
		t.Fatalf("sibling read returned %d bytes, want the %d staged bytes", n, len(payload))
	}
	// The tail past the last full block-cache page is where the pack's
	// last object lives; a read there must not report end of file.
	tail := make([]byte, 4096)
	n, err = e.fs.Read(ctx, r, tail, int64(len(payload))-1219)
	if err != nil || n != 1219 {
		t.Fatalf("tail read: n=%d err=%v, want 1219 bytes", n, err)
	}

	// A file that already has committed content behaves the same: the
	// sibling reader sees the rewrite in progress, not the committed
	// version.
	if _, err := e.fs.ReadDirPath(ctx, "/ali/docs"); err != nil {
		t.Fatal(err)
	}
	old, err := e.store.Resolve(ctx, "/ali/docs/old.txt")
	if err != nil {
		t.Fatal(err)
	}
	w2, err := e.fs.Open(ctx, old.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, w2)
	if _, err := e.fs.Write(ctx, w2, []byte("NEW"), 0); err != nil {
		t.Fatal(err)
	}
	r2, err := e.fs.Open(ctx, old.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, r2)
	buf := make([]byte, 64)
	n, err = e.fs.Read(ctx, r2, buf, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "NEW content" {
		t.Fatalf("sibling read of a rewrite in progress got %q, want %q", buf[:n], "NEW content")
	}
}

// TestReadHandleFollowsASiblingWritersCommit is the other half of the same
// promise: a read handle opened while the file was being rewritten keeps
// answering correctly after the writer closes. Its snapshot of the node
// names the version from before the rewrite, whose cache entry the commit
// forgets; reading through that snapshot would fetch the old content back
// from the drive.
func TestReadHandleFollowsASiblingWritersCommit(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("docs/old.txt", []byte("old content"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali/docs"); err != nil {
		t.Fatal(err)
	}
	old, err := e.store.Resolve(ctx, "/ali/docs/old.txt")
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.fs.Open(ctx, old.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, r)
	buf := make([]byte, 64)
	if n, err := e.fs.Read(ctx, r, buf, 0); err != nil || string(buf[:n]) != "old content" {
		t.Fatalf("before the rewrite: %q, %v", buf[:n], err)
	}
	w, err := e.fs.Open(ctx, old.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, w, []byte("NEW"), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Release(ctx, w); err != nil {
		t.Fatal(err)
	}
	n, err := e.fs.Read(ctx, r, buf, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "NEW content" {
		t.Fatalf("after the writer closed, the read handle got %q, want %q", buf[:n], "NEW content")
	}
}

// TestLookupAndListingReportStagedSize: the kernel trusts the size in a
// LOOKUP or readdir reply as much as one from GETATTR — it sets the inode's
// size from it and drops the page cache beyond. A file being written for
// the first time has size 0 in the tree until its handle commits, so once
// the kernel's entry timeout expires (30 s; a git fetch over a slow link
// takes longer) the next path resolution of the file told the kernel it
// was empty, and every read past offset 0 returned nothing without
// reaching the daemon. That was `git pull` dying with "premature end of
// pack file". Stat already overlays the staged size; Lookup, StatPath and
// the listings must say the same thing.
func TestLookupAndListingReportStagedSize(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Create(ctx, root.Ino, "tmp_pack")
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, h)
	payload := bytes.Repeat([]byte("p"), 10000)
	if _, err := e.fs.Write(ctx, h, payload, 0); err != nil {
		t.Fatal(err)
	}
	want := int64(len(payload))

	if a, err := e.fs.Lookup(ctx, root.Ino, "tmp_pack"); err != nil || a.Size != want {
		t.Fatalf("Lookup size = %d, %v; want %d", a.Size, err, want)
	}
	if a, err := e.fs.StatPath(ctx, "/ali/tmp_pack"); err != nil || a.Size != want {
		t.Fatalf("StatPath size = %d, %v; want %d", a.Size, err, want)
	}
	for name, list := range map[string]func() ([]Attr, error){
		"ReadDir":     func() ([]Attr, error) { return e.fs.ReadDir(ctx, root.Ino) },
		"ReadDirPath": func() ([]Attr, error) { return e.fs.ReadDirPath(ctx, "/ali") },
	} {
		entries, err := list()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, a := range entries {
			if a.Name == "tmp_pack" {
				found = true
				if a.Size != want {
					t.Fatalf("%s entry size = %d, want %d", name, a.Size, want)
				}
			}
		}
		if !found {
			t.Fatalf("%s did not list tmp_pack", name)
		}
	}
}
