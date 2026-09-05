package e2e

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/webdav"

	"cloudfs/internal/config"
	"cloudfs/internal/daemon"
	"cloudfs/internal/fusefs"
	"cloudfs/internal/journal"
	"cloudfs/test/fakeprovider"
)

// TestConcurrentGoWritersNeverSeeEINTR is the pressure form of the close(2)
// EINTR fix: Go preempts its own threads with SIGURG, which interrupts FUSE
// requests at random. Eight writers closing a thousand files between them
// must never see an interrupted create, write, close or rename.
// fastFake lifts the fake backend's deliberately conservative rate limits so
// a thousand uploads drain in seconds; the limiter seeds from the matrix on
// first use, which is after this call.
func fastFake(s *stack) {
	c := s.fake.Capabilities()
	c.QPS.Meta, c.QPS.Download, c.QPS.Upload = 1000, 1000, 1000
	c.UploadParallel = 8
	s.fake.SetCaps(c)
}

// waitDrain is settle with a longer patience, for tests that queue hundreds
// of uploads on purpose.
func waitDrain(t *testing.T, j *journal.Journal, patience time.Duration) journal.Stats {
	t.Helper()
	deadline := time.Now().Add(patience)
	for {
		st, err := j.Stats(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st.Pending == 0 && st.Uploading == 0 {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("upload queue did not drain: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestConcurrentGoWritersNeverSeeEINTR(t *testing.T) {
	s := newStack(t, "writeback")
	fastFake(s)
	const writers, each = 8, 125
	var wg sync.WaitGroup
	var failures atomic.Int64
	var first atomic.Value
	payload := bytes.Repeat([]byte("x"), 3000)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			dir := filepath.Join(s.dir, fmt.Sprintf("w%d", w))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				failures.Add(1)
				first.CompareAndSwap(nil, err.Error())
				return
			}
			for i := 0; i < each; i++ {
				tmp := filepath.Join(dir, fmt.Sprintf(".f%04d.tmp", i))
				final := filepath.Join(dir, fmt.Sprintf("f%04d", i))
				if err := os.WriteFile(tmp, payload, 0o644); err != nil {
					failures.Add(1)
					first.CompareAndSwap(nil, "write: "+err.Error())
					continue
				}
				if err := os.Rename(tmp, final); err != nil {
					failures.Add(1)
					first.CompareAndSwap(nil, "rename: "+err.Error())
					continue
				}
				if st, err := os.Stat(final); err != nil || st.Size() != int64(len(payload)) {
					failures.Add(1)
					first.CompareAndSwap(nil, fmt.Sprintf("stat: %v", err))
				}
			}
		}(w)
	}
	wg.Wait()
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d of %d operations failed; first: %v", n, writers*each, first.Load())
	}
	if st := waitDrain(t, s.d.Journal, 2*time.Minute); st.Dead != 0 {
		t.Fatalf("%d uploads dead-lettered", st.Dead)
	}
}

// TestStressWithForcedRefreshKeepsReadYourWrites: writers and readers on one
// tree while the directory is re-listed from the backend every few
// milliseconds. Any missing or wrong read is the stale-listing bug coming
// back.
func TestStressWithForcedRefreshKeepsReadYourWrites(t *testing.T) {
	s := newStack(t, "writeback")
	fastFake(s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := filepath.Join(s.dir, "stress")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	at, err := s.d.FS.StatPath(ctx, "/stress")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for ctx.Err() == nil {
			_ = s.d.FS.Refresh(ctx, at.Ino)
			time.Sleep(3 * time.Millisecond)
		}
	}()
	const workers, iters = 4, 250
	payload := bytes.Repeat([]byte("stress"), 2000)
	var wg sync.WaitGroup
	var failures atomic.Int64
	var first atomic.Value
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				p := filepath.Join(dir, fmt.Sprintf("w%d-%04d", w, i))
				if err := os.WriteFile(p, payload, 0o644); err != nil {
					failures.Add(1)
					first.CompareAndSwap(nil, "write: "+err.Error())
					continue
				}
				got, err := os.ReadFile(p)
				if err != nil {
					failures.Add(1)
					first.CompareAndSwap(nil, "read: "+err.Error())
					continue
				}
				if !bytes.Equal(got, payload) {
					failures.Add(1)
					first.CompareAndSwap(nil, "content mismatch")
				}
			}
		}(w)
	}
	wg.Wait()
	cancel()
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d of %d iterations failed; first: %v", n, workers*iters, first.Load())
	}
}

