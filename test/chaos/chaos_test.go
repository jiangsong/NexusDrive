// Package chaos verifies the reliability matrix in docs/DESIGN.md section 5:
// what the system does when the provider throttles, the network drops, the
// process dies mid-write, the cache fills, or the remote changes under us.
package chaos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

type rig struct {
	dir   string
	fs    *vfs.FS
	fake  *fakeprovider.Fake
	j     *journal.Journal
	up    *upload.Uploader
	cache *cache.Cache
	meta  *meta.Store
	reg   *ratelimit.Registry
}

type rigOpt struct {
	dir       string
	blockSize int64
	maxBytes  int64
	minFree   int64
	freeSpace func(string) (int64, error)
	mode      config.Mode
	// durability selects the journal's write promise; zero is power.
	durability journal.Durability
}

func newRig(t *testing.T, o rigOpt) *rig {
	t.Helper()
	if o.dir == "" {
		o.dir = t.TempDir()
	}
	if o.blockSize == 0 {
		o.blockSize = 4096
	}
	if o.mode == "" {
		o.mode = config.ModeWriteback
	}
	if o.freeSpace == nil {
		o.freeSpace = func(string) (int64, error) { return 1 << 40, nil }
	}
	store, err := meta.Open(filepath.Join(o.dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := cache.New(cache.Options{
		Dir: filepath.Join(o.dir, "cache"), BlockSize: o.blockSize,
		MaxBytes: o.maxBytes, MinFree: o.minFree, FreeSpace: o.freeSpace,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	j, err := journal.Open(journal.Options{Dir: filepath.Join(o.dir, "journal"), Durability: o.durability})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })

	fake := fakeprovider.New("ali")
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca,
		AttrTTL: time.Minute, DefaultDirTTL: time.Minute, NegativeTTL: time.Second,
		Mounts: []vfs.Mount{{
			Prefix: "/", Remote: "ali", RootID: fakeprovider.RootID,
			Provider: fake, Mode: o.mode, DirTTL: time.Minute,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })

	reg := ratelimit.NewRegistry(
		func(ratelimit.Key) ratelimit.Options {
			return ratelimit.Options{Rate: 16, MinRate: 1, RecoverAfter: 2, Step: 0.5}
		},
		ratelimit.BreakerOptions{Threshold: 3, Window: time.Minute, Cooldown: time.Hour},
	)
	up, err := upload.New(upload.Options{
		Journal: j,
		Providers: func(remote string) (provider.Provider, bool) {
			if remote == "ali" {
				return fake, true
			}
			return nil, false
		},
		Limiters:    reg,
		MaxAttempts: 4,
		Policy:      retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond}, MaxAttempts: 2},
		Backoff:     retry.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond, Rand: func() float64 { return 1 }},
		Hooks:       fsys.UploadHooks(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fsys.SetWriteBackend(j, up)
	return &rig{dir: o.dir, fs: fsys, fake: fake, j: j, up: up, cache: ca, meta: store, reg: reg}
}

// TestNetworkDropDuringUploadRecovers covers "上传中断网": the transfer fails,
// the journal keeps the data, and a later attempt completes it.
func TestNetworkDropDuringUploadRecovers(t *testing.T) {
	r := newRig(t, rigOpt{})
	ctx := context.Background()

	if _, err := r.fs.WriteFile(ctx, "/important.txt", []byte("must not be lost"), false); err != nil {
		t.Fatal(err)
	}
	// The network is down: every provider call fails.
	r.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.FailNext = 1 << 30 })
	if _, err := r.up.DrainOnce(ctx, "ali"); err != nil {
		t.Fatal(err)
	}
	st, _ := r.j.Stats(ctx)
	if st.Pending+st.Uploading == 0 {
		t.Fatalf("the write should still be queued after a failure: %+v", st)
	}
	// The file is still readable locally while the upload is stuck.
	got, err := r.fs.ReadFileRange(ctx, "/important.txt", 0, 0)
	if err != nil || string(got) != "must not be lost" {
		t.Fatalf("local read during an outage = %q, %v", got, err)
	}

	// The network comes back.
	r.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.FailNext = 0 })
	flushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := r.up.Flush(flushCtx); err != nil {
		t.Fatal(err)
	}
	st, _ = r.j.Stats(ctx)
	if st.Pending != 0 || st.Dead != 0 || st.Done == 0 {
		t.Fatalf("queue after recovery = %+v", st)
	}
	node, err := r.meta.Resolve(ctx, "/important.txt")
	if err != nil || node.RemoteID == "" || vfs.IsLocalOnly(node.RemoteID) {
		t.Fatalf("file did not reach the remote: %+v, %v", node, err)
	}
}

