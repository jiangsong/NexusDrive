package vfs

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

// seedBinaryTree seeds `depth` levels of two subdirectories each with one file
// per leaf directory: every directory holds at most fakeprovider.ListPageSize
// (2) children, so one List call lists one directory and Calls("List") counts
// directories. It returns that count, the provider root included.
func seedBinaryTree(f *fakeprovider.Fake, depth int) int {
	var walk func(prefix string, level int)
	walk = func(prefix string, level int) {
		if level == depth {
			f.Seed(prefix+"/leaf.txt", []byte("x"))
			return
		}
		walk(prefix+"/0", level+1)
		walk(prefix+"/1", level+1)
	}
	walk("", 0)
	return 1<<(depth+1) - 1
}

func TestCrawlListsEveryDirectoryOnceAndSkipsWhatIsAboveTheMount(t *testing.T) {
	e := newEnv(t, envOpt{})
	dirs := seedBinaryTree(e.fake, 3) // 15 directories under /ali
	ctx := context.Background()
	prog, err := e.fs.CrawlOnce(ctx, CrawlOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.fake.Calls("List"); got != dirs || int(prog.Listed) != dirs {
		t.Fatalf("List calls = %d, listed = %d, want %d", got, prog.Listed, dirs)
	}
	// The root sits above the only mount: nothing to ask a provider for.
	if prog.Skipped != 1 || prog.Failed != 0 || prog.Finished.IsZero() || prog.Running {
		t.Fatalf("progress after a full pass: %+v", prog)
	}
	cov, err := e.fs.Meta().Coverage(ctx)
	if err != nil || cov.Listed != int64(dirs) || cov.Known != int64(dirs)+1 {
		t.Fatalf("coverage %+v %v", cov, err)
	}
	if _, err := e.fs.CrawlOnce(ctx, CrawlOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.Calls("List"); got != dirs {
		t.Fatalf("a second pass listed complete directories again: %d", got)
	}
	if e.fake.Calls("Stat") != 0 {
		t.Fatalf("crawl issued %d Stat calls; listings carry attributes", e.fake.Calls("Stat"))
	}
}

func TestCrawlHonoursRemoteAndExcludeFilters(t *testing.T) {
	e := newEnv(t, envOpt{})
	e.fake.Seed("/src/main.go", []byte("x"))
	e.fake.Seed("/src/node_modules/dep/index.js", []byte("x"))
	e.fake.Seed("/build/out.bin", []byte("x"))
	ctx := context.Background()
	first, err := e.fs.CrawlOnce(ctx, CrawlOptions{Remotes: []string{"other"}})
	if err != nil {
		t.Fatal(err)
	}
	// / is above the mount and /ali is on a remote the crawl does not cover.
	if n := e.fake.TotalCalls(); n != 0 || first.Skipped != 2 {
		t.Fatalf("a crawl limited to another remote made %d calls, progress %+v", n, first)
	}
	prog, err := e.fs.CrawlOnce(ctx, CrawlOptions{Remotes: []string{"ali"}, Exclude: []string{"node_modules", "/ali/build"}})
	if err != nil {
		t.Fatal(err)
	}
	// /ali, /ali/src: listed. /, /ali/build, /ali/src/node_modules: skipped.
	// node_modules/dep is never known because its parent was not listed.
	// The counters are cumulative, so the second pass is read as a delta.
	if e.fake.Calls("List") != 2 || prog.Listed != 2 || prog.Skipped-first.Skipped != 3 {
		t.Fatalf("List calls = %d, progress %+v", e.fake.Calls("List"), prog)
	}
}

func TestCrawlYieldsToForegroundIO(t *testing.T) {
	e := newEnv(t, envOpt{})
	seedBinaryTree(e.fake, 4)
	e.fs.fgIO.Add(1) // the kernel is "waiting on us" for the whole test
	defer e.fs.fgIO.Add(-1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prog, err := e.fs.CrawlOnce(ctx, CrawlOptions{YieldMax: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if prog.Yields == 0 {
		t.Fatal("the crawler never stood aside for foreground IO")
	}
	if prog.Listed != 31 {
		t.Fatalf("yielding must end at YieldMax, not stop the crawl: listed %d", prog.Listed)
	}
}

func TestCrawlSleepsAfterRiskControl(t *testing.T) {
	e := newEnv(t, envOpt{})
	seedBinaryTree(e.fake, 5) // 63 directories
	e.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.RiskControlAfter = 10 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.fs.StartCrawl(ctx, CrawlOptions{Enabled: true, IdleAfter: time.Millisecond, RiskSleep: time.Hour})
	defer e.fs.StopCrawl()
	deadline := time.Now().Add(5 * time.Second)
	for e.fs.CrawlProgress().Paused != "risk_control" {
		if time.Now().After(deadline) {
			t.Fatalf("no risk-control pause: %+v", e.fs.CrawlProgress())
		}
		time.Sleep(10 * time.Millisecond)
	}
	lists := e.fake.Calls("List")
	time.Sleep(300 * time.Millisecond)
	if got := e.fake.Calls("List"); got != lists {
		t.Fatalf("List calls kept coming during the pause: %d -> %d", lists, got)
	}
	if p := e.fs.CrawlProgress(); p.ResumeAt.Before(time.Now().Add(50 * time.Minute)) {
		t.Fatalf("resume_at = %v, want ≈ now + RiskSleep", p.ResumeAt)
	}
	// Stopping does not wait out the hour.
	done := make(chan struct{})
	go func() { e.fs.StopCrawl(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StopCrawl waited on the risk-control sleep")
	}
	if p := e.fs.CrawlProgress(); p.Running || p.Paused != "" {
		t.Fatalf("progress after stop: %+v", p)
	}
}

func TestDisabledCrawlerRunsOnlyWhenKicked(t *testing.T) {
	e := newEnv(t, envOpt{})
	seedBinaryTree(e.fake, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if e.fs.KickCrawl() {
		t.Fatal("KickCrawl without a manager must report false")
	}
	e.fs.StartCrawl(ctx, CrawlOptions{Enabled: false, IdleAfter: time.Millisecond, Rescan: time.Millisecond})
	defer e.fs.StopCrawl()
	time.Sleep(100 * time.Millisecond)
	if n := e.fake.TotalCalls(); n != 0 {
		t.Fatalf("a disabled crawler made %d provider calls", n)
	}
	if !e.fs.KickCrawl() {
		t.Fatal("KickCrawl with a manager must report true")
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.fs.CrawlProgress().Finished.IsZero() {
		if time.Now().After(deadline) {
			t.Fatalf("kicked pass never finished: %+v", e.fs.CrawlProgress())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.fake.Calls("List"); got != 15 {
		t.Fatalf("kicked pass listed %d directories, want 15", got)
	}
}

// cutoffLister passes List through to the backend until `allow` calls have
// gone by, then cancels the pass: a process that died mid-sweep, at a
// known point.
type cutoffLister struct {
	provider.Provider
	allow  int32
	calls  atomic.Int32
	cancel context.CancelFunc
}

func (c *cutoffLister) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	if c.calls.Add(1) > c.allow {
		c.cancel()
		return nil, "", context.Canceled
	}
	return c.Provider.List(ctx, dirID, cursor)
}

// A pass interrupted part-way is resumed by the next process from dir_state:
// directories already complete are never listed again. The store is closed
// without any crawler shutdown, the way test/chaos models kill -9.
func TestCrawlResumesAfterAnUncleanStop(t *testing.T) {
	dir := t.TempDir()
	fake := fakeprovider.New("ali")
	const dirs = 63
	seedBinaryTree(fake, 5)
	open := func(backend provider.Provider) (*FS, *meta.Store) {
		store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
		if err != nil {
			t.Fatal(err)
		}
		ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 64, FreeSpace: func(string) (int64, error) { return 1 << 40, nil }})
		if err != nil {
			t.Fatal(err)
		}
		f, err := New(Options{Meta: store, Cache: ca, DefaultDirTTL: time.Hour, AttrTTL: time.Hour, NegativeTTL: time.Second,
			Mounts: []Mount{{Prefix: "/", Remote: "ali", RootID: fake.RootID(), Provider: backend, Mode: config.ModeWriteback, DirTTL: time.Hour}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ca.Close() })
		return f, store
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const firstPass = 10
	f1, store1 := open(&cutoffLister{Provider: fake, allow: firstPass, cancel: cancel})
	if _, err := f1.CrawlOnce(ctx, CrawlOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("the first pass should have been cut off: %v", err)
	}
	if listed := fake.Calls("List"); listed != firstPass {
		t.Fatalf("the first pass listed %d directories, want %d", listed, firstPass)
	}
	cov, err := store1.Coverage(context.Background())
	if err != nil || cov.Listed != firstPass {
		t.Fatalf("coverage after the interrupted pass: %+v %v", cov, err)
	}
	store1.Close() // no f1.Close(): nothing is flushed on purpose
	f2, store2 := open(fake)
	defer func() { f2.Close(); store2.Close() }()
	if _, err := f2.CrawlOnce(context.Background(), CrawlOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := fake.Calls("List"); got != dirs {
		t.Fatalf("List calls across both passes = %d, want %d: a complete directory was listed twice", got, dirs)
	}
	if cov, err := store2.Coverage(context.Background()); err != nil || cov.Listed != dirs || cov.Known != dirs {
		t.Fatalf("coverage after the resumed pass: %+v %v", cov, err)
	}
}

func TestYieldToForegroundReturnsAtOnceWhenIdle(t *testing.T) {
	e := newEnv(t, envOpt{})
	if e.fs.yieldToForeground(nil, time.Second) {
		t.Fatal("waited with no foreground IO")
	}
	e.fs.fgIO.Add(1)
	defer e.fs.fgIO.Add(-1)
	start := time.Now()
	if !e.fs.yieldToForeground(nil, 20*time.Millisecond) {
		t.Fatal("did not wait behind foreground IO")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("wait ignored its limit")
	}
	stop := make(chan struct{})
	close(stop)
	start = time.Now()
	e.fs.yieldToForeground(stop, time.Hour)
	if time.Since(start) > time.Second {
		t.Fatal("wait ignored stop")
	}
}
