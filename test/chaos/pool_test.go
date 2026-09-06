package chaos

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/meta"
	"cloudfs/internal/pool"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

type poolRig struct {
	fs      *vfs.FS
	pool    *pool.Pool
	members map[string]*fakeprovider.Fake
}

func newPoolRig(t *testing.T, memberNames ...string) *poolRig {
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
	r := &poolRig{members: map[string]*fakeprovider.Fake{}}
	opt := pool.Options{Name: "home", StateDir: filepath.Join(dir, "pool"), Settings: config.Pool{Replicas: 2, MinReplicas: 1}}
	for _, n := range memberNames {
		f := fakeprovider.New(n)
		r.members[n] = f
		opt.Members = append(opt.Members, pool.Member{Name: n, Provider: f, Adopt: true})
	}
	p, err := pool.New(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	r.pool = p
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca,
		AttrTTL: time.Second, DefaultDirTTL: time.Second, NegativeTTL: time.Second,
		Mounts: []vfs.Mount{{Prefix: "/", Remote: "home", RootID: p.RootID(), Provider: p, Mode: config.ModeWriteback, DirTTL: time.Second}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	r.fs = fsys
	return r
}

func listNames(t *testing.T, fsys *vfs.FS, p string) string {
	t.Helper()
	entries, err := fsys.ReadDirPath(context.Background(), p)
	if err != nil {
		t.Fatalf("readdir %s: %v", p, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return strings.Join(names, ",")
}

// TestPoolListSurvivesOneMemberDown: "成员失联" in the reliability matrix. A
// member that stops answering never empties a directory: what other members
// hold is served, what only it held is still listed (and reads of it fail
// with a distinct, retryable error rather than vanishing).
func TestPoolListSurvivesOneMemberDown(t *testing.T) {
	r := newPoolRig(t, "a", "b")
	a, b := r.members["a"], r.members["b"]
	a.Seed("/on-a.txt", []byte("a"))
	b.Seed("/on-b.txt", []byte("b"))
	a.Seed("/both.txt", []byte("both"))
	b.Seed("/both.txt", []byte("both"))
	ctx := context.Background()

	if got := listNames(t, r.fs, "/"); got != "both.txt,on-a.txt,on-b.txt" {
		t.Fatalf("warm listing = %s", got)
	}
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	if _, err := r.fs.DropCaches(ctx); err != nil {
		t.Fatal(err)
	}
	if got := listNames(t, r.fs, "/"); got != "both.txt,on-a.txt,on-b.txt" {
		t.Fatalf("listing with a down = %s", got)
	}
	if data, err := r.fs.ReadFileRange(ctx, "/both.txt", 0, 0); err != nil || string(data) != "both" {
		t.Fatalf("file replicated on b while a is down = %q, %v", data, err)
	}
	if data, err := r.fs.ReadFileRange(ctx, "/on-b.txt", 0, 0); err != nil || string(data) != "b" {
		t.Fatalf("file on b while a is down = %q, %v", data, err)
	}
	_, err := r.fs.ReadFileRange(ctx, "/on-a.txt", 0, 0)
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("file only on the down member = %v, want ErrUnavailable", err)
	}
	// The member comes back; nothing needed restarting.
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })
	if data, err := r.fs.ReadFileRange(ctx, "/on-a.txt", 0, 0); err != nil || string(data) != "a" {
		t.Fatalf("after recovery = %q, %v", data, err)
	}
}
