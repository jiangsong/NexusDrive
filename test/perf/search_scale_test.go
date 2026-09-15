package perf

import (
	"context"
	"testing"
	"time"

	"cloudfs/internal/vfs"
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

// The crawler lists each directory exactly once, and a disabled crawler is
// free: the existing call-count baselines in this package depend on that.
func TestCrawlListsEveryDirectoryExactlyOnce(t *testing.T) {
	h := newHarness(t, 4096, 0, 0)
	dirs := seedBinaryTree(h.fake, 9) // 1023 directories, at most two entries each
	ctx := context.Background()
	prog, err := h.fs.CrawlOnce(ctx, vfs.CrawlOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.fake.Calls("List"); got != dirs || int(prog.Listed) != dirs {
		t.Fatalf("List calls = %d, listed = %d, want %d", got, prog.Listed, dirs)
	}
	if h.fake.Calls("Stat") != 0 {
		t.Fatalf("crawl issued %d Stat calls; listings carry attributes", h.fake.Calls("Stat"))
	}
	if _, err := h.fs.CrawlOnce(ctx, vfs.CrawlOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := h.fake.Calls("List"); got != dirs {
		t.Fatalf("a second pass listed complete directories again: %d", got)
	}
	cov, _ := h.fs.Meta().Coverage(ctx)
	if cov.Listed != int64(dirs) || cov.Known != int64(dirs) {
		t.Fatalf("coverage %+v after a full crawl", cov)
	}
	t.Logf("crawled %d directories with %d List calls", dirs, h.fake.Calls("List"))
}

func TestCrawlerOffCostsNoCalls(t *testing.T) {
	h := newHarness(t, 4096, 0, 0)
	seedTree(h.fake, 5, 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.fs.StartCrawl(ctx, vfs.CrawlOptions{Enabled: false, IdleAfter: time.Millisecond, Rescan: time.Millisecond})
	defer h.fs.StopCrawl()
	time.Sleep(200 * time.Millisecond)
	if n := h.fake.TotalCalls(); n != 0 {
		t.Fatalf("a disabled crawler made %d provider calls", n)
	}
	if h.fs.CrawlProgress().Running {
		t.Fatal("a disabled crawler reports running")
	}
}