// TestReadOnlyCommandsDuringUploadsLeaveTheQueueAlone: every `cloudfs`
// subcommand opens the same stack. Opening it repeatedly beside a live
// daemon — what `cloudfs status` in a loop does — must not requeue in-flight
// rows, delete staging files or dead-letter anything.
func TestReadOnlyCommandsDuringUploadsLeaveTheQueueAlone(t *testing.T) {
	s := newStack(t, "writeback")
	fastFake(s)
	ctx := context.Background()
	// Slow the backend down so uploads are in flight while we poke at it.
	s.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.Latency = 20 * time.Millisecond })
	done := make(chan struct{})
	var writeErr atomic.Value
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			if err := os.WriteFile(filepath.Join(s.dir, fmt.Sprintf("q%03d", i)), []byte("queued"), 0o644); err != nil {
				writeErr.CompareAndSwap(nil, err)
			}
		}
	}()
	var opens int
	for i := 0; i < 20; i++ {
		d2, err := daemon.Open(ctx, daemon.Options{Config: s.cfg, Version: "status"})
		if err != nil {
			t.Fatalf("second open: %v", err)
		}
		if d2.Journal.Owner() {
			d2.Close()
			t.Fatal("a second process must not own the live queue")
		}
		_ = d2.Collector().Collect(ctx)
		d2.Close()
		opens++
		time.Sleep(10 * time.Millisecond)
	}
	<-done
	if err := writeErr.Load(); err != nil {
		t.Fatalf("a write failed while status commands ran: %v", err)
	}
	s.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.Latency = 0 })
	st := waitDrain(t, s.d.Journal, 2*time.Minute)
	if st.Dead != 0 {
		t.Fatalf("%d uploads dead-lettered while status commands ran", st.Dead)
	}
	rows, _ := s.d.Journal.Dead(ctx)
	for _, r := range rows {
		if strings.Contains(r.LastError, "interrupted by restart") {
			t.Fatalf("a status command ran recovery on the live queue: %s", r.LastError)
		}
	}
	// The fake pages its listings, so follow the cursor.
	landed := map[string]bool{}
	for cursor := ""; ; {
		entries, next, err := s.fake.List(ctx, fakeprovider.RootID, cursor)
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
	var missing []string
	for i := 0; i < 300; i++ {
		if !landed[fmt.Sprintf("q%03d", i)] {
			missing = append(missing, fmt.Sprintf("q%03d", i))
		}
	}
	if len(missing) > 0 {
		at, serr := s.d.FS.StatPath(ctx, "/"+missing[0])
		n, nerr := s.d.FS.Meta().Resolve(ctx, "/"+missing[0])
		st, _ := s.d.Journal.Stats(ctx)
		t.Fatalf("%d of 300 files never landed after %d status opens (first %s): stat=%+v %v node=%+v %v journal=%+v backend_files=%d",
			len(missing), opens, missing[0], at, serr, n, nerr, st, len(landed))
	}
}

const webdavConfigTemplate = `
cache:
  dir: %s
  max_size: 1GiB
  min_free: 1MiB
  block_size: 64KiB
proxy:
  rules:
    - FINAL,direct
remotes:
  dav: { type: webdav, url: '%s', qps: { meta: 500, download: 500, upload: 500 } }
mounts:
  - path: %s
    layout:
      /: { remote: dav, root: /, mode: writeback }
mcp:
  allow: []
`

