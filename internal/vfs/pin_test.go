package vfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/meta"
	"cloudfs/test/fakeprovider"
)

func pinFS(t *testing.T, dir string, backend *fakeprovider.Fake, budget int64, configured bool) *FS {
	t.Helper()
	s, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 64, MaxBytes: budget})
	if err != nil {
		t.Fatal(err)
	}
	f, err := New(Options{Meta: s, Cache: c, Mounts: []Mount{{Prefix: "/ali", Remote: "ali", RootID: fakeprovider.RootID, Provider: backend, Pin: configured}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close(); c.Close(); s.Close() })
	return f
}

func pinnedKey(t *testing.T, f *FS, p string) cache.FileKey {
	t.Helper()
	n, err := f.meta.Resolve(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
}

func TestPinProtectsBeforeFillingAndReportsInsufficientSpace(t *testing.T) {
	p := fakeprovider.New("ali")
	p.Seed("/big", bytes.Repeat([]byte("x"), 128))
	f := pinFS(t, t.TempDir(), p, 64, false)
	err := f.Pin(context.Background(), "/ali/big")
	if !errors.Is(err, ErrNoSpace) && !errors.Is(err, cache.ErrNoSpace) {
		t.Fatalf("pin too large = %v", err)
	}
	k := pinnedKey(t, f, "/ali/big")
	if !f.cache.IsPinned(k) || !f.cache.Has(k, 0) || f.cache.Has(k, 1) {
		t.Fatal("partial pin was evicted or falsely completed")
	}
	if len(f.PinPolicies()) != 1 {
		t.Fatal("failed pin lost its durable intent")
	}
	if err := f.Unpin(context.Background(), "/ali/big"); err != nil {
		t.Fatal(err)
	}
	if f.cache.IsPinned(k) {
		t.Fatal("unpin kept user protection")
	}
	if !f.cache.Has(k, 0) {
		t.Fatal("unpin deleted content")
	}
}

func TestPinSurvivesRestartWithoutNetworkAndUnpinAllowsEviction(t *testing.T) {
	ctx := context.Background()
	p := fakeprovider.New("ali")
	p.Seed("/a", []byte("first"))
	dir := t.TempDir()
	f := pinFS(t, dir, p, 64, false)
	if err := f.Pin(ctx, "/ali/a"); err != nil {
		t.Fatal(err)
	}
	k := pinnedKey(t, f, "/ali/a")
	f.Close()
	f.cache.Close()
	f.meta.Close()
	g := pinFS(t, dir, p, 64, false)
	if !g.cache.IsPinned(k) || !g.cache.Has(k, 0) {
		t.Fatal("restart lost pin protection")
	}
	other := cache.FileKey{Remote: "other", RemoteID: "other", Version: "1"}
	if err := g.cache.Put(other, 0, make([]byte, 64), 64); !errors.Is(err, cache.ErrNoSpace) {
		t.Fatalf("pin evicted: %v", err)
	}
	if err := g.Unpin(ctx, "/ali/a"); err != nil {
		t.Fatal(err)
	}
	if err := g.cache.Put(other, 0, make([]byte, 64), 64); err != nil {
		t.Fatal(err)
	}
	if g.cache.Has(k, 0) {
		t.Fatal("released pin still unevictable")
	}
}

func TestOverlappingPinsAndTemporaryWriteProtection(t *testing.T) {
	ctx := context.Background()
	p := fakeprovider.New("ali")
	p.Seed("/a", []byte("a"))
	p.Seed("/b", []byte("b"))
	f := pinFS(t, t.TempDir(), p, 0, false)
	for _, path := range []string{"/ali", "/ali/a"} {
		if err := f.Pin(ctx, path); err != nil {
			t.Fatal(err)
		}
	}
	a, b := pinnedKey(t, f, "/ali/a"), pinnedKey(t, f, "/ali/b")
	f.cache.Pin(b, true)
	if err := f.Unpin(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if !f.cache.IsPinned(a) || !f.cache.IsPinned(b) {
		t.Fatal("removed nested pin or pending-write protection")
	}
	f.cache.Pin(b, false)
	if f.cache.IsPinned(b) {
		t.Fatal("parent user pin not released")
	}
	f.cache.Pin(a, true)
	f.cache.Pin(a, false)
	if !f.cache.IsPinned(a) {
		t.Fatal("temporary unpin removed user policy")
	}
	if err := f.Unpin(ctx, "/ali/a"); err != nil {
		t.Fatal(err)
	}
	if f.cache.IsPinned(a) {
		t.Fatal("exact pin not released")
	}
}

func TestConfiguredPinAndRefreshFindNewChildren(t *testing.T) {
	ctx := context.Background()
	p := fakeprovider.New("ali")
	p.Seed("/a", []byte("first"))
	f := pinFS(t, t.TempDir(), p, 0, true)
	if err := f.RefreshPins(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.cache.IsPinned(pinnedKey(t, f, "/ali/a")) {
		t.Fatal("config pin ignored")
	}
	if err := f.Unpin(ctx, "/ali"); err == nil {
		t.Fatal("silently removed configured pin")
	}
	p.Seed("/b", []byte("new"))
	root, _ := f.meta.Resolve(ctx, "/ali")
	if err := f.meta.Invalidate(ctx, root.Ino); err != nil {
		t.Fatal(err)
	}
	if err := f.RefreshPins(ctx); err != nil {
		t.Fatal(err)
	}
	k := pinnedKey(t, f, "/ali/b")
	if !f.cache.IsPinned(k) || !f.cache.Has(k, 0) {
		t.Fatal("new child not filled and pinned")
	}
}

func TestRenameDoesNotMovePathPin(t *testing.T) {
	ctx := context.Background()
	p := fakeprovider.New("ali")
	p.Seed("/a", []byte("first"))
	f := pinFS(t, t.TempDir(), p, 0, false)
	if err := f.Pin(ctx, "/ali/a"); err != nil {
		t.Fatal(err)
	}
	k := pinnedKey(t, f, "/ali/a")
	root, _ := f.meta.Resolve(ctx, "/ali")
	if err := f.Rename(ctx, root.Ino, "a", root.Ino, "b"); err != nil {
		t.Fatal(err)
	}
	if f.cache.IsPinned(k) {
		t.Fatal("pin followed renamed file instead of path")
	}
	if err := f.Unpin(ctx, "/ali/a"); err != nil {
		t.Fatal("unpin of vanished path must be offline:", err)
	}
}

func TestPinDoesNotSucceedWhenCacheWritebackFails(t *testing.T) {
	p := fakeprovider.New("ali")
	p.Seed("/a", []byte("cannot persist"))
	f := pinFS(t, t.TempDir(), p, 0, false)
	blocks := filepath.Join(f.cache.Dir(), "blocks")
	if err := os.Remove(blocks); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocks, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.Pin(context.Background(), "/ali/a"); err == nil {
		t.Fatal("reported successful pin despite rejected disk write")
	}
	if len(f.PinPolicies()) != 1 {
		t.Fatal("lost incomplete pin intent")
	}
}

func TestPinRecoveryResumesOnlyMissingBlocks(t *testing.T) {
	ctx := context.Background()
	p := fakeprovider.New("ali")
	p.Seed("/a", bytes.Repeat([]byte("a"), 128))
	dir := t.TempDir()
	f := pinFS(t, dir, p, 64, false)
	if err := f.Pin(ctx, "/ali/a"); err == nil {
		t.Fatal("expected partial pin")
	}
	k := pinnedKey(t, f, "/ali/a")
	f.Close()
	f.cache.Close()
	f.meta.Close()
	before := p.ReadBytes()
	g := pinFS(t, dir, p, 256, false)
	if p.ReadBytes() != before {
		t.Fatal("opening VFS must not initiate downloads")
	}
	if err := g.RefreshPins(ctx); err != nil {
		t.Fatal(err)
	}
	if p.ReadBytes()-before != 64 || !g.cache.Has(k, 0) || !g.cache.Has(k, 1) {
		t.Fatal("recovery did not reuse already cached block")
	}
}

func TestPinWorkerResumesAndStops(t *testing.T) {
	ctx := context.Background()
	p := fakeprovider.New("ali")
	p.Seed("/a", []byte("resume"))
	f := pinFS(t, t.TempDir(), p, 0, false)
	if err := f.meta.AddPin(ctx, meta.Pin{Path: "/ali/a"}); err != nil {
		t.Fatal(err)
	}
	if err := f.loadPins(ctx); err != nil {
		t.Fatal(err)
	}
	f.StartPins(ctx, time.Hour)
	deadline := time.After(3 * time.Second)
	for f.cache.Stats().PinnedBlocks != 1 {
		select {
		case <-deadline:
			t.Fatal("pin worker did not resume persisted policy")
		case <-time.After(time.Millisecond):
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPinAliasesAreAdditive(t *testing.T) {
	ctx := context.Background()
	p := fakeprovider.New("ali")
	p.Seed("/a", []byte("shared"))
	f := pinFS(t, t.TempDir(), p, 0, false)
	f.mounts = append(f.mounts, Mount{Prefix: "/alias", Remote: "ali", RootID: fakeprovider.RootID, Provider: p})
	if err := f.ensureMountDirs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if err := f.Pin(ctx, "/alias/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadFileRange(ctx, "/ali/a", 0, 6); err != nil {
		t.Fatal(err)
	}
	k := pinnedKey(t, f, "/ali/a")
	if !f.cache.IsPinned(k) {
		t.Fatal("reading unpinned alias removed other alias's retention")
	}
	if err := f.loadPins(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.cache.IsPinned(k) {
		t.Fatal("reconcile considered only first alias")
	}
}

func TestConcurrentPinUnpinAndRead(t *testing.T) {
	ctx := context.Background()
	p := fakeprovider.New("ali")
	p.Seed("/a", []byte("shared"))
	f := pinFS(t, t.TempDir(), p, 0, false)
	if err := f.Pin(ctx, "/ali/a"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				switch i {
				case 0:
					_ = f.Pin(ctx, "/ali/a")
				case 1:
					if err := f.Unpin(ctx, "/ali/a"); err != nil {
						t.Error(err)
					}
				case 2:
					if _, err := f.ReadFileRange(ctx, "/ali/a", 0, 6); err != nil {
						t.Error(err)
					}
				}
			}
		}(i)
	}
	wg.Wait()
	if err := f.Unpin(ctx, "/ali/a"); err != nil {
		t.Fatal(err)
	}
	if f.cache.IsPinned(pinnedKey(t, f, "/ali/a")) {
		t.Fatal("stale concurrent pin remained after final unpin")
	}
}

func TestPinnedDirectoryKeepsNewlyUploadedContent(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if err := e.fs.Pin(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	root, _ := e.store.Resolve(ctx, "/ali")
	h, err := e.fs.Create(ctx, root.Ino, "new")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, h, []byte("pin after upload"), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}
	local := pinnedKey(t, e.fs, "/ali/new")
	if !e.cache.IsPinned(local) {
		t.Fatal("pending write lost protection")
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	k := pinnedKey(t, e.fs, "/ali/new")
	if IsLocalOnly(k.RemoteID) || !e.cache.IsPinned(k) {
		t.Fatal("upload landing lost directory pin")
	}
	if _, ok := e.cache.HydratedPath(k); !ok {
		t.Fatal("uploaded bytes not reused locally")
	}
	if err := e.fs.Unpin(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if e.cache.IsPinned(k) {
		t.Fatal("uploaded key did not release user pin")
	}
}

func TestPinWorkerCannotStartAfterClose(t *testing.T) {
	p := fakeprovider.New("ali")
	f := pinFS(t, t.TempDir(), p, 0, false)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); f.StartPins(context.Background(), time.Hour) }()
		go func() { defer wg.Done(); f.Close() }()
	}
	wg.Wait()
	f.StartPins(context.Background(), time.Hour)
	if !f.pinClosed {
		t.Fatal("close did not terminate pin worker lifecycle")
	}
}
