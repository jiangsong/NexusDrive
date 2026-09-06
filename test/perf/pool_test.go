package perf

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/meta"
	"cloudfs/internal/pool"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

// poolHarness mounts a pool of fake members at / of a VFS, the way the
// daemon does, so the call-count baselines below measure the whole stack.
type poolHarness struct {
	fs      *vfs.FS
	members []*fakeprovider.Fake
}

func newPoolHarness(t *testing.T, memberNames ...string) *poolHarness {
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
	h := &poolHarness{}
	opt := pool.Options{Name: "home", StateDir: filepath.Join(dir, "pool"), Settings: config.Pool{Replicas: 2, MinReplicas: 1}}
	for _, n := range memberNames {
		f := fakeprovider.New(n)
		h.members = append(h.members, f)
		opt.Members = append(opt.Members, pool.Member{Name: n, Provider: f, Adopt: true})
	}
	p, err := pool.New(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca,
		AttrTTL: time.Hour, DefaultDirTTL: time.Hour, NegativeTTL: time.Minute,
		Mounts: []vfs.Mount{{Prefix: "/", Remote: "home", RootID: p.RootID(), Provider: p, Mode: config.ModeWriteback, DirTTL: time.Hour}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	h.fs = fsys
	return h
}

func (h *poolHarness) memberCalls() int {
	n := 0
	for _, m := range h.members {
		n += m.TotalCalls()
	}
	return n
}

// TestPoolWarmTraversalIsFree: the pool is one more backend to the VFS, so
// the headline claim holds for it too — after one traversal, walking the
// same fused tree again costs zero calls on any member.
func TestPoolWarmTraversalIsFree(t *testing.T) {
	h := newPoolHarness(t, "a", "b", "c")
	const dirs, perDir = 12, 10
	want := 0
	for d := 0; d < dirs; d++ {
		// Spread the tree: each directory lives on two of the three
		// members, files mirrored on both.
		m1, m2 := h.members[d%3], h.members[(d+1)%3]
		for i := 0; i < perDir; i++ {
			p := fmt.Sprintf("proj/dir%03d/file%03d.txt", d, i)
			m1.Seed(p, []byte("content"))
			m2.Seed(p, []byte("content"))
			want++
		}
	}
	cold := walk(t, h.fs, "/")
	if cold.files != want {
		t.Fatalf("cold walk saw %d files, want %d", cold.files, want)
	}
	coldCalls := h.memberCalls()
	if coldCalls == 0 {
		t.Fatal("a cold walk must talk to the members")
	}
	warm := walk(t, h.fs, "/")
	if warm.files != want {
		t.Fatalf("warm walk saw %d files, want %d", warm.files, want)
	}
	if extra := h.memberCalls() - coldCalls; extra != 0 {
		t.Fatalf("a warm walk made %d member calls, want 0", extra)
	}
	t.Logf("cold walk of %d files across 3 members cost %d member calls; warm walk cost 0", want, coldCalls)
}

// TestPoolColdDirCostsOneCallPerHoldingMember: listing a cold directory
// asks only the members that hold it, once each (per page). A member that
// does not have the directory is not consulted for its content.
func TestPoolColdDirCostsOneCallPerHoldingMember(t *testing.T) {
	h := newPoolHarness(t, "a", "b", "c")
	a, b, c := h.members[0], h.members[1], h.members[2]
	a.Seed("/shared/x.txt", []byte("x"))
	b.Seed("/shared/x.txt", []byte("x"))
	c.Seed("/elsewhere/y.txt", []byte("y"))
	ctx := context.Background()
	if _, err := h.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	before := [3]int{a.Calls("List"), b.Calls("List"), c.Calls("List")}
	if _, err := h.fs.ReadDirPath(ctx, "/shared"); err != nil {
		t.Fatal(err)
	}
	da, db, dc := a.Calls("List")-before[0], b.Calls("List")-before[1], c.Calls("List")-before[2]
	// One directory of one file is one page on the fake.
	if da != 1 || db != 1 {
		t.Fatalf("holders were asked a=%d b=%d times, want 1 each", da, db)
	}
	if dc != 0 {
		t.Fatalf("a member without the directory was asked %d times", dc)
	}
}
