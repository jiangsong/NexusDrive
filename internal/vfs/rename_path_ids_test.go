package vfs

import (
	"context"
	"testing"
)

// A backend whose ids are paths (sftp, webdav, s3, smb) changes the id of
// every descendant when a directory is renamed or moved. The tree has to
// follow, or a read of a descendant inside the directory TTL asks the backend
// for a path that no longer exists.
func TestRenamingADirectoryRetargetsDescendantsOnAPathIDBackend(t *testing.T) {
	e := newEnv(t, envOpt{pathIDs: true})
	ctx := context.Background()
	e.fake.Seed("proj/src/main.go", []byte("package main"))

	// Warm the tree so the descendant is cached under its old id.
	if _, err := e.fs.ReadDirPath(ctx, "/ali/proj/src"); err != nil {
		t.Fatal(err)
	}
	before, err := e.store.Resolve(ctx, "/ali/proj/src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if before.RemoteID != "/proj/src/main.go" {
		t.Fatalf("path-id backend should address the file by its path, got %q", before.RemoteID)
	}

	root, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Rename(ctx, root.Ino, "proj", root.Ino, "proj2"); err != nil {
		t.Fatal(err)
	}

	// Inside the directory TTL, nothing has re-listed. The descendant must
	// still be readable, which means its id was rewritten with its parent's.
	node, err := e.store.Resolve(ctx, "/ali/proj2/src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if node.RemoteID != "/proj2/src/main.go" {
		t.Fatalf("descendant id after the rename = %q, want /proj2/src/main.go", node.RemoteID)
	}
	h, err := e.fs.Open(ctx, node.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, h)
	buf := make([]byte, 12)
	n, err := e.fs.Read(ctx, h, buf, 0)
	if err != nil {
		t.Fatalf("reading a descendant of the renamed directory: %v", err)
	}
	if got := string(buf[:n]); got != "package main" {
		t.Fatalf("content = %q", got)
	}
}

// Moving a directory into another directory changes the same ids a rename
// does, and through a different provider call.
func TestMovingADirectoryRetargetsDescendantsOnAPathIDBackend(t *testing.T) {
	e := newEnv(t, envOpt{pathIDs: true})
	ctx := context.Background()
	e.fake.Seed("proj/src/main.go", []byte("package main"))
	e.fake.Seed("archive/keep.txt", []byte("keep"))

	if _, err := e.fs.ReadDirPath(ctx, "/ali/proj/src"); err != nil {
		t.Fatal(err)
	}
	root, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := e.store.Resolve(ctx, "/ali/archive")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Rename(ctx, root.Ino, "proj", archive.Ino, "proj"); err != nil {
		t.Fatal(err)
	}

	node, err := e.store.Resolve(ctx, "/ali/archive/proj/src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if node.RemoteID != "/archive/proj/src/main.go" {
		t.Fatalf("descendant id after the move = %q", node.RemoteID)
	}
	h, err := e.fs.Open(ctx, node.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, h)
	buf := make([]byte, 12)
	if _, err := e.fs.Read(ctx, h, buf, 0); err != nil {
		t.Fatalf("reading a descendant of the moved directory: %v", err)
	}
}

// An opaque-id backend keeps the id it gave out. Renaming a directory there
// must not rewrite anything beneath it — the ids underneath never contained
// the parent's, and a prefix rewrite would be inventing addresses.
func TestRenameLeavesDescendantIDsAloneOnAnOpaqueIDBackend(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("proj/src/main.go", []byte("package main"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali/proj/src"); err != nil {
		t.Fatal(err)
	}
	before, err := e.store.Resolve(ctx, "/ali/proj/src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	root, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Rename(ctx, root.Ino, "proj", root.Ino, "proj2"); err != nil {
		t.Fatal(err)
	}
	after, err := e.store.Resolve(ctx, "/ali/proj2/src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if after.RemoteID != before.RemoteID {
		t.Fatalf("opaque id changed from %q to %q", before.RemoteID, after.RemoteID)
	}
}