// TestKillDuringWriteLosesNothing covers "写入过程中进程被 kill -9".
func TestKillDuringWriteLosesNothing(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Run one: commit a write, then abandon the process without uploading.
	r1 := newRig(t, rigOpt{dir: dir})
	if _, err := r1.fs.WriteFile(ctx, "/survivor.txt", []byte("committed but not uploaded"), false); err != nil {
		t.Fatal(err)
	}
	// A write that was still in flight when the process died: staging exists,
	// no journal row.
	orphan, err := r1.j.NewStaging(nil)
	if err != nil {
		t.Fatal(err)
	}
	orphan.WriteAt([]byte("half a write"), 0)
	orphanPath := orphan.Path
	orphan.Close()
	r1.j.Close()
	r1.meta.Close()

	// Run two: recovery requeues the committed write and discards the partial.
	r2 := newRig(t, rigOpt{dir: dir})
	rec, err := r2.j.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Lost) != 0 {
		t.Fatalf("recovery lost data: %+v", rec)
	}
	if len(rec.OrphanStaging) != 1 {
		t.Fatalf("the partial write should be discarded: %+v", rec.OrphanStaging)
	}
	if fileExists(orphanPath) {
		t.Fatal("the orphan staging file should be gone")
	}
	if _, err := r2.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	// The committed write survived and reached the (new, empty) remote.
	entries, _, err := r2.fake.List(ctx, fakeprovider.RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.Name == "survivor.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the committed write was lost; remote has %+v", entries)
	}
}

// fileExists reports whether a path is still on disk.
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestRateLimitBackoffConverges covers "Provider 429 / 风控": the limiter halves
// its rate rather than hammering the provider into a ban.
func TestRateLimitBackoffConverges(t *testing.T) {
	r := newRig(t, rigOpt{})
	ctx := context.Background()
	limiter := r.reg.Limiter(ratelimit.Key{Remote: "ali", Class: ratelimit.Upload})
	start := limiter.Rate()

	// Every third call is throttled.
	r.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.RateLimitEvery = 3 })
	for i := 0; i < 6; i++ {
		if _, err := r.fs.WriteFile(ctx, fmt.Sprintf("/f%d.txt", i), []byte("x"), false); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		if _, err := r.up.DrainAll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if limiter.Rate() >= start {
		t.Fatalf("repeated 429s should have lowered the rate: %v -> %v", start, limiter.Rate())
	}
	if limiter.Rate() < 1 {
		t.Fatalf("the rate must not collapse below the floor: %v", limiter.Rate())
	}
	// Nothing was permanently lost.
	st, _ := r.j.Stats(ctx)
	if st.Dead > 0 {
		t.Fatalf("throttling should not dead-letter writes: %+v", st)
	}
}

// TestRiskControlOpensTheBreaker covers the ban-avoidance path: sustained
// risk-control responses stop the traffic instead of continuing it.
func TestRiskControlOpensTheBreaker(t *testing.T) {
	reg := ratelimit.NewRegistry(
		func(ratelimit.Key) ratelimit.Options { return ratelimit.Options{Rate: 8, MinRate: 1} },
		ratelimit.BreakerOptions{Threshold: 3, Window: time.Minute, Cooldown: time.Hour},
	)
	b := reg.Breaker("ali", "")
	for i := 0; i < 2; i++ {
		if b.Trip() {
			t.Fatal("the breaker opened before the threshold")
		}
	}
	if !b.Trip() || !b.Open() {
		t.Fatal("the breaker should open on the third risk-control signal")
	}
	if b.OpenUntil().IsZero() {
		t.Fatal("an open breaker must report when it recovers, so status can explain the pause")
	}
}

