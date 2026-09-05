package vfs

import (
	"bytes"
	"context"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

type cursorResetProvider struct {
	provider.Provider
	cursor string
}

func (p cursorResetProvider) Capabilities() provider.Caps {
	caps := p.Provider.Capabilities()
	caps.Delta = true
	return caps
}

func (p cursorResetProvider) Changes(context.Context, string) ([]provider.Change, string, error) {
	return nil, "", &provider.CursorResetError{Cursor: p.cursor}
}

// mountOf returns the single mount of a test env.
func (e *env) mount() Mount { return e.fs.Mounts()[0] }

func TestRefresherAppliesRemoteEdits(t *testing.T) {
	e := newEnv(t, envOpt{blockSize: 16})
	ctx := context.Background()
	e.fake.Seed("shared.txt", []byte("first version"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	// Read it so the cache holds blocks for the old version.
	if _, err := e.fs.ReadFileRange(ctx, "/ali/shared.txt", 0, 0); err != nil {
		t.Fatal(err)
	}
	before, _ := e.store.Resolve(ctx, "/ali/shared.txt")
	oldKey := cache.FileKey{Remote: before.Remote, RemoteID: before.RemoteID, Version: before.Version}
	if have, _ := e.cache.Present(oldKey); have == 0 {
		t.Fatal("expected the first version to be cached")
	}

	r := NewRefresher(e.fs, time.Minute)
	// Start from the current position so only the edit below is seen.
	if err := e.store.SetCursor(ctx, "ali", e.fake.Cursor()); err != nil {
		t.Fatal(err)
	}
	// Someone else edits the file.
	e.fake.Seed("shared.txt", []byte("second version, longer"))

	n, err := r.PollOnce(ctx, e.mount())
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("the refresher applied nothing after a remote edit")
	}
	after, err := e.store.Resolve(ctx, "/ali/shared.txt")
	if err != nil {
		t.Fatal(err)
	}
	if after.Version == before.Version {
		t.Fatalf("version not updated: %s", after.Version)
	}
	if after.Size != int64(len("second version, longer")) {
		t.Fatalf("size not updated: %d", after.Size)
	}
	if after.RemoteVersion != after.Version {
		t.Fatalf("remote version should track the observed version: %q vs %q", after.RemoteVersion, after.Version)
	}
	// The stale blocks are gone, so a read returns the new content.
	if have, _ := e.cache.Present(oldKey); have != 0 {
		t.Fatal("blocks for the superseded version should have been dropped")
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/shared.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("second version, longer")) {
		t.Fatalf("read after refresh = %q", got)
	}
}

func TestRefresherAppliesRemoteDeletes(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	entry := e.fake.Seed("doomed.txt", []byte("bye"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Resolve(ctx, "/ali/doomed.txt"); err != nil {
		t.Fatal(err)
	}
	r := NewRefresher(e.fs, time.Minute)
	e.store.SetCursor(ctx, "ali", e.fake.Cursor())
	if err := e.fake.Delete(ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PollOnce(ctx, e.mount()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.StatPath(ctx, "/ali/doomed.txt"); err == nil {
		t.Fatal("a remotely deleted file should disappear locally")
	}
}

// TestRefresherDoesNotDestroyPendingWrites is the important safety property: a
// change event about the version our local write was based on must not throw
// that write away.
func TestRefresherDoesNotDestroyPendingWrites(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	entry := e.fake.Seed("mine.txt", []byte("remote copy"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	// Write locally without uploading.
	if _, err := e.fs.WriteFile(ctx, "/ali/mine.txt", []byte("my unsaved edit"), false); err != nil {
		t.Fatal(err)
	}
	r := NewRefresher(e.fs, time.Minute)
	e.store.SetCursor(ctx, "ali", e.fake.Cursor())
	// The remote deletes the file underneath us.
	if err := e.fake.Delete(ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PollOnce(ctx, e.mount()); err != nil {
		t.Fatal(err)
	}
	// The local write survives and is still readable.
	got, err := e.fs.ReadFileRange(ctx, "/ali/mine.txt", 0, 0)
	if err != nil {
		t.Fatalf("the pending write must survive a remote delete: %v", err)
	}
	if string(got) != "my unsaved edit" {
		t.Fatalf("content = %q", got)
	}
	st, _ := e.j.Stats(ctx)
	if st.Pending == 0 {
		t.Fatal("the queued upload should still be queued")
	}
}

func TestRefresherAdvancesCursorAndIsIdempotent(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("a.txt", []byte("a"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	r := NewRefresher(e.fs, time.Minute)
	e.store.SetCursor(ctx, "ali", e.fake.Cursor())
	e.fake.Seed("a.txt", []byte("a changed"))

	first, err := r.PollOnce(ctx, e.mount())
	if err != nil {
		t.Fatal(err)
	}
	if first == 0 {
		t.Fatal("first poll applied nothing")
	}
	// Polling again with no new changes does nothing and costs one call.
	second, err := r.PollOnce(ctx, e.mount())
	if err != nil {
		t.Fatal(err)
	}
	if second != 0 {
		t.Fatalf("a second poll applied %d events, want 0", second)
	}
	polls, applied := r.Stats()
	if polls != 2 || applied != first {
		t.Fatalf("stats = %d polls, %d applied", polls, applied)
	}
}

func TestRefresherSkipsRemotesWithoutAChangeFeed(t *testing.T) {
	e := newEnv(t, envOpt{})
	// Report no delta support even though the fake implements the interface:
	// the capability matrix is the contract.
	caps := e.fake.Capabilities()
	caps.Delta = false
	e.fake.SetCaps(caps)

	r := NewRefresher(e.fs, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	time.Sleep(60 * time.Millisecond)
	r.Stop()
	cancel()
	if polls, _ := r.Stats(); polls != 0 {
		t.Fatalf("a remote that does not advertise delta should not be polled, got %d polls", polls)
	}
}

func TestRefresherCursorResetInvalidatesListingsAndAdoptsBaseline(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("cached.txt", []byte("cached"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	before, err := e.store.DirState(ctx, root.Ino)
	if err != nil || !before.Complete {
		t.Fatalf("precondition state = %+v, %v", before, err)
	}
	if err := e.store.SetCursor(ctx, "ali", "expired"); err != nil {
		t.Fatal(err)
	}
	mount := e.mount()
	mount.Provider = cursorResetProvider{Provider: mount.Provider, cursor: "new-baseline"}
	invalidations := 0
	e.fs.SetInvalidateAll(func() { invalidations++ })
	r := NewRefresher(e.fs, time.Minute)
	if applied, err := r.PollOnce(ctx, mount); err != nil || applied != 0 {
		t.Fatalf("reset poll = %d, %v", applied, err)
	}
	if got, err := e.store.Cursor(ctx, "ali"); err != nil || got != "new-baseline" {
		t.Fatalf("replacement cursor = %q, %v", got, err)
	}
	after, err := e.store.DirState(ctx, root.Ino)
	if err != nil || after.Complete {
		t.Fatalf("reset left listing fresh: %+v, %v", after, err)
	}
	if invalidations != 1 {
		t.Fatalf("kernel-wide invalidations = %d, want 1", invalidations)
	}
	if polls, applied := r.Stats(); polls != 1 || applied != 0 {
		t.Fatalf("stats = polls %d, applied %d", polls, applied)
	}

	mount.Provider = cursorResetProvider{Provider: mount.Provider, cursor: "new-baseline"}
	if _, err := r.PollOnce(ctx, mount); err == nil {
		t.Fatal("a replacement cursor equal to the stored cursor should be rejected")
	}
}

func TestRefresherStartStop(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.fake.Seed("x.txt", []byte("x"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	e.store.SetCursor(ctx, "ali", e.fake.Cursor())

	r := NewRefresher(e.fs, 10*time.Millisecond)
	r.Start(ctx)
	e.fake.Seed("x.txt", []byte("x changed"))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, applied := r.Stats(); applied > 0 {
			r.Stop()
			// Stop is idempotent and does not hang.
			r.Stop()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.Stop()
	polls, applied := r.Stats()
	t.Fatalf("background refresher applied nothing: %d polls, %d applied", polls, applied)
}

func TestRefresherIgnoresUnknownParents(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	// Never list anything, so nothing is cached.
	r := NewRefresher(e.fs, time.Minute)
	e.store.SetCursor(ctx, "ali", e.fake.Cursor())
	e.fake.Seed("unseen/deep/file.txt", []byte("x"))

	if _, err := r.PollOnce(ctx, e.mount()); err != nil {
		t.Fatal(err)
	}
	// The most it may do is mark a directory it does have stale. It must never
	// invent tree entries for paths it cannot place, because a fabricated
	// entry would be served to readdir as if it had been verified.
	if _, err := e.store.Resolve(ctx, "/ali/unseen"); err == nil {
		t.Fatal("the refresher should not create tree entries it cannot place")
	}
	if _, err := e.store.Resolve(ctx, "/ali/unseen/deep"); err == nil {
		t.Fatal("the refresher should not create nested entries either")
	}
	// The mount root is marked stale so the next listing discovers the subtree.
	root, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	st, err := e.store.DirState(ctx, root.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if st.Complete {
		t.Fatal("a change under an unlisted subtree should mark the known parent stale")
	}
}

var _ = fakeprovider.RootID
