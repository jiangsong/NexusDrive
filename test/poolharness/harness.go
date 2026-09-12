// Package poolharness builds a VFS over a pool of fake members, assembled
// the way internal/daemon assembles the real thing. Tests that measure what
// the whole stack costs — provider call counts, bytes read, what a warm tree
// asks a backend for — need a stack, not a mock, and every one of them needs
// the same one.
package poolharness

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/pool"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

// Harness is one assembled stack. Every component is exposed because the
// assertions are about how they interact: what the VFS asked the pool for,
// what the pool asked its members for.
type Harness struct {
	Dir      string
	FS       *vfs.FS
	Members  []*fakeprovider.Fake
	Uploader *upload.Uploader
	Pool     *pool.Pool
	Cache    *cache.Cache
	Meta     *meta.Store
	Journal  *journal.Journal
}

// Options configures Open.
type Options struct {
	// Members are the member names, one fake backend each.
	Members []string
	// Settings is the pool's replica policy. The zero value is two replicas
	// with a minimum of one.
	Settings config.Pool
	// Capacity is each member's total space; 0 means unlimited.
	Capacity int64
	// BlockSize is the block cache's block size (0 = 4 KiB, small enough
	// that a test file spans several blocks).
	BlockSize int64
}

// New builds a two-replica pool over the named members.
func New(t *testing.T, memberNames ...string) *Harness {
	t.Helper()
	return Open(t, Options{Members: memberNames, Settings: config.Pool{Replicas: 2, MinReplicas: 1}})
}

// NewWith is New with the pool settings and a per-member capacity spelled
// out, for tests that need a pool whose members can be full.
func NewWith(t *testing.T, settings config.Pool, capacity int64, memberNames ...string) *Harness {
	t.Helper()
	return Open(t, Options{Members: memberNames, Settings: settings, Capacity: capacity})
}

// Open assembles the stack and registers its teardown with t.
func Open(t *testing.T, opt Options) *Harness {
	t.Helper()
	if opt.Settings.Replicas == 0 {
		opt.Settings.Replicas, opt.Settings.MinReplicas = 2, 1
	}
	if opt.BlockSize == 0 {
		opt.BlockSize = 4096
	}
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: opt.BlockSize})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	h := &Harness{Dir: dir, Meta: store, Cache: ca}
	po := pool.Options{Name: "home", StateDir: filepath.Join(dir, "pool"), Settings: opt.Settings}
	for i, n := range opt.Members {
		f := fakeprovider.New(n)
		h.Members = append(h.Members, f)
		po.Members = append(po.Members, pool.Member{Name: n, Provider: f, Adopt: true, Capacity: opt.Capacity, Domain: fmt.Sprintf("acct-%d", i)})
	}
	p, err := pool.New(po)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	h.Pool = p
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca,
		AttrTTL: time.Hour, DefaultDirTTL: time.Hour, NegativeTTL: time.Minute,
		Mounts: []vfs.Mount{{Prefix: "/", Remote: "home", RootID: p.RootID(), Provider: p, Mode: config.ModeWriteback, DirTTL: time.Hour}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	h.FS = fsys
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	h.Journal = j
	up, err := upload.New(upload.Options{
		Journal:   j,
		Providers: func(string) (provider.Provider, bool) { return p, true },
		Policy:    retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: time.Millisecond}, MaxAttempts: 2},
		Hooks:     fsys.UploadHooks(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fsys.SetWriteBackend(j, up)
	h.Uploader = up
	return h
}

// MemberCalls is the total number of provider calls every member has served.
func (h *Harness) MemberCalls() int {
	n := 0
	for _, m := range h.Members {
		n += m.TotalCalls()
	}
	return n
}

// Calls is the number of times every member served one operation.
func (h *Harness) Calls(op string) int {
	n := 0
	for _, m := range h.Members {
		n += m.Calls(op)
	}
	return n
}

// ReadBytes is how many bytes of file content the members have served.
func (h *Harness) ReadBytes() int64 {
	var n int64
	for _, m := range h.Members {
		n += m.ReadBytes()
	}
	return n
}
