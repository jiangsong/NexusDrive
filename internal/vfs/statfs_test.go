package vfs

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/meta"
	"cloudfs/test/fakeprovider"
)

// TestSpaceReportsBackendQuota: df on a mount shows the backends' space
// when they can report it, summed over distinct remotes, and nothing when
// none can.
func TestSpaceReportsBackendQuota(t *testing.T) {
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	a, b, quiet := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("quiet")
	a.SetQuota(1000, 300)
	b.SetQuota(500, 100)
	fsys, err := New(Options{Meta: store, Cache: ca, AttrTTL: time.Minute, DefaultDirTTL: time.Minute,
		Mounts: []Mount{
			{Prefix: "/a", Remote: "a", RootID: fakeprovider.RootID, Provider: a, Mode: config.ModeWriteback},
			{Prefix: "/a2", Remote: "a", RootID: fakeprovider.RootID, Provider: a, Mode: config.ModeReadonly},
			{Prefix: "/b", Remote: "b", RootID: fakeprovider.RootID, Provider: b, Mode: config.ModeWriteback},
			{Prefix: "/q", Remote: "quiet", RootID: fakeprovider.RootID, Provider: quiet, Mode: config.ModeWriteback},
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	sp := fsys.Space(context.Background())
	if !sp.Known || sp.Total != 1500 || sp.Used != 400 || sp.Free() != 1100 {
		t.Fatalf("space = %+v", sp)
	}
	// Two prefixes of one remote count it once; a remote that cannot say
	// adds nothing. And df is cheap: the answer is cached.
	if a.Calls("Quota") != 1 {
		t.Fatalf("a was asked %d times", a.Calls("Quota"))
	}
	fsys.Space(context.Background())
	if a.Calls("Quota") != 1 {
		t.Fatal("the space is not cached")
	}

	only, _ := New(Options{Meta: store, Cache: ca, Mounts: []Mount{{Prefix: "/", Remote: "quiet", RootID: fakeprovider.RootID, Provider: quiet}}})
	defer only.Close()
	if sp := only.Space(context.Background()); sp.Known {
		t.Fatalf("a mount of a silent backend claims %+v", sp)
	}
}