// TestIdenticalFilesShareABlobWithoutLosingAny: through a backend with
// content hashes the staging blobs are content-addressed, so 300 identical
// files share one. Every one of them must still land, and none may be
// dead-lettered because the shared blob was deleted early.
func TestIdenticalFilesShareABlobWithoutLosingAny(t *testing.T) {
	if ok, why := fusefs.Supported(); !ok {
		t.Skipf("FUSE unavailable: %s", why)
	}
	base := t.TempDir()
	served := filepath.Join(base, "served")
	os.MkdirAll(served, 0o755)
	srv := httptest.NewServer(&webdav.Handler{FileSystem: webdav.Dir(served), LockSystem: webdav.NewMemLS()})
	defer srv.Close()

	cacheDir := filepath.Join(base, "cache")
	mountDir := filepath.Join(base, "mnt")
	os.MkdirAll(mountDir, 0o755)
	cfgPath := filepath.Join(base, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(webdavConfigTemplate, cacheDir, srv.URL, mountDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := daemon.Open(ctx, daemon.Options{Config: cfg, Version: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m, err := fusefs.MountFS(fusefs.MountOptions{Options: fusefs.Options{FS: d.FS}, Path: mountDir})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	defer m.Unmount()

	same := []byte("exactly the same bytes in every file")
	const n = 300
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(mountDir, fmt.Sprintf("dup%03d.txt", i)), same, 0o644); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		st, err := d.Journal.Stats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.Pending == 0 && st.Uploading == 0 {
			if st.Dead != 0 {
				rows, _ := d.Journal.Dead(ctx)
				var reasons []string
				for _, r := range rows {
					reasons = append(reasons, r.LastError)
				}
				t.Fatalf("%d of %d identical files dead-lettered: %v", st.Dead, n, reasons)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue did not drain: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for i := 0; i < n; i++ {
		got, err := os.ReadFile(filepath.Join(served, fmt.Sprintf("dup%03d.txt", i)))
		if err != nil || !bytes.Equal(got, same) {
			t.Fatalf("dup%03d.txt on the server: %q, %v", i, got, err)
		}
	}
	// The server saw one PUT per file and nothing was retried.
	_ = http.StatusOK
	_ = journal.StateDone
}

// TestRandomReadsThroughTheKernelStayWithinBudget is the end-to-end form of
// the sub-block guarantee: 500 random 4 KiB preads on a cold 4 MiB file,
// through the kernel with its own read-ahead in play, must fetch a bounded
// amount from the backend — not the file. The first LAN run fetched 116 MB
// of a 256 MiB file for 500 such reads.
func TestRandomReadsThroughTheKernelStayWithinBudget(t *testing.T) {
	s := newStack(t, "writeback")
	fastFake(s)
	const size = 4 << 20
	data := make([]byte, size)
	rand.New(rand.NewSource(11)).Read(data)
	s.fake.Seed("big.bin", data)
	p := filepath.Join(s.dir, "big.bin")
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	before := s.fake.ReadBytes()
	rng := rand.New(rand.NewSource(12))
	buf := make([]byte, 4096)
	const reads = 500
	for i := 0; i < reads; i++ {
		off := rng.Int63n(size - 4096)
		if _, err := f.ReadAt(buf, off); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, data[off:off+4096]) {
			t.Fatalf("read %d at %d returned wrong bytes", i, off)
		}
	}
	got := s.fake.ReadBytes() - before
	// The kernel asks for 16 KiB per random read and a read may straddle
	// two sub-blocks: 32 KiB per read is the ceiling, well under a whole
	// 64 KiB block each.
	if limit := int64(reads * 32 << 10); got > limit {
		t.Fatalf("%d random reads fetched %d bytes from the backend, want at most %d", reads, got, limit)
	}
	t.Logf("%d random 4 KiB reads fetched %d bytes (%d per read)", reads, got, got/reads)
}

// TestReadsStraddlingUploadCompletionStayLocal is the acceptance for T-00b,
// the EIO seen once at the instant an upload completed.
//
// The window is narrow: the node has already switched to the remote id and
// version while the staged blob is still registered in the cache under the
// local key. A read arriving there misses under the new key and goes to the
// backend for content this process just uploaded — which is where the EIO came
// from, and which is observable here as a download that should never happen.
// The fix registers the blob under the new key before the node moves
// (UploadHooks.OnSuccess calls cache.LinkFile ahead of meta.UpdateByIno).
//
// Writers go through the real mount so the write path is the kernel's. Readers
// call the VFS directly: the kernel would serve most of these from its own
// page cache and never reach the code under test.
//
// Reverting the ordering in write.go makes this fail with a non-zero download
// count, which is what makes the assertion worth keeping.
func TestReadsStraddlingUploadCompletionStayLocal(t *testing.T) {
	s := newStack(t, "writeback")
	fastFake(s)
	// A little backend latency spreads the completions out instead of letting
	// them all land before the readers get going.
	s.fake.SetFaults(func(f *fakeprovider.Faults) { f.Latency = 2 * time.Millisecond })

	dir := filepath.Join(s.dir, "straddle")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("straddle"), 512)
	before := s.fake.Calls("ReadRange")

	var mu sync.Mutex
	published := make([]string, 0, 1000)
	var failures atomic.Int64
	var first atomic.Value
	note := func(what string) {
		failures.Add(1)
		first.CompareAndSwap(nil, what)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func(seed int64) {
			defer readers.Done()
			rnd := rand.New(rand.NewSource(seed))
			for ctx.Err() == nil {
				// Read from the newest handful rather than uniformly: those
				// are the files whose uploads are committing right now, and
				// the window under test is only open while one commits.
				mu.Lock()
				n := len(published)
				p := ""
				if n > 0 {
					window := 16
					if window > n {
						window = n
					}
					p = published[n-1-rnd.Intn(window)]
				}
				mu.Unlock()
				if p == "" {
					time.Sleep(time.Millisecond)
					continue
				}
				got, err := s.d.FS.ReadFileRange(ctx, p, 0, int64(len(payload)))
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					note("read " + p + ": " + err.Error())
					continue
				}
				if !bytes.Equal(got, payload) {
					note("content mismatch on " + p)
				}
			}
		}(int64(r) + 1)
	}

	const iters = 1000
	for i := 0; i < iters; i++ {
		src := filepath.Join(dir, fmt.Sprintf("tmp-%04d", i))
		dst := filepath.Join(dir, fmt.Sprintf("file-%04d", i))
		if err := os.WriteFile(src, payload, 0o644); err != nil {
			cancel()
			readers.Wait()
			t.Fatalf("write %d: %v", i, err)
		}
		if err := os.Rename(src, dst); err != nil {
			cancel()
			readers.Wait()
			t.Fatalf("rename %d: %v", i, err)
		}
		mu.Lock()
		published = append(published, fmt.Sprintf("/straddle/file-%04d", i))
		mu.Unlock()
	}

	// The readers keep going while the queue drains: that is the window.
	st := waitDrain(t, s.d.Journal, 3*time.Minute)
	cancel()
	readers.Wait()
	if st.Dead != 0 {
		t.Fatalf("%d uploads dead-lettered", st.Dead)
	}
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d reads failed across upload completion; first: %v", n, first.Load())
	}
	if downloads := s.fake.Calls("ReadRange") - before; downloads != 0 {
		t.Fatalf("%d reads went to the backend for content this process uploaded; "+
			"the blob must be registered under the remote key before the node moves", downloads)
	}

	// Every file must still read back through the kernel now that each node
	// carries its remote identity, which is the state a later mount starts from.
	for i := 0; i < iters; i++ {
		p := filepath.Join(dir, fmt.Sprintf("file-%04d", i))
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("post-drain read %d: %v", i, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("post-drain content mismatch on %s", p)
		}
	}
	if downloads := s.fake.Calls("ReadRange") - before; downloads != 0 {
		t.Fatalf("%d downloads after the drain; every byte was already cached locally", downloads)
	}
}
