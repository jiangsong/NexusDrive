package meta

import (
	"context"
	"testing"
)

// On a backend whose ids are paths, renaming a directory changes the id of
// everything under it. Retarget rewrites the node and, when asked, every
// descendant that carried the old id as its path prefix.
func TestRetargetRewritesTheSubtreePathPrefix(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	proj, err := s.Upsert(ctx, withID(dir(RootIno, "proj"), "/proj"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := s.Upsert(ctx, withID(dir(proj.Ino, "src"), "/proj/src"))
	if err != nil {
		t.Fatal(err)
	}
	main, err := s.Upsert(ctx, withID(file(src.Ino, "main.go", 12), "/proj/src/main.go"))
	if err != nil {
		t.Fatal(err)
	}
	// A sibling outside the subtree whose id merely starts with the same
	// characters must not be touched: "/projector" is not under "/proj".
	outside, err := s.Upsert(ctx, withID(file(RootIno, "projector", 3), "/projector"))
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Retarget(ctx, proj.Ino, "/proj2", true); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		ino  uint64
		want string
	}{
		{proj.Ino, "/proj2"},
		{src.Ino, "/proj2/src"},
		{main.Ino, "/proj2/src/main.go"},
		{outside.Ino, "/projector"},
	} {
		got, err := s.Get(ctx, tc.ino)
		if err != nil {
			t.Fatal(err)
		}
		if got.RemoteID != tc.want {
			t.Errorf("%s remote id = %q, want %q", got.Name, got.RemoteID, tc.want)
		}
	}
}

// S3 directory object IDs include a trailing slash. Retarget must not append
// a second slash when it constructs the descendant prefix, or every child
// misses the update and continues pointing at the old key.
func TestRetargetRewritesTrailingSlashDirectoryIDs(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	proj, err := s.Upsert(ctx, withID(dir(RootIno, "proj"), "/proj/"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := s.Upsert(ctx, withID(dir(proj.Ino, "src"), "/proj/src/"))
	if err != nil {
		t.Fatal(err)
	}
	main, err := s.Upsert(ctx, withID(file(src.Ino, "main.go", 12), "/proj/src/main.go"))
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Retarget(ctx, proj.Ino, "/proj2/", true); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		ino  uint64
		want string
	}{
		{proj.Ino, "/proj2/"},
		{src.Ino, "/proj2/src/"},
		{main.Ino, "/proj2/src/main.go"},
	} {
		got, err := s.Get(ctx, tc.ino)
		if err != nil {
			t.Fatal(err)
		}
		if got.RemoteID != tc.want {
			t.Errorf("%s remote id = %q, want %q", got.Name, got.RemoteID, tc.want)
		}
	}
}

// A file that exists only as a queued write has no backend id at all. Its
// placeholder must survive a rename of the directory above it, or the write
// loses the blob it is holding.
func TestRetargetLeavesUnuploadedDescendantsAlone(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	proj, err := s.Upsert(ctx, withID(dir(RootIno, "proj"), "/proj"))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.Upsert(ctx, withID(file(proj.Ino, "draft.txt", 1), "cloudfs-local:u7"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Retarget(ctx, proj.Ino, "/proj2", true); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, pending.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteID != "cloudfs-local:u7" {
		t.Fatalf("queued write id = %q, want it untouched", got.RemoteID)
	}
}

// With descendants off — an opaque-id backend that reports a new id for the
// entry it moved — only the node itself is rewritten.
func TestRetargetWithoutDescendantsTouchesOnlyTheNode(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	d, err := s.Upsert(ctx, withID(dir(RootIno, "proj"), "abc"))
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.Upsert(ctx, withID(file(d.Ino, "a.txt", 1), "abc/inner"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Retarget(ctx, d.Ino, "xyz", false); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, d.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteID != "xyz" {
		t.Fatalf("node id = %q", got.RemoteID)
	}
	kid, err := s.Get(ctx, child.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if kid.RemoteID != "abc/inner" {
		t.Fatalf("descendant id = %q, want it untouched", kid.RemoteID)
	}
}

func withID(n Node, id string) Node {
	n.Remote = "ali"
	n.RemoteID = id
	return n
}

// A nested mount puts another remote's subtree under this one's directory.
// Renaming the outer directory changes nothing about where the inner remote
// addresses its own objects, so a descendant belonging to a different remote
// must keep its id even when that id happens to share the prefix.
func TestRetargetLeavesOtherRemotesAlone(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	proj, err := s.Upsert(ctx, withID(dir(RootIno, "proj"), "/proj"))
	if err != nil {
		t.Fatal(err)
	}
	mine, err := s.Upsert(ctx, withID(file(proj.Ino, "mine.txt", 4), "/proj/mine.txt"))
	if err != nil {
		t.Fatal(err)
	}
	other := withID(file(proj.Ino, "theirs.txt", 4), "/proj/theirs.txt")
	other.Remote = "second"
	theirs, err := s.Upsert(ctx, other)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Retarget(ctx, proj.Ino, "/proj2", true); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, mine.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteID != "/proj2/mine.txt" {
		t.Fatalf("this remote's descendant = %q", got.RemoteID)
	}
	got, err = s.Get(ctx, theirs.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteID != "/proj/theirs.txt" {
		t.Fatalf("another remote's node was rewritten by our rename: %q", got.RemoteID)
	}
}

// The subtree update is unbounded in the size of the tree, so it has to be
// the caller's context that decides how long it may run.
func TestRetargetHonoursACancelledContext(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	proj, err := s.Upsert(ctx, withID(dir(RootIno, "proj"), "/proj"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(ctx, withID(file(proj.Ino, "a.txt", 1), "/proj/a.txt")); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Retarget(cancelled, proj.Ino, "/proj2", true); err == nil {
		t.Fatal("a cancelled context did not stop the retarget")
	}
	got, err := s.Get(ctx, proj.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteID != "/proj" {
		t.Fatalf("a cancelled retarget still committed: %q", got.RemoteID)
	}
}
