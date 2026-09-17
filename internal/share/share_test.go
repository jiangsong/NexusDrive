package share

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

func newStack(t *testing.T) (*vfs.FS, *fakeprovider.Fake, *upload.Uploader) {
	t.Helper()
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	fake := fakeprovider.New("ali")
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca, AttrTTL: time.Minute, DefaultDirTTL: time.Minute, NegativeTTL: time.Second,
		Mounts: []vfs.Mount{{Prefix: "/", Remote: "ali", RootID: fakeprovider.RootID, Provider: fake, Mode: config.ModeWriteback, DirTTL: time.Minute}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	up, err := upload.New(upload.Options{
		Journal:   j,
		Providers: func(string) (provider.Provider, bool) { return fake, true },
		Policy:    retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: time.Millisecond}, MaxAttempts: 2},
		Hooks:     fsys.UploadHooks(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fsys.SetWriteBackend(j, up)
	return fsys, fake, up
}

// TestShareRequiresConfirm: without confirm nothing is looked at and
// nothing is asked of the provider.
func TestShareRequiresConfirm(t *testing.T) {
	fsys, fake, _ := newStack(t)
	fake.Seed("report.md", []byte("# Q3\n"))
	before := fake.TotalCalls()
	if _, err := Create(context.Background(), fsys, Request{Path: "/report.md"}); !errors.Is(err, ErrConfirm) {
		t.Fatalf("without confirm: %v", err)
	}
	if fake.TotalCalls() != before {
		t.Fatal("an unconfirmed share touched the provider")
	}
}

// TestShareRefusesUncachedFile: a file the drive holds but the cache does
// not is refused with the pin hint and costs no download (fake ReadRange
// count 0); once pinned it shares with exactly one CreateShare; a file
// not uploaded yet is refused as not synced.
func TestShareRefusesUncachedFile(t *testing.T) {
	fsys, fake, up := newStack(t)
	ctx := context.Background()
	fake.Seed("report.md", []byte("# Q3 report\n\nAll good.\n"))
	if _, err := fsys.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	reads := fake.Calls("ReadRange")
	_, err := Create(ctx, fsys, Request{Path: "/report.md", Confirm: true})
	if !errors.Is(err, ErrNotCached) || !strings.Contains(err.Error(), "pin") {
		t.Fatalf("uncached: %v", err)
	}
	if fake.Calls("ReadRange") != reads || fake.Calls("CreateShare") != 0 {
		t.Fatalf("a refused share downloaded or shared: reads %d shares %d", fake.Calls("ReadRange")-reads, fake.Calls("CreateShare"))
	}
	if err := fsys.Pin(ctx, "/report.md"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Prefetch(ctx, "/report.md"); err != nil {
		t.Fatal(err)
	}
	res, err := Create(ctx, fsys, Request{Path: "/report.md", Confirm: true, Expires: time.Hour})
	if err != nil || res.URL == "" || res.ExpiresAt.IsZero() || len(res.Findings) != 0 {
		t.Fatalf("cached share: %+v %v", res, err)
	}
	if fake.Calls("CreateShare") != 1 {
		t.Fatalf("CreateShare calls: %d", fake.Calls("CreateShare"))
	}
	// Not uploaded yet.
	up.Stop()
	if _, err := fsys.WriteFile(ctx, "/draft.md", []byte("draft\n"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, fsys, Request{Path: "/draft.md", Confirm: true}); !errors.Is(err, ErrNotSynced) {
		t.Fatalf("local-only: %v", err)
	}
	if _, err := Create(ctx, fsys, Request{Path: "/", Confirm: true}); !errors.Is(err, vfs.ErrIsDir) {
		t.Fatalf("directory: %v", err)
	}
}

// TestShareRefusesCredentialLookingContent: content with a credential
// shape is refused with the rule and line, and shared only with force,
// which the result says.
func TestShareRefusesCredentialLookingContent(t *testing.T) {
	fsys, fake, _ := newStack(t)
	ctx := context.Background()
	fake.Seed("env.md", []byte("# Setup\n\nexport AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE\n"))
	if _, err := fsys.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Pin(ctx, "/env.md"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Prefetch(ctx, "/env.md"); err != nil {
		t.Fatal(err)
	}
	res, err := Create(ctx, fsys, Request{Path: "/env.md", Confirm: true})
	if !errors.Is(err, ErrCredentials) || !strings.Contains(err.Error(), "aws-access-key at line 3") || len(res.Findings) != 1 {
		t.Fatalf("credential content: %+v %v", res, err)
	}
	if strings.Contains(err.Error(), "AKIA") {
		t.Fatal("the refusal leaks the matched text")
	}
	if fake.Calls("CreateShare") != 0 {
		t.Fatal("a refused share was created")
	}
	forced, err := Create(ctx, fsys, Request{Path: "/env.md", Confirm: true, Force: true})
	if err != nil || !forced.Forced || forced.URL == "" || len(forced.Findings) != 1 {
		t.Fatalf("forced: %+v %v", forced, err)
	}
}

// TestShareUnsupportedRemote: a provider without Caps.Share is refused
// before any call.
func TestShareUnsupportedRemote(t *testing.T) {
	fsys, fake, _ := newStack(t)
	c := fake.Capabilities()
	c.Share = false
	fake.SetCaps(c)
	fake.Seed("x.md", []byte("x\n"))
	if _, err := fsys.ReadDirPath(context.Background(), "/"); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(context.Background(), fsys, Request{Path: "/x.md", Confirm: true}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported: %v", err)
	}
}