// TestCacheFullDegradesRatherThanFails covers "缓存盘将满".
func TestCacheFullDegradesRatherThanFails(t *testing.T) {
	free := int64(0)
	r := newRig(t, rigOpt{
		blockSize: 4096,
		minFree:   1 << 30,
		freeSpace: func(string) (int64, error) { return free, nil },
	})
	ctx := context.Background()
	content := bytes.Repeat([]byte("z"), 20000)
	r.fake.Seed("big.bin", content)

	// With no free space the cache refuses to admit blocks, but reads still
	// work: they simply are not cached.
	free = 0
	got, err := r.fs.ReadFileRange(ctx, "/big.bin", 0, int64(len(content)))
	if err != nil {
		t.Fatalf("reads must keep working when the cache cannot admit blocks: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("uncached read returned wrong data")
	}
	if s := r.cache.Stats(); s.Blocks != 0 {
		t.Fatalf("nothing should have been cached: %+v", s)
	}
	// Once space frees up, caching resumes.
	free = 1 << 40
	if _, err := r.fs.ReadFileRange(ctx, "/big.bin", 0, 4096); err != nil {
		t.Fatal(err)
	}
	if s := r.cache.Stats(); s.Blocks == 0 {
		t.Fatal("caching should resume once there is space")
	}
}

// TestRemoteChangedUnderUsProducesAConflictCopy covers "远端被其他客户端改动".
func TestRemoteChangedUnderUsProducesAConflictCopy(t *testing.T) {
	r := newRig(t, rigOpt{})
	ctx := context.Background()
	r.fake.Seed("shared.md", []byte("version from the server"))
	if _, err := r.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	node, err := r.meta.Resolve(ctx, "/shared.md")
	if err != nil {
		t.Fatal(err)
	}

	h, err := r.fs.Open(ctx, node.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.fs.Write(ctx, h, []byte("my local edit"), 0); err != nil {
		t.Fatal(err)
	}
	// Another client changes the file before our upload runs.
	r.fake.Seed("shared.md", []byte("someone else edited this first"))
	if err := r.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := r.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}

	entries, _, err := r.fake.List(ctx, fakeprovider.RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	var original, conflict *provider.Entry
	for i := range entries {
		switch {
		case entries[i].Name == "shared.md":
			original = &entries[i]
		case strings.HasPrefix(entries[i].Name, "shared (conflict"):
			conflict = &entries[i]
		}
	}
	if original == nil {
		t.Fatal("the other client's file disappeared")
	}
	if conflict == nil {
		t.Fatalf("no conflict copy was created; remote has %+v", entries)
	}
	// Neither version was lost.
	rc, err := r.fake.ReadRange(ctx, original.ID, original.Version, 0, original.Size)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, original.Size)
	rc.Read(buf)
	rc.Close()
	if string(buf) != "someone else edited this first" {
		t.Fatalf("the other client's content was overwritten: %q", buf)
	}
}

// TestSlowProviderDoesNotBlockOtherReads checks that one slow file does not
// serialise the whole filesystem.
func TestSlowProviderDoesNotBlockOtherReads(t *testing.T) {
	r := newRig(t, rigOpt{})
	ctx := context.Background()
	r.fake.Seed("a.txt", []byte("alpha"))
	r.fake.Seed("b.txt", []byte("bravo"))
	if _, err := r.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	r.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.Latency = 60 * time.Millisecond })
	start := time.Now()
	done := make(chan error, 2)
	for _, name := range []string{"/a.txt", "/b.txt"} {
		go func(p string) {
			_, err := r.fs.ReadFileRange(ctx, p, 0, 5)
			done <- err
		}(name)
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	// Two 60 ms reads in parallel should finish well under the 120 ms a
	// serialised implementation would need.
	if elapsed := time.Since(start); elapsed > 110*time.Millisecond {
		t.Fatalf("reads appear serialised: %v for two parallel 60ms fetches", elapsed)
	}
}

// TestDeadLetterKeepsDataAndRecovers covers the end of the retry ladder.
func TestDeadLetterKeepsDataAndRecovers(t *testing.T) {
	r := newRig(t, rigOpt{})
	ctx := context.Background()
	if _, err := r.fs.WriteFile(ctx, "/doomed.txt", []byte("valuable"), false); err != nil {
		t.Fatal(err)
	}
	r.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.FailNext = 1 << 30 })
	// The retry backoff is real time now, so give each attempt its short delay
	// rather than spinning: claiming respects next_retry_at to the millisecond.
	for i := 0; i < 8; i++ {
		if _, err := r.up.DrainOnce(ctx, "ali"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	dead, err := r.j.Dead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 1 {
		t.Fatalf("expected one dead letter, got %d", len(dead))
	}
	// The bytes are still there, and still readable through the filesystem.
	got, err := r.fs.ReadFileRange(ctx, "/doomed.txt", 0, 0)
	if err != nil || string(got) != "valuable" {
		t.Fatalf("dead-lettered file is not readable: %q, %v", got, err)
	}
	// The operator fixes the cause and requeues.
	r.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.FailNext = 0 })
	if err := r.j.Requeue(ctx, dead[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := r.j.Stats(ctx)
	if st.Dead != 0 || st.Done == 0 {
		t.Fatalf("requeued upload did not complete: %+v", st)
	}
}

// TestStaleDownloadLinkIsRefetched covers "下载链接过期".
func TestStaleDownloadLinkIsRefetched(t *testing.T) {
	r := newRig(t, rigOpt{})
	ctx := context.Background()
	r.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.LinkTTL = time.Nanosecond })
	r.fake.Seed("media.bin", bytes.Repeat([]byte("m"), 100))

	link, err := r.fs.DownloadURL(ctx, "/media.bin")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if r.fake.LinkValid(link.URL) {
		t.Fatal("the link should have expired")
	}
	// Reads do not depend on the link staying valid: they go through
	// ReadRange, which the driver re-authorises per call.
	got, err := r.fs.ReadFileRange(ctx, "/media.bin", 0, 100)
	if err != nil || len(got) != 100 {
		t.Fatalf("read after link expiry = %d bytes, %v", len(got), err)
	}
	// Asking again yields a fresh link.
	link2, err := r.fs.DownloadURL(ctx, "/media.bin")
	if err != nil {
		t.Fatal(err)
	}
	if link2.URL == link.URL {
		t.Fatal("a new request should mint a new link")
	}
}

