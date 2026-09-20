//go:build diag

// This file is a diagnostic, not a regression test. It is behind the `diag`
// build tag because it measures wall clock, which the rest of test/perf
// deliberately never does — the suite asserts provider call counts so that it
// stays meaningful on any machine. Run it by hand when deciding whether a
// durability change is worth its risk:
//
//	./gow test -tags diag ./test/perf/ -run TestDurabilityCostDiag -v
//
// What it answers: how much of close(2) is device flushes, per level.
//
// Only F_FULLFSYNC — the call that makes the drive empty its write cache —
// costs anything here. On this project's development machine it is 4.07 ms
// against 74 us for an ordinary fsync, so the three levels are separated
// almost entirely by how many of them they spend per file:
//
//	power    2 flushes, both before the row insert   9.51 ms/file
//	barrier  1 flush, after the row insert           5.65 ms/file
//	crash    none                                    1.26 ms/file
//
// The SQLite transactions on that path (the synchronous=FULL row insert,
// meta.PublishByIno, MarkPublished) add about 70 us between them, because
// SQLite issues a plain fsync unless PRAGMA fullfsync is set and nothing in
// this tree sets it — which is also why batching the publication side was
// measured, costed at 0.7%, and abandoned. barrier-minus-crash is 4.40 ms:
// one flush plus change, which is the shape the level was built to have.
package perf

import (
	"context"
	"fmt"
	"path/filepath"
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

// diagRig is a real-clock filesystem: the shared harness runs on a fake clock,
// which is right for counting calls and useless for timing them.
type diagRig struct {
	fs *vfs.FS
}

func newDiagRig(t *testing.T, durability journal.Durability) *diagRig {
	t.Helper()
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal"), Durability: durability})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	fake := fakeprovider.New("ali")
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca,
		AttrTTL: time.Hour, DefaultDirTTL: time.Hour, NegativeTTL: time.Minute,
		Mounts: []vfs.Mount{{
			Prefix: "/", Remote: "ali", RootID: fakeprovider.RootID,
			Provider: fake, Mode: config.ModeWriteback, DirTTL: time.Hour,
		}},
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
	return &diagRig{fs: fsys}
}

// createSerially writes n small files one at a time, the way cp(1) does. The
// journal's group commit only opens its window when two commits are already in
// flight (internal/journal/group.go), so a single-threaded writer never
// benefits from it — which is exactly the case this measures.
func (r *diagRig) createSerially(t *testing.T, n int, payload []byte) time.Duration {
	t.Helper()
	ctx := context.Background()
	root, err := r.fs.Meta().Resolve(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < n; i++ {
		fh, err := r.fs.Create(ctx, root.Ino, fmt.Sprintf("f%05d.txt", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.fs.Write(ctx, fh, payload, 0); err != nil {
			t.Fatal(err)
		}
		if err := r.fs.Sync(ctx, fh); err != nil {
			t.Fatal(err)
		}
		if err := r.fs.Release(ctx, fh); err != nil {
			t.Fatal(err)
		}
	}
	return time.Since(start)
}

func TestDurabilityCostDiag(t *testing.T) {
	const files = 300
	payload := make([]byte, 10<<10)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	var results []struct {
		mode  journal.Durability
		total time.Duration
	}
	for _, mode := range []journal.Durability{journal.DurabilityPower, journal.DurabilityBarrier, journal.DurabilityCrash} {
		r := newDiagRig(t, mode)
		total := r.createSerially(t, files, payload)
		results = append(results, struct {
			mode  journal.Durability
			total time.Duration
		}{mode, total})
		t.Logf("%-6s %d files: %v total, %v per file",
			mode, files, total.Round(time.Millisecond), (total / files).Round(time.Microsecond))
	}
	power, barrier, crash := results[0].total, results[1].total, results[2].total
	t.Logf("power/crash %.2fx, power/barrier %.2fx, barrier/crash %.2fx",
		float64(power)/float64(crash), float64(power)/float64(barrier), float64(barrier)/float64(crash))
	t.Logf("per-file: power->barrier saves %v; barrier->crash still costs %v",
		((power - barrier) / files).Round(time.Microsecond), ((barrier - crash) / files).Round(time.Microsecond))
	t.Log("A small gap means the per-file flushes are NOT the dominant cost and " +
		"batching the publication side cannot pay for its risk.")
}
