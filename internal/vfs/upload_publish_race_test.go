package vfs

import (
	"context"
	"strings"
	"testing"

	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
)

// TestACompletingUploadCannotRevertANewerWrite is the regression for a rewrite
// that reported the previous content.
//
// Finishing an upload and committing a newer write are two read-modify-write
// sequences over the same metadata row. The uploader reads the node, decides
// what the file should now look like, and writes that back; a close(2) in
// between commits a newer version to the same row. Whichever wrote second used
// to win, so `os.WriteFile` twice in a row could leave the file reporting the
// first write's size and pointing at the first write's remote object — with
// the second write's upload still queued behind it.
//
// The publication is conditional on the identity the upload was published
// under, so the loser notices instead of overwriting. Making AdoptByIno an
// unconditional UpdateByIno again makes this fail.
func TestACompletingUploadCannotRevertANewerWrite(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	first := strings.Repeat("a", 4096)
	if _, err := e.fs.WriteFile(ctx, "/ali/rewrite.bin", []byte(first), false); err != nil {
		t.Fatal(err)
	}
	queued, err := e.j.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("the first write queued %d uploads, want 1", len(queued))
	}
	firstUpload := queued[0]

	// The second write commits from inside the first upload's completion,
	// exactly between the row it read and the row it is about to write.
	second := strings.Repeat("b", 64)
	var writeErr error
	e.fs.publishFault = func(phase string) error {
		if phase != "upload-result-read" {
			return nil
		}
		e.fs.publishFault = nil // once: the second write publishes too
		_, writeErr = e.fs.WriteFile(ctx, "/ali/rewrite.bin", []byte(second), false)
		return nil
	}
	hooks := e.fs.UploadHooks()
	if err := hooks.OnSuccess(ctx, firstUpload, upload.Result{Entry: provider.Entry{
		ID: "remote-first", Kind: provider.KindFile, Size: int64(len(first)), Version: "v-first",
	}}); err != nil {
		t.Fatal(err)
	}
	e.fs.publishFault = nil
	if writeErr != nil {
		t.Fatalf("the interleaved write failed: %v", writeErr)
	}

	attr, err := e.fs.StatPath(ctx, "/ali/rewrite.bin")
	if err != nil {
		t.Fatal(err)
	}
	if attr.Size != int64(len(second)) {
		t.Fatalf("the file reports %d bytes; the completing upload put the previous content's size back", attr.Size)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/rewrite.bin", 0, int64(len(second)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != second {
		t.Fatalf("read back %q, want the second write", string(got))
	}
	node, err := e.store.Get(ctx, attr.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if node.RemoteID == "remote-first" {
		t.Fatal("the node points at the object the superseded upload created")
	}
	// The superseded upload still has to report where it left the remote: the
	// next upload's conflict check compares against exactly that.
	if node.RemoteVersion != "v-first" {
		t.Fatalf("remote version is %q; the superseded upload must still record what it left on the provider", node.RemoteVersion)
	}
}

// TestASupersededUploadOnlyRecordsTheRemoteVersion covers the other branch:
// the node had already moved on when the upload finished, so nothing but the
// remote version may be taken from it.
func TestASupersededUploadOnlyRecordsTheRemoteVersion(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/doc.bin", []byte(strings.Repeat("a", 4096)), false); err != nil {
		t.Fatal(err)
	}
	queued, err := e.j.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stale := queued[0]
	// A newer write lands before the older upload reports back.
	small := "second"
	if _, err := e.fs.WriteFile(ctx, "/ali/doc.bin", []byte(small), false); err != nil {
		t.Fatal(err)
	}
	before, err := e.fs.StatPath(ctx, "/ali/doc.bin")
	if err != nil {
		t.Fatal(err)
	}
	hooks := e.fs.UploadHooks()
	if err := hooks.OnSuccess(ctx, stale, upload.Result{Entry: provider.Entry{
		ID: "remote-stale", Kind: provider.KindFile, Size: 4096, Version: "v-stale",
	}}); err != nil {
		t.Fatal(err)
	}
	after, err := e.fs.StatPath(ctx, "/ali/doc.bin")
	if err != nil {
		t.Fatal(err)
	}
	if after.Size != before.Size || after.Version != before.Version {
		t.Fatalf("a superseded upload changed the node from %+v to %+v", before, after)
	}
	node, err := e.store.Get(ctx, after.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if node.RemoteVersion != "v-stale" {
		t.Fatalf("remote version is %q, want the version the superseded upload left", node.RemoteVersion)
	}
}

// TestAPathTruncateUsesTheOpenHandleInsteadOfMakingAnother: every write handle
// owns a private staging file, so a truncate that opened one of its own would
// commit a second, competing snapshot of the same inode. Two uploads for one
// write means the file's content after close(2) is decided by which of them
// happened to land second.
func TestAPathTruncateUsesTheOpenHandleInsteadOfMakingAnother(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/doc.bin", []byte(strings.Repeat("a", 4096)), false); err != nil {
		t.Fatal(err)
	}
	attr, err := e.fs.StatPath(ctx, "/ali/doc.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}

	// This is a rewrite: open for writing, truncate by path, write, close.
	h, err := e.fs.Open(ctx, attr.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.TruncatePath(ctx, attr.Ino, 0); err != nil {
		t.Fatal(err)
	}
	queued, err := e.j.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Fatalf("the truncate committed %d uploads of its own; it must go through the handle that is already open", len(queued))
	}
	// The truncate is visible immediately, or a program that truncates and
	// then checks the size sees the content it just removed.
	if a, err := e.fs.Stat(ctx, attr.Ino); err != nil || a.Size != 0 {
		t.Fatalf("after truncating, stat reports %d bytes (%v)", a.Size, err)
	}
	body := []byte("rewritten")
	if _, err := e.fs.Write(ctx, h, body, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}
	queued, err = e.j.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("the rewrite queued %d uploads, want exactly 1", len(queued))
	}
	if queued[0].Size != int64(len(body)) {
		t.Fatalf("the queued upload is %d bytes, want %d", queued[0].Size, len(body))
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/doc.bin", 0, int64(len(body)))
	if err != nil || string(got) != string(body) {
		t.Fatalf("read back %q (%v), want %q", string(got), err, string(body))
	}
}

// TestAPathTruncateWithNoOpenHandleStillApplies: the fallback must keep
// working, or truncate(2) on a file nobody has open would do nothing.
func TestAPathTruncateWithNoOpenHandleStillApplies(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/doc.bin", []byte(strings.Repeat("a", 4096)), false); err != nil {
		t.Fatal(err)
	}
	attr, err := e.fs.StatPath(ctx, "/ali/doc.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.TruncatePath(ctx, attr.Ino, 16); err != nil {
		t.Fatal(err)
	}
	a, err := e.fs.Stat(ctx, attr.Ino)
	if err != nil || a.Size != 16 {
		t.Fatalf("after truncating with no open handle, stat reports %d bytes (%v)", a.Size, err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if content, ok := e.fake.Content("doc.bin"); !ok || len(content) != 16 {
		t.Fatalf("the backend holds %d bytes, want 16", len(content))
	}
}
