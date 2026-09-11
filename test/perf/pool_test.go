package perf

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sync"
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

// poolHarness mounts a pool of fake members at / of a VFS, the way the
// daemon does, so the call-count baselines below measure the whole stack.
type poolHarness struct {
	fs      *vfs.FS
	members []*fakeprovider.Fake
	up      *upload.Uploader
	pool    *pool.Pool
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
	h.pool = p
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
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
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
	h.up = up
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

// TestPoolWriteThenReadIsLocal: what this machine wrote it reads from its
// own cache, before and after the upload, without asking any member.
func TestPoolWriteThenReadIsLocal(t *testing.T) {
	h := newPoolHarness(t, "a", "b")
	ctx := context.Background()
	if _, err := h.fs.WriteFile(ctx, "/local.txt", []byte("mine"), false); err != nil {
		t.Fatal(err)
	}
	before := h.memberCalls()
	if data, err := h.fs.ReadFileRange(ctx, "/local.txt", 0, 0); err != nil || string(data) != "mine" {
		t.Fatalf("read before upload = %q, %v", data, err)
	}
	if extra := h.memberCalls() - before; extra != 0 {
		t.Fatalf("reading a just-written file cost %d member calls", extra)
	}
	if _, err := h.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	after := h.memberCalls()
	if data, err := h.fs.ReadFileRange(ctx, "/local.txt", 0, 0); err != nil || string(data) != "mine" {
		t.Fatalf("read after upload = %q, %v", data, err)
	}
	if extra := h.memberCalls() - after; extra != 0 {
		t.Fatalf("reading an uploaded file cost %d member calls", extra)
	}
}

// TestPoolDedupUploadCostsNoParts: bytes a member already holds go up by
// hash. The second write of the same content sends no part anywhere.
func TestPoolDedupUploadCostsNoParts(t *testing.T) {
	h := newPoolHarness(t, "a", "b")
	ctx := context.Background()
	content := []byte("the same bytes twice")
	if _, err := h.fs.WriteFile(ctx, "/one.bin", content, false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	parts := 0
	for _, m := range h.members {
		parts += m.Calls("UploadPart")
	}
	if parts == 0 {
		t.Fatal("the first upload should have sent parts")
	}
	if _, err := h.fs.WriteFile(ctx, "/two.bin", content, false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	after := 0
	for _, m := range h.members {
		after += m.Calls("UploadPart")
	}
	if after != parts {
		t.Fatalf("the second upload of the same bytes sent %d parts", after-parts)
	}
	if data, err := h.fs.ReadFileRange(ctx, "/two.bin", 0, 0); err != nil || string(data) != string(content) {
		t.Fatalf("dedup read = %q, %v", data, err)
	}
}

// TestRepairCostIsOneUploadPerReplica: making the missing replicas of N
// files costs N uploads on the receiving member and no listing anywhere.
// Repair reads the index, not the drives.
func TestRepairCostIsOneUploadPerReplica(t *testing.T) {
	h := newPoolHarness(t, "a", "b")
	ctx := context.Background()
	const files = 8
	for i := 0; i < files; i++ {
		if _, err := h.fs.WriteFile(ctx, fmt.Sprintf("/f%02d.txt", i), []byte(fmt.Sprintf("content %d", i)), false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	listsBefore := h.members[0].Calls("List") + h.members[1].Calls("List")
	uploadsBefore := h.members[0].Calls("BeginUpload") + h.members[1].Calls("BeginUpload")
	made := 0
	for i := 0; i < 4 && made < files; i++ {
		n, err := h.pool.RepairOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		made += n
	}
	if made != files {
		t.Fatalf("repair made %d replicas, want %d", made, files)
	}
	if extra := h.members[0].Calls("List") + h.members[1].Calls("List") - listsBefore; extra != 0 {
		t.Fatalf("repair listed directories %d times", extra)
	}
	if extra := h.members[0].Calls("BeginUpload") + h.members[1].Calls("BeginUpload") - uploadsBefore; extra != files {
		t.Fatalf("repair began %d uploads for %d files", extra, files)
	}
}

// TestPoolReadFanoutAddsBandwidth: a 3-replica file reads at close to three
// drives' speed. Each member serves at most two requests at a time (its
// connection budget, which is what caps a real drive's throughput), and a
// reader issues eight block requests at once the way VFS readahead does.
// One member serves 12 blocks two at a time; three members serve them six at
// a time. This is the one perf test that asserts a wall-clock ratio: call
// counts cannot show bandwidth. The pool is driven directly so the VFS
// window policy does not blur what the replica picker adds.
func TestPoolReadFanoutAddsBandwidth(t *testing.T) {
	const (
		blockSize = 4 << 20
		blocks    = 12 // 48 MiB
		window    = 8
		latency   = 20 * time.Millisecond
		conns     = 2
		// The design target is 0.5 (docs/pool-v2.md §4.5) and the expected
		// ratio is about 1/3; 0.6 leaves room for a loaded CI machine
		// without letting "no fan-out at all" (≈ 1.0) pass.
		maxRatio = 0.6
	)
	data := make([]byte, 0, blocks*blockSize)
	for i := 0; i < blocks; i++ {
		data = append(data, bytes.Repeat([]byte{byte('A' + i)}, blockSize)...)
	}
	timed := func(members int) time.Duration {
		opt := pool.Options{Name: "home", StateDir: t.TempDir(), Settings: config.Pool{Replicas: members, MinReplicas: 1}}
		for i := 0; i < members; i++ {
			f := fakeprovider.New(fmt.Sprintf("m%d", i))
			f.Seed("/movie.mkv", data)
			caps := f.Capabilities()
			caps.MaxConnsPerHost = conns
			f.SetCaps(caps)
			opt.Members = append(opt.Members, pool.Member{Name: f.Name(), Provider: f, Adopt: true})
			f.SetFaults(func(ft *fakeprovider.Faults) { ft.Latency = latency; ft.MaxConcurrent = conns })
		}
		p, err := pool.New(opt)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		ctx := context.Background()
		entries, _, err := p.List(ctx, p.RootID(), "")
		if err != nil || len(entries) != 1 {
			t.Fatalf("list = %v, %v", entries, err)
		}
		e := entries[0]
		best := time.Duration(0)
		for round := 0; round < 2; round++ {
			next := make(chan int, blocks)
			for i := 0; i < blocks; i++ {
				next <- i
			}
			close(next)
			var wg sync.WaitGroup
			var mu sync.Mutex
			var failed error
			fail := func(err error) {
				mu.Lock()
				if failed == nil {
					failed = err
				}
				mu.Unlock()
			}
			start := time.Now()
			for w := 0; w < window; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					// A reused buffer keeps copying out of the measurement:
					// what is timed is the members, not allocation.
					got := make([]byte, blockSize)
					for i := range next {
						off := int64(i) * blockSize
						rc, err := p.ReadRange(ctx, e.ID, e.Version, off, blockSize)
						if err != nil {
							fail(err)
							continue
						}
						_, err = io.ReadFull(rc, got)
						rc.Close()
						if err != nil || !bytes.Equal(got, data[off:off+blockSize]) {
							fail(fmt.Errorf("block %d: %v", i, err))
						}
					}
				}()
			}
			wg.Wait()
			took := time.Since(start)
			if failed != nil {
				t.Fatalf("%d members: %v", members, failed)
			}
			if best == 0 || took < best {
				best = took
			}
		}
		return best
	}
	single := timed(1)
	fused := timed(3)
	ratio := float64(fused) / float64(single)
	t.Logf("48 MiB in 12 blocks: 1 member %v, 3 members %v, ratio %.2f", single, fused, ratio)
	if ratio >= maxRatio {
		t.Fatalf("3 replicas read in %.2f× the single-member time, want < %.2f", ratio, maxRatio)
	}
}
