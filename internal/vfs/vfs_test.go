package vfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/test/fakeprovider"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

type env struct {
	fs    *FS
	fake  *fakeprovider.Fake
	j     *journal.Journal
	up    *upload.Uploader
	clk   *clock
	store *meta.Store
	cache *cache.Cache
}

type envOpt struct {
	// wrapProvider replaces the mount's backend with a decorator over the
	// fake, for tests that need a capability the fake does not have.
	wrapProvider    func(*fakeprovider.Fake) provider.Provider
	blockSize       int64
	subBlockSize    int64
	mode            config.Mode
	readAheadBlocks int
	prefetchDepth   int
	dirTTL          time.Duration
	// pathIDs runs the backend with path-shaped ids (sftp, webdav, s3, smb)
	// instead of opaque ones.
	pathIDs bool
}

func newEnv(t *testing.T, o envOpt) *env {
	t.Helper()
	if o.blockSize == 0 {
		o.blockSize = 64
	}
	if o.mode == "" {
		o.mode = config.ModeWriteback
	}
	if o.dirTTL == 0 {
		o.dirTTL = time.Minute
	}
	dir := t.TempDir()
	c := &clock{t: time.Unix(1_700_000_000, 0)}

	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	ca, err := cache.New(cache.Options{
		Dir: filepath.Join(dir, "cache"), BlockSize: o.blockSize, SubBlockSize: o.subBlockSize, Now: c.now,
		FreeSpace: func(string) (int64, error) { return 1 << 40, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal"), Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })

	fake := fakeprovider.New("ali")
	if o.pathIDs {
		fake = fakeprovider.NewPathIDs("ali")
	}
	var backend provider.Provider = fake
	if o.wrapProvider != nil {
		backend = o.wrapProvider(fake)
	}
	fs, err := New(Options{
		Meta: store, Cache: ca, Now: c.now,
		DefaultDirTTL: o.dirTTL, AttrTTL: time.Minute, NegativeTTL: 5 * time.Second,
		ReadAheadBlocks: o.readAheadBlocks, PrefetchDepth: o.prefetchDepth,
		Mounts: []Mount{{
			Prefix: "/ali", Remote: "ali", RootID: fake.RootID(),
			Provider: backend, Mode: o.mode, DirTTL: o.dirTTL,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })

	e := &env{fs: fs, fake: fake, j: j, clk: c, store: store, cache: ca}
	u, err := upload.New(upload.Options{
		Journal: j,
		Providers: func(remote string) (provider.Provider, bool) {
			if remote == "ali" {
				return backend, true
			}
			return nil, false
		},
		MaxAttempts:  3,
		Policy:       retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: time.Millisecond}, MaxAttempts: 2},
		Backoff:      retry.Backoff{Base: time.Millisecond, Max: time.Millisecond, Rand: func() float64 { return 1 }},
		PollInterval: 5 * time.Millisecond,
		Now:          c.now,
		Hooks:        fs.UploadHooks(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.up = u
	fs.SetWriteBackend(j, u)
	return e
}

func TestReadDirAndLookupUseCache(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("docs/a.txt", []byte("alpha"))
	e.fake.Seed("docs/b.txt", []byte("bravo"))

	entries, err := e.fs.ReadDirPath(ctx, "/ali/docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "a.txt" {
		t.Fatalf("listing = %+v", entries)
	}
	calls := e.fake.TotalCalls()
	if calls == 0 {
		t.Fatal("first listing should hit the provider")
	}
	// A second listing within the TTL must not touch the provider at all.
	if _, err := e.fs.ReadDirPath(ctx, "/ali/docs"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.StatPath(ctx, "/ali/docs/a.txt"); err != nil {
		t.Fatal(err)
	}
	if e.fake.TotalCalls() != calls {
		t.Fatalf("warm listing made %d extra provider calls", e.fake.TotalCalls()-calls)
	}
	// After the TTL it refreshes.
	e.clk.advance(2 * time.Minute)
	if _, err := e.fs.ReadDirPath(ctx, "/ali/docs"); err != nil {
		t.Fatal(err)
	}
	if e.fake.TotalCalls() == calls {
		t.Fatal("expired listing should refresh from the provider")
	}
}

func TestNegativeCacheAvoidsProviderCalls(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("proj/main.go", []byte("package main"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali/proj"); err != nil {
		t.Fatal(err)
	}
	dirNode, _ := e.store.Resolve(ctx, "/ali/proj")

	if _, err := e.fs.Lookup(ctx, dirNode.Ino, ".git"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing name = %v", err)
	}
	calls := e.fake.TotalCalls()
	for i := 0; i < 5; i++ {
		if _, err := e.fs.Lookup(ctx, dirNode.Ino, ".git"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing name = %v", err)
		}
	}
	if e.fake.TotalCalls() != calls {
		t.Fatalf("negative cache leaked %d provider calls", e.fake.TotalCalls()-calls)
	}
	// The negative entry expires before the directory listing does. While the
	// listing is still fresh the answer stays local: the listing is the source
	// of truth for which names exist.
	e.clk.advance(10 * time.Second)
	if _, err := e.fs.Lookup(ctx, dirNode.Ino, ".git"); !errors.Is(err, ErrNotFound) {
		t.Fatal("still missing")
	}
	if e.fake.TotalCalls() != calls {
		t.Fatalf("a fresh listing should answer locally, got %d extra calls", e.fake.TotalCalls()-calls)
	}
	// Once the listing itself expires, the lookup refreshes from the provider
	// and a name that appeared meanwhile becomes visible.
	e.clk.advance(2 * time.Minute)
	e.fake.Seed("proj/.git", []byte("gitdir"))
	if _, err := e.fs.Lookup(ctx, dirNode.Ino, ".git"); err != nil {
		t.Fatalf("name created remotely should be found after the listing TTL: %v", err)
	}
	if e.fake.TotalCalls() == calls {
		t.Fatal("expired listing should re-check the provider")
	}
}

func TestReadBlocksAndCacheHits(t *testing.T) {
	e := newEnv(t, envOpt{blockSize: 64})
	ctx := context.Background()
	content := bytes.Repeat([]byte("0123456789"), 30) // 300 bytes → 5 blocks
	e.fake.Seed("big.bin", content)

	got, err := e.fs.ReadFileRange(ctx, "/ali/big.bin", 0, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch: %d bytes", len(got))
	}
	reads := e.fake.Calls("ReadRange")
	if reads != 5 {
		t.Fatalf("expected 5 range reads for 5 blocks, got %d", reads)
	}
	// Re-reading is served entirely from the cache.
	got2, err := e.fs.ReadFileRange(ctx, "/ali/big.bin", 0, int64(len(content)))
	if err != nil || !bytes.Equal(got2, content) {
		t.Fatal("second read mismatch")
	}
	if e.fake.Calls("ReadRange") != reads {
		t.Fatalf("warm read hit the provider %d extra times", e.fake.Calls("ReadRange")-reads)
	}
	// A partial range spanning a block boundary is correct.
	mid, err := e.fs.ReadFileRange(ctx, "/ali/big.bin", 60, 10)
	if err != nil || !bytes.Equal(mid, content[60:70]) {
		t.Fatalf("cross-block read = %q", mid)
	}
	// Reading past EOF returns nothing rather than an error.
	tail, err := e.fs.ReadFileRange(ctx, "/ali/big.bin", int64(len(content)), 10)
	if err != nil || len(tail) != 0 {
		t.Fatalf("read past EOF = %q, %v", tail, err)
	}
}

func TestReadAheadFetchesFollowingBlocks(t *testing.T) {
	e := newEnv(t, envOpt{blockSize: 64, readAheadBlocks: 4})
	ctx := context.Background()
	content := bytes.Repeat([]byte("x"), 64*10)
	e.fake.Seed("seq.bin", content)

	n, err := e.store.Resolve(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.ReadDir(ctx, n.Ino); err != nil {
		t.Fatal(err)
	}
	node, err := e.store.Resolve(ctx, "/ali/seq.bin")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Open(ctx, node.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, h)

	buf := make([]byte, 64)
	// Three contiguous block reads arm the window: two adjacent blocks alone
	// are what a random reader produces by chance.
	for i := int64(0); i < 3; i++ {
		if _, err := e.fs.Read(ctx, h, buf, i*64); err != nil {
			t.Fatal(err)
		}
	}
	key := cache.FileKey{Remote: node.Remote, RemoteID: node.RemoteID, Version: node.Version}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if have, _ := e.cache.Present(key); have > 3 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	have, total := e.cache.Present(key)
	t.Fatalf("read-ahead did not prefetch: %d/%d blocks cached", have, total)
}

func TestPrefetchWarmsSubdirectories(t *testing.T) {
	e := newEnv(t, envOpt{prefetchDepth: 2})
	ctx := context.Background()
	e.fake.Seed("proj/src/main.go", []byte("m"))
	e.fake.Seed("proj/src/util.go", []byte("u"))
	e.fake.Seed("proj/docs/readme.md", []byte("r"))

	if _, err := e.fs.ReadDirPath(ctx, "/ali/proj"); err != nil {
		t.Fatal(err)
	}
	e.fs.prefetch.drain(3 * time.Second)

	calls := e.fake.TotalCalls()
	// The subdirectories should already be listed locally.
	if _, err := e.fs.ReadDirPath(ctx, "/ali/proj/src"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.ReadDirPath(ctx, "/ali/proj/docs"); err != nil {
		t.Fatal(err)
	}
	if e.fake.TotalCalls() != calls {
		t.Fatalf("prefetch missed: %d extra provider calls", e.fake.TotalCalls()-calls)
	}
}

func TestWarmWalksTree(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("a/b/c/deep.txt", []byte("d"))
	dirs, err := e.fs.Warm(ctx, "/ali", -1)
	if err != nil {
		t.Fatal(err)
	}
	if dirs < 4 {
		t.Fatalf("warm visited %d dirs, want at least 4", dirs)
	}
	calls := e.fake.TotalCalls()
	if _, err := e.fs.StatPath(ctx, "/ali/a/b/c/deep.txt"); err != nil {
		t.Fatal(err)
	}
	if e.fake.TotalCalls() != calls {
		t.Fatal("stat after warm should be local")
	}
}

func TestWriteBackCommitsLocallyThenUploads(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root, _ := e.store.Resolve(ctx, "/ali")

	h, err := e.fs.Create(ctx, root.Ino, "new.txt")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("written by cloudfs")
	if _, err := e.fs.Write(ctx, h, payload, 0); err != nil {
		t.Fatal(err)
	}
	uploadsBefore := e.fake.Calls("CompleteUpload") + e.fake.Calls("BeginUpload")
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}
	// close() returned without uploading: the write is durable locally only.
	if e.fake.Calls("CompleteUpload")+e.fake.Calls("BeginUpload") != uploadsBefore {
		t.Fatal("writeback mode must not upload synchronously")
	}
	st, _ := e.j.Stats(ctx)
	if st.Pending != 1 {
		t.Fatalf("journal stats = %+v", st)
	}
	// The new size is visible immediately.
	a, err := e.fs.StatPath(ctx, "/ali/new.txt")
	if err != nil || a.Size != int64(len(payload)) {
		t.Fatalf("stat after write = %+v, %v", a, err)
	}
	// Reading it back before the upload serves the staged bytes.
	got, err := e.fs.ReadFileRange(ctx, "/ali/new.txt", 0, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("read back before upload = %q", got)
	}

	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ = e.j.Stats(ctx)
	if st.Pending != 0 || st.Done != 1 {
		t.Fatalf("after drain = %+v", st)
	}
	// The remote now has it, and the local node points at the remote id.
	node, err := e.store.Resolve(ctx, "/ali/new.txt")
	if err != nil || node.RemoteID == "" || node.Dirty {
		t.Fatalf("node after upload = %+v, %v", node, err)
	}
	rc, err := e.fake.ReadRange(ctx, node.RemoteID, node.Version, 0, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := make([]byte, len(payload))
	rc.Read(buf)
	if !bytes.Equal(buf, payload) {
		t.Fatalf("remote content = %q", buf)
	}
	// The uploaded blob became the read cache entry: no download needed.
	reads := e.fake.Calls("ReadRange")
	got, err = e.fs.ReadFileRange(ctx, "/ali/new.txt", 0, int64(len(payload)))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("post-upload read = %q, %v", got, err)
	}
	if e.fake.Calls("ReadRange") != reads {
		t.Fatal("reading a just-uploaded file should hit the local cache")
	}
}

func TestStrictModeWaitsForUpload(t *testing.T) {
	e := newEnv(t, envOpt{mode: config.ModeStrict})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root, _ := e.store.Resolve(ctx, "/ali")
	h, err := e.fs.Create(ctx, root.Ino, "strict.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, h, []byte("durable"), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}
	// close() returned only after the upload completed.
	st, _ := e.j.Stats(ctx)
	if st.Pending != 0 || st.Done != 1 {
		t.Fatalf("strict close left work queued: %+v", st)
	}
	node, _ := e.store.Resolve(ctx, "/ali/strict.txt")
	if node.RemoteID == "" {
		t.Fatal("strict write should have a remote id after close")
	}
}

func TestReadOnlyMountRefusesWrites(t *testing.T) {
	e := newEnv(t, envOpt{mode: config.ModeReadonly})
	ctx := context.Background()
	e.fake.Seed("locked.txt", []byte("read only"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root, _ := e.store.Resolve(ctx, "/ali")
	if _, err := e.fs.Create(ctx, root.Ino, "nope.txt"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("create on readonly mount = %v", err)
	}
	node, _ := e.store.Resolve(ctx, "/ali/locked.txt")
	if _, err := e.fs.Open(ctx, node.Ino, true); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("open for write on readonly mount = %v", err)
	}
	// Reads still work.
	got, err := e.fs.ReadFileRange(ctx, "/ali/locked.txt", 0, 9)
	if err != nil || string(got) != "read only" {
		t.Fatalf("read on readonly mount = %q, %v", got, err)
	}
}

func TestOverwriteExistingFileKeepsPrefix(t *testing.T) {
	e := newEnv(t, envOpt{blockSize: 16})
	ctx := context.Background()
	original := []byte("0123456789abcdefghij") // 20 bytes
	e.fake.Seed("edit.txt", original)
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	node, _ := e.store.Resolve(ctx, "/ali/edit.txt")

	h, err := e.fs.Open(ctx, node.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	// A partial overwrite must preserve the bytes around it.
	if _, err := e.fs.Write(ctx, h, []byte("XY"), 4); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/edit.txt", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("0123XY6789abcdefghij")
	if !bytes.Equal(got, want) {
		t.Fatalf("after partial write = %q, want %q", got, want)
	}
}

func TestWriteFileAndAppend(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/notes.md", []byte("first line\n"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/notes.md", []byte("second line\n"), true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/notes.md", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first line\nsecond line\n" {
		t.Fatalf("append result = %q", got)
	}
	// Overwrite replaces the whole file.
	if _, err := e.fs.WriteFile(ctx, "/ali/notes.md", []byte("replaced"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = e.fs.ReadFileRange(ctx, "/ali/notes.md", 0, 0)
	if string(got) != "replaced" {
		t.Fatalf("overwrite result = %q", got)
	}
}

func TestMkdirRemoveRename(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root, _ := e.store.Resolve(ctx, "/ali")

	d, err := e.fs.Mkdir(ctx, root.Ino, "sub")
	if err != nil || !d.IsDir {
		t.Fatalf("mkdir = %+v, %v", d, err)
	}
	if _, err := e.fs.Mkdir(ctx, root.Ino, "sub"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate mkdir = %v", err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/sub/f.txt", []byte("hello"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	// A non-empty directory cannot be removed without recursion.
	if err := e.fs.Remove(ctx, root.Ino, "sub", false); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("rmdir non-empty = %v", err)
	}
	// Rename within the same remote.
	if err := e.fs.Rename(ctx, d.Ino, "f.txt", d.Ino, "g.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.StatPath(ctx, "/ali/sub/g.txt"); err != nil {
		t.Fatalf("renamed file missing: %v", err)
	}
	if _, err := e.fs.StatPath(ctx, "/ali/sub/f.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatal("old name should be gone")
	}
	// Remove the file, then the now-empty directory.
	if err := e.fs.Remove(ctx, d.Ino, "g.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Remove(ctx, root.Ino, "sub", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.StatPath(ctx, "/ali/sub"); !errors.Is(err, ErrNotFound) {
		t.Fatal("removed directory should be gone")
	}
}

func TestConflictCopyOnRemoteChange(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("shared.txt", []byte("original"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	node, _ := e.store.Resolve(ctx, "/ali/shared.txt")

	// Start a local edit.
	h, err := e.fs.Open(ctx, node.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, h, []byte("local change"), 0); err != nil {
		t.Fatal(err)
	}
	// Someone else changes the remote before our upload runs.
	e.fake.Seed("shared.txt", []byte("remote change by another client"))
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	// The remote original keeps the other client's content.
	e.clk.advance(2 * time.Minute) // let the listing refresh
	entries, err := e.fs.ReadDirPath(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var conflicts int
	for _, en := range entries {
		names = append(names, en.Name)
		if len(en.Name) > 8 && en.Name[:8] == "shared (" {
			conflicts++
		}
	}
	if conflicts != 1 {
		t.Fatalf("expected one conflict copy, got %d in %v", conflicts, names)
	}
}

func TestCrossRemoteRenameRefused(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 64, Now: c.now,
		FreeSpace: func(string) (int64, error) { return 1 << 40, nil }})
	if err != nil {
		t.Fatal(err)
	}
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("f.txt", []byte("x"))
	fs, err := New(Options{
		Meta: store, Cache: ca, Now: c.now, AttrTTL: time.Minute, DefaultDirTTL: time.Minute,
		Mounts: []Mount{
			{Prefix: "/a", Remote: "a", RootID: fakeprovider.RootID, Provider: a},
			{Prefix: "/b", Remote: "b", RootID: fakeprovider.RootID, Provider: b},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	ctx := context.Background()
	if _, err := fs.ReadDirPath(ctx, "/a"); err != nil {
		t.Fatal(err)
	}
	src, _ := store.Resolve(ctx, "/a")
	dst, _ := store.Resolve(ctx, "/b")
	if err := fs.Rename(ctx, src.Ino, "f.txt", dst.Ino, "f.txt"); !errors.Is(err, ErrCrossMount) {
		t.Fatalf("cross-remote rename = %v", err)
	}
}

func TestConcurrentReadsShareOneFetch(t *testing.T) {
	e := newEnv(t, envOpt{blockSize: 1 << 16})
	ctx := context.Background()
	content := bytes.Repeat([]byte("s"), 1<<16)
	e.fake.Seed("hot.bin", content)
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	// Slow the provider so the readers overlap.
	e.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.Latency = 40 * time.Millisecond })
	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			got, err := e.fs.ReadFileRange(ctx, "/ali/hot.bin", 0, 1024)
			if err == nil && !bytes.Equal(got, content[:1024]) {
				err = fmt.Errorf("content mismatch")
			}
			errCh <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	if n := e.fake.Calls("ReadRange"); n != 1 {
		t.Fatalf("8 concurrent readers caused %d fetches, want 1", n)
	}
}

func TestRecoverRequeuesAfterRestart(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/pending.txt", []byte("not yet uploaded"), false); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: the journal row exists, the upload never ran.
	rec, err := e.j.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Lost) != 0 {
		t.Fatalf("nothing should be lost: %+v", rec)
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	node, err := e.store.Resolve(ctx, "/ali/pending.txt")
	if err != nil || node.RemoteID == "" {
		t.Fatalf("recovered upload did not land: %+v, %v", node, err)
	}
}

// TestConcurrentWritesSurviveDirectoryRefresh reproduces a read-your-writes
// failure that only appeared under concurrency: while several writers create
// files in one directory, a refresh of that directory lists the backend, which
// has not received the queued uploads yet, and the entries were deleted. A
// caller then failed to read back the file it had just written.
func TestConcurrentWritesSurviveDirectoryRefresh(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	dirAttr, err := e.fs.StatPath(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n*2)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := fmt.Sprintf("/ali/concurrent-%02d.txt", i)
			body := []byte(fmt.Sprintf("body of %d", i))
			if _, err := e.fs.WriteFile(ctx, p, body, false); err != nil {
				errs <- fmt.Errorf("write %s: %w", p, err)
				return
			}
			// A refresh racing with the writes must not remove them.
			if i%4 == 0 {
				if err := e.fs.Refresh(ctx, dirAttr.Ino); err != nil {
					errs <- fmt.Errorf("refresh: %w", err)
					return
				}
			}
			got, err := e.fs.ReadFileRange(ctx, p, 0, 0)
			if err != nil {
				errs <- fmt.Errorf("read back %s: %w", p, err)
				return
			}
			if !bytes.Equal(got, body) {
				errs <- fmt.Errorf("%s = %q, want %q", p, got, body)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// One more refresh, then every file must still be there.
	if err := e.fs.Refresh(ctx, dirAttr.Ino); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("/ali/concurrent-%02d.txt", i)
		if _, err := e.fs.StatPath(ctx, p); err != nil {
			t.Errorf("%s disappeared after a refresh: %v", p, err)
		}
	}
}

// TestDropCachesGoesColdWithoutLosingLocalData: a drop must make the next
// listing and read hit the backend again, while a file whose only copy is
// local — its upload still queued — stays readable.
func TestDropCachesGoesColdWithoutLosingLocalData(t *testing.T) {
	e := newEnv(t, envOpt{blockSize: 16})
	ctx := context.Background()
	e.fake.Seed("remote.txt", []byte("served from the backend"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.ReadFileRange(ctx, "/ali/remote.txt", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/mine.txt", []byte("not uploaded yet"), false); err != nil {
		t.Fatal(err)
	}
	lists, reads := e.fake.Calls("List"), e.fake.Calls("ReadRange")

	// Warm: nothing goes to the backend.
	e.fs.ReadDirPath(ctx, "/ali")
	e.fs.ReadFileRange(ctx, "/ali/remote.txt", 0, 0)
	if e.fake.Calls("List") != lists || e.fake.Calls("ReadRange") != reads {
		t.Fatal("warm accesses should not reach the backend")
	}

	dropped, err := e.fs.DropCaches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 {
		t.Fatal("nothing was dropped")
	}
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/remote.txt", 0, 0)
	if err != nil || string(got) != "served from the backend" {
		t.Fatalf("read after drop = %q, %v", got, err)
	}
	if e.fake.Calls("List") == lists {
		t.Fatal("the listing should have been refetched after a drop")
	}
	if e.fake.Calls("ReadRange") == reads {
		t.Fatal("the blocks should have been refetched after a drop")
	}
	// The queued write is the only copy of its data and must survive.
	mine, err := e.fs.ReadFileRange(ctx, "/ali/mine.txt", 0, 0)
	if err != nil || string(mine) != "not uploaded yet" {
		t.Fatalf("local-only file after drop = %q, %v", mine, err)
	}
}

// staleLister answers List with what the backend held before a local change
// that happens while the listing is in flight — the interleaving a background
// prefetch produces against a mkdir.
type staleLister struct {
	provider.Provider
	during func()
}

func (s *staleLister) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	entries, next, err := s.Provider.List(ctx, dirID, cursor)
	if s.during != nil {
		d := s.during
		s.during = nil
		d()
	}
	return entries, next, err
}

// TestStaleListingDoesNotDeleteEntriesCreatedMeanwhile reproduces a failure
// seen while a benchmark created its dataset: a prefetch listed the parent,
// a mkdir landed, the listing was applied and the new directory vanished, so
// the next create inside it failed with ENOENT.
func TestStaleListingDoesNotDeleteEntriesCreatedMeanwhile(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root, err := e.fs.StatPath(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	m := e.mount()
	m.Provider = &staleLister{Provider: e.fake, during: func() {
		if _, err := e.fs.Mkdir(ctx, root.Ino, "fresh"); err != nil {
			t.Errorf("mkdir during listing: %v", err)
		}
	}}
	dirNode, err := e.store.Get(ctx, root.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.fetchDir(ctx, m, root.Ino, dirNode, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.StatPath(ctx, "/ali/fresh"); err != nil {
		t.Fatalf("a directory created while the listing was in flight was deleted by it: %v", err)
	}
	// The mkdir reached the backend, so the next honest listing keeps it too.
	if _, err := e.fs.Refresh(ctx, root.Ino), error(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.StatPath(ctx, "/ali/fresh"); err != nil {
		t.Fatalf("after a fresh listing: %v", err)
	}
}

// TestCancelledContextIsNotReportedAsNotFound: an interrupted request must
// surface as an interruption. Reported as "not found" it would be cached by
// the kernel as a negative entry, and a benchmark creating files saw exactly
// that as random ENOENT.
func TestCancelledContextIsNotReportedAsNotFound(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root, err := e.fs.StatPath(ctx, "/ali")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err = e.fs.MountForIno(cancelled, root.Ino)
	if err == nil {
		t.Skip("the store answered without consulting the context")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("a cancelled context was reported as not found: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the cancellation to be visible, got %v", err)
	}
}

// TestRandomReadsFetchSubBlocksNotWholeBlocks is the cold random-read
// property: a 4 KiB read that misses must cost one sub-block, not a whole
// block, or 500 random reads pull the entire file.
func TestRandomReadsFetchSubBlocksNotWholeBlocks(t *testing.T) {
	e := newEnv(t, envOpt{blockSize: 4096, subBlockSize: 512})
	ctx := context.Background()
	body := make([]byte, 64*1024)
	for i := range body {
		body[i] = byte(i * 7)
	}
	e.fake.Seed("big.bin", body)
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	at, err := e.fs.StatPath(ctx, "/ali/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Open(ctx, at.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, h)

	// One read per block, at a random spot, never twice in a block: no
	// locality, so nothing should be promoted to a whole-block fetch.
	buf := make([]byte, 100)
	for blk := int64(0); blk < 16; blk++ {
		off := blk*4096 + int64((blk*997)%3900)
		n, err := e.fs.Read(ctx, h, buf, off)
		if err != nil || n != 100 {
			t.Fatalf("read at %d: %d, %v", off, n, err)
		}
		if !bytes.Equal(buf[:n], body[off:off+100]) {
			t.Fatalf("read at %d returned wrong bytes", off)
		}
	}
	calls := e.fake.Calls("ReadRange")
	if calls != 16 {
		t.Fatalf("16 isolated reads made %d backend calls, want 16", calls)
	}
	// 16 reads × at most 2 sub-blocks each (a 100-byte read can straddle
	// two 512-byte sub-blocks) is far below 16 whole 4 KiB blocks.
	if st := e.cache.Stats(); st.Bytes > 16*1024 {
		t.Fatalf("cache holds %d bytes after 16 small reads; whole blocks were fetched", st.Bytes)
	}
	// Reading the same spots again is free.
	for blk := int64(0); blk < 16; blk++ {
		off := blk*4096 + int64((blk*997)%3900)
		if _, err := e.fs.Read(ctx, h, buf, off); err != nil {
			t.Fatal(err)
		}
	}
	if e.fake.Calls("ReadRange") != calls {
		t.Fatal("repeated reads of cached sub-blocks reached the backend")
	}
	// Repeated misses inside one block show locality: the rest of that
	// block is fetched in the background and later reads there are local.
	h2, _ := e.fs.Open(ctx, at.Ino, false)
	defer e.fs.Release(ctx, h2)
	// Block 9's first-loop read touched sub-block 2; these hit sub-blocks
	// 4 to 7, so every one of them is a miss.
	for i := int64(0); i < subBlockPromote; i++ {
		if _, err := e.fs.Read(ctx, h2, buf, 9*4096+2200+i*512); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	n9 := e.nodeOf(t, "/ali/big.bin")
	key := cache.FileKey{Remote: n9.Remote, RemoteID: n9.RemoteID, Version: n9.Version}
	for !e.cache.Has(key, 9) {
		if time.Now().After(deadline) {
			t.Fatalf("block with repeated misses was never completed in the background: calls=%d stats=%+v",
				e.fake.Calls("ReadRange"), e.cache.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A sequential reader still takes whole blocks: read block 10 and 11 in
	// order with a large buffer and check the whole blocks arrived.
	big := make([]byte, 4096)
	h3, _ := e.fs.Open(ctx, at.Ino, false)
	defer e.fs.Release(ctx, h3)
	for blk := int64(10); blk <= 12; blk++ {
		if _, err := e.fs.Read(ctx, h3, big, blk*4096); err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/big.bin", 0, 0)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("full read after mixed access is wrong: %v", err)
	}
}

// nodeOf resolves a path to its tree node.
func (e *env) nodeOf(t *testing.T, p string) meta.Node {
	t.Helper()
	n, err := e.store.Resolve(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestListingDoesNotRemoveAFileBeingCreated: a file that exists as an open
// write handle — created, not yet committed, so it has no remote id — must
// survive a listing that started after it appeared and cannot see it.
func TestListingDoesNotRemoveAFileBeingCreated(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root, _ := e.fs.StatPath(ctx, "/ali")
	h, err := e.fs.Create(ctx, root.Ino, "open.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, h, []byte("half"), 0); err != nil {
		t.Fatal(err)
	}
	// A listing that started after the create and does not include it.
	if err := e.fs.Refresh(ctx, root.Ino); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.StatPath(ctx, "/ali/open.txt"); err != nil {
		t.Fatalf("an open, uncommitted file was removed by a listing: %v", err)
	}
	if err := e.fs.Sync(ctx, h); err != nil {
		t.Fatalf("commit after the listing: %v", err)
	}
	e.fs.Release(ctx, h)
	got, err := e.fs.ReadFileRange(ctx, "/ali/open.txt", 0, 0)
	if err != nil || string(got) != "half" {
		t.Fatalf("read back = %q, %v", got, err)
	}
}