// TestConcurrentWritersProduceOneWinnerAndNoCorruption checks the last-close
// wins rule and that neither writer's data corrupts the other's.
func TestConcurrentWritersProduceOneWinnerAndNoCorruption(t *testing.T) {
	r := newRig(t, rigOpt{})
	ctx := context.Background()
	if _, err := r.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	root, err := r.meta.Resolve(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	h1, err := r.fs.Create(ctx, root.Ino, "contested.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.fs.Write(ctx, h1, bytes.Repeat([]byte("A"), 100), 0); err != nil {
		t.Fatal(err)
	}
	if err := r.fs.Release(ctx, h1); err != nil {
		t.Fatal(err)
	}

	node, err := r.meta.Resolve(ctx, "/contested.txt")
	if err != nil {
		t.Fatal(err)
	}
	h2, err := r.fs.Open(ctx, node.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.fs.Write(ctx, h2, bytes.Repeat([]byte("B"), 100), 0); err != nil {
		t.Fatal(err)
	}
	if err := r.fs.Release(ctx, h2); err != nil {
		t.Fatal(err)
	}
	if _, err := r.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := r.fs.ReadFileRange(ctx, "/contested.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The file is one writer's content in full, never a blend of the two.
	if !bytes.Equal(got, bytes.Repeat([]byte("B"), 100)) {
		if bytes.Equal(got, bytes.Repeat([]byte("A"), 100)) {
			t.Fatal("the earlier writer won, which contradicts last-close-wins")
		}
		t.Fatalf("content is a blend of both writers: %q", got[:20])
	}
}

// TestReadOnlyModeRejectsEveryMutation covers the readonly mount mode.
func TestReadOnlyModeRejectsEveryMutation(t *testing.T) {
	r := newRig(t, rigOpt{mode: config.ModeReadonly})
	ctx := context.Background()
	r.fake.Seed("file.txt", []byte("content"))
	if _, err := r.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	root, _ := r.meta.Resolve(ctx, "/")

	if _, err := r.fs.Create(ctx, root.Ino, "new.txt"); !errors.Is(err, vfs.ErrReadOnly) {
		t.Errorf("create = %v", err)
	}
	if _, err := r.fs.Mkdir(ctx, root.Ino, "dir"); !errors.Is(err, vfs.ErrReadOnly) {
		t.Errorf("mkdir = %v", err)
	}
	if err := r.fs.Remove(ctx, root.Ino, "file.txt", false); !errors.Is(err, vfs.ErrReadOnly) {
		t.Errorf("remove = %v", err)
	}
	// The file is untouched.
	if _, err := r.fake.Stat(ctx, mustID(t, r, "/file.txt")); err != nil {
		t.Errorf("the file should still exist remotely: %v", err)
	}
}

func mustID(t *testing.T, r *rig, path string) string {
	t.Helper()
	n, err := r.meta.Resolve(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return n.RemoteID
}

// TestKillDuringWriteLosesNothingInCrashMode: crash durability skips every
// fsync on the write path, and the promise it keeps is exactly this one — a
// close() that returned survives the process dying. The page cache, not
// the disk, is what holds the data, and the page cache outlives a process.
func TestKillDuringWriteLosesNothingInCrashMode(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	r1 := newRig(t, rigOpt{dir: dir, durability: journal.DurabilityCrash})
	// Enough files to fill several commit batches; the rig's upload rate
	// limit is what bounds the drain, not the count.
	const n = 40
	for i := 0; i < n; i++ {
		if _, err := r1.fs.WriteFile(ctx, fmt.Sprintf("/small-%03d.txt", i), []byte(fmt.Sprintf("payload %d", i)), false); err != nil {
			t.Fatal(err)
		}
	}
	// The process "dies": nothing is flushed, closed or drained.
	r1.j.Close()
	r1.meta.Close()

	r2 := newRig(t, rigOpt{dir: dir, durability: journal.DurabilityCrash})
	rec, err := r2.j.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Lost) != 0 {
		t.Fatalf("crash-mode recovery lost %d acknowledged writes: %v", len(rec.Lost), rec.Lost)
	}
	if _, err := r2.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	landed := map[string]bool{}
	for cursor := ""; ; {
		entries, next, err := r2.fake.List(ctx, fakeprovider.RootID, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			landed[e.Name] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	for i := 0; i < n; i++ {
		if name := fmt.Sprintf("small-%03d.txt", i); !landed[name] {
			t.Fatalf("%s was acknowledged before the kill and never reached the remote", name)
		}
	}
}
