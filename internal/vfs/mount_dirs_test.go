package vfs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/meta"
	"cloudfs/test/fakeprovider"
)

// TestRootMountDoesNotSwallowPrefixMounts: a remote mounted at "/" lists
// the root completely and knows nothing of /raw. The prefix directories of
// the other mounts belong to the layout and must survive every listing —
// including the re-listing a mkdir at the root triggers.
func TestRootMountDoesNotSwallowPrefixMounts(t *testing.T) {
	dir := t.TempDir()
	// The meta clock runs an hour behind: the mount directories are then
	// older than the listing, which is the state of any daemon a second
	// after it started, and the "created after the listing began" rule
	// cannot mask what this test is about.
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{Now: func() time.Time { return time.Now().Add(-time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	root, raw := fakeprovider.New("root"), fakeprovider.New("raw")
	root.Seed("/top.txt", []byte("t"))
	raw.Seed("/inside.txt", []byte("i"))
	fsys, err := New(Options{Meta: store, Cache: ca, AttrTTL: time.Minute, DefaultDirTTL: time.Minute,
		Mounts: []Mount{
			{Prefix: "/", Remote: "root", RootID: fakeprovider.RootID, Provider: root, Mode: config.ModeWriteback},
			{Prefix: "/raw/b", Remote: "raw", RootID: fakeprovider.RootID, Provider: raw, Mode: config.ModeReadonly},
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	ctx := context.Background()
	names := func(p string) string {
		entries, err := fsys.ReadDirPath(ctx, p)
		if err != nil {
			t.Fatalf("readdir %s: %v", p, err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names("/"); got != "raw,top.txt" {
		t.Fatalf("first root listing = %s", got)
	}
	if _, err := fsys.Mkdir(ctx, meta.RootIno, "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.DropCaches(ctx); err != nil {
		t.Fatal(err)
	}
	if got := names("/"); got != "new,raw,top.txt" {
		t.Fatalf("root listing after a mkdir and a re-list = %s", got)
	}
	if got := names("/raw/b"); got != "inside.txt" {
		t.Fatalf("/raw/b = %s", got)
	}
}
