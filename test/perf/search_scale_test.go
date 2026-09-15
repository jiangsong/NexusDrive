package perf

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/testx"
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

// scaleDirs is how many 5000-file directories the scale test ingests. The
// full million (200) costs about seven minutes through the listing path on
// the machine this was written on, which with the rest of the package is
// over go test's default timeout, so the default is half that and
// CLOUDFS_SCALE_DIRS=200 runs the full size. The per-query numbers were
// measured at both: they do not move, because nothing in the query shape
// depends on the tree size once it stops scanning nodes.
func scaleDirs(t *testing.T) int {
	if raw := os.Getenv("CLOUDFS_SCALE_DIRS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			t.Fatalf("CLOUDFS_SCALE_DIRS=%q is not a directory count", raw)
		}
		return n
	}
	return 100
}

// TestSearchScaleMillionNodeTree ingests scaleDirs × 5000 files through the
// real listing path and asserts latency percentiles and that no query plan
// scans nodes: a plan that does would answer in time here and grow with the
// tree.
func TestSearchScaleMillionNodeTree(t *testing.T) {
	if testing.Short() {
		t.Skip("large-tree ingest and query baseline (minutes)")
	}
	if testx.RaceEnabled {
		t.Skip("ingesting hundreds of thousands of nodes is a timing test; under -race it alone exceeds the package timeout")
	}
	h := newHarness(t, 4096, 0, 0)
	ctx := context.Background()
	store := h.fs.Meta()
	exts := []string{"go", "md", "txt", "pdf"}
	start := time.Now()
	dirs, perDir := scaleDirs(t), 5000
	for d := 0; d < dirs; d++ {
		parent, err := store.Upsert(ctx, meta.Node{ParentIno: meta.RootIno, Name: fmt.Sprintf("dir%03d", d), Kind: provider.KindDir, Remote: "ali", RemoteID: fmt.Sprintf("d%03d", d), TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		l, err := store.BeginDirListing(ctx, parent)
		if err != nil {
			t.Fatal(err)
		}
		batch := make([]meta.Node, 0, meta.DirListingBatch)
		for i := 0; i < perDir; i++ {
			batch = append(batch, meta.Node{Name: fmt.Sprintf("file-%03d-%05d.%s", d, i, exts[i%4]), Kind: provider.KindFile, Size: int64(i * 37 % 100000),
				MTime: time.Unix(int64(1_700_000_000+d*perDir+i), 0), Remote: "ali", RemoteID: fmt.Sprintf("f%03d-%05d", d, i), Version: "1", TTL: time.Hour})
			if len(batch) == meta.DirListingBatch {
				if err := l.Append(ctx, batch); err != nil {
					t.Fatal(err)
				}
				batch = batch[:0]
			}
		}
		if err := l.Commit(ctx, time.Hour, nil); err != nil {
			t.Fatal(err)
		}
		l.Close()
	}
	if err := store.FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("ingest %d nodes through DirListing: %s", dirs*perDir, time.Since(start))
	p95 := func(f meta.Filter, sort string) time.Duration {
		var samples []time.Duration
		for i := 0; i < 20; i++ {
			s := time.Now()
			if _, err := store.Find(ctx, meta.SearchQuery{Filter: f, Limit: 20, Sort: sort}); err != nil {
				t.Fatal(err)
			}
			samples = append(samples, time.Since(s))
		}
		slices.Sort(samples)
		return samples[18]
	}
	last := fmt.Sprintf("file-%03d-04999", dirs-1)
	for _, tc := range []struct {
		q, sort string
		max     time.Duration
	}{
		{last, "", 100 * time.Millisecond},
		{"99", "", 50 * time.Millisecond},
		{"ext:go size:>1k", "mtime", 100 * time.Millisecond},
		{"*.pdf", "size", 100 * time.Millisecond},
	} {
		f, err := meta.ParseQuery(tc.q)
		if err != nil {
			t.Fatal(err)
		}
		got := p95(f, tc.sort)
		if got > tc.max {
			t.Errorf("%q p95 = %s, want < %s", tc.q, got, tc.max)
		}
		plan, err := store.ExplainFind(ctx, meta.SearchQuery{Filter: f, Limit: 20, Sort: tc.sort})
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range plan {
			if strings.HasPrefix(line, "SCAN n") {
				t.Errorf("%q: full table scan: %s", tc.q, line)
			}
		}
		t.Logf("%q sort=%q: p95 %s; plan: %s", tc.q, tc.sort, got, strings.Join(plan, " | "))
	}
}
