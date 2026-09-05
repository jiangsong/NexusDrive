package bench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeMetrics serves a /metrics page whose counters grow on every scrape, so
// a test can check that deltas are taken around each run.
func fakeMetrics(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	var scrapes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cache/drop" {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			fmt.Fprint(w, `{"files_dropped":3}`)
			return
		}
		n := scrapes.Add(1)
		fmt.Fprintf(w, "# HELP x\ncloudfs_remote_calls_total{op=\"_all\",remote=\"r\"} %d\n", n*10)
		fmt.Fprintf(w, "cloudfs_remote_calls_total{op=\"list\",remote=\"r\"} %d\n", n*7)
		fmt.Fprintf(w, "cloudfs_remote_calls_total{op=\"read_range\",remote=\"r\"} %d\n", n*3)
		fmt.Fprintf(w, "cloudfs_cache_hits_total %d\ncloudfs_cache_misses_total %d\n", n*100, n*5)
		fmt.Fprintf(w, "cloudfs_remote_read_bytes_total{remote=\"r\"} %d\n", n*4096)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), &scrapes
}

func smallOptions(dir, label string) Options {
	return Options{Dir: dir, Label: label, BigMiB: 4, SmallCount: 20, SmallSize: 512, MetaFiles: 50, Threads: 2}
}

func TestPrepareIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	o := smallOptions(dir, "x")
	if err := Prepare(dir, o); err != nil {
		t.Fatal(err)
	}
	big, err := os.Stat(filepath.Join(dir, bigName))
	if err != nil || big.Size() != 4<<20 {
		t.Fatalf("big file = %v, %v", big, err)
	}
	quarter, _ := os.Stat(filepath.Join(dir, big4Name))
	if quarter.Size() != 1<<20 {
		t.Fatalf("quarter file = %d", quarter.Size())
	}
	var files int
	filepath.WalkDir(filepath.Join(dir, treeDir), func(p string, d os.DirEntry, err error) error {
		if !d.IsDir() {
			files++
		}
		return nil
	})
	if files != treeDirs*treeFiles {
		t.Fatalf("tree has %d files", files)
	}
	before, _ := os.Stat(filepath.Join(dir, bigName))
	if err := Prepare(dir, o); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(dir, bigName))
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("a second Prepare rewrote a file that was already right")
	}
}

func TestRunEmitsOneLinePerResultWithDeltas(t *testing.T) {
	dir := t.TempDir()
	addr, scrapes := fakeMetrics(t)
	o := smallOptions(dir, "unit")
	if err := Prepare(dir, o); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	o.Out = &out
	o.Metrics = addr
	o.Repeat = 3
	results, err := Run(context.Background(), o, []string{"walk", "seqread4k", "randread", "smallfiles", "metadata", "stress", "randwrite"})
	if err != nil {
		t.Fatal(err)
	}
	// metadata expands to four phases.
	want := 6 + 4
	if len(results) != want {
		t.Fatalf("got %d results, want %d", len(results), want)
	}
	lines := 0
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var r Result
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line is not JSON: %s", sc.Text())
		}
		if r.Label != "unit" || r.Runs != 3 {
			t.Fatalf("result = %+v", r)
		}
		if r.Failed {
			t.Fatalf("%s failed: %s", r.Test, r.Note)
		}
		if r.MinMS > r.WallMS || r.WallMS > r.MaxMS {
			t.Fatalf("median outside min/max: %+v", r)
		}
		// Each run scrapes twice and the fake counters grow by 10 per
		// scrape, so every delta is exactly 10 calls: 7 list + 3 read_range.
		if r.RemoteCalls != 10 || r.RemoteByOp["list"] != 7 || r.RemoteByOp["read_range"] != 3 {
			t.Fatalf("delta = %d %v", r.RemoteCalls, r.RemoteByOp)
		}
		if r.CacheHits != 100 || r.CacheMisses != 5 || r.RemoteReadBytes != 4096 {
			t.Fatalf("cache/bytes delta wrong: %+v", r)
		}
		lines++
	}
	if lines != want {
		t.Fatalf("emitted %d lines, want %d", lines, want)
	}
	// 7 workloads × 3 repeats × 2 scrapes.
	if got := scrapes.Load(); got != 42 {
		t.Fatalf("scraped %d times, want 42", got)
	}
}

func TestColdHookRunsBeforeEveryRepetition(t *testing.T) {
	dir := t.TempDir()
	o := smallOptions(dir, "cold")
	if err := Prepare(dir, o); err != nil {
		t.Fatal(err)
	}
	var calls int
	o.Cold = func(context.Context) error { calls++; return nil }
	o.Repeat = 4
	o.Out = &bytes.Buffer{}
	if _, err := Run(context.Background(), o, []string{"statstorm"}); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("cold hook ran %d times, want 4", calls)
	}
}

func TestDropCachesHitsTheControlEndpoint(t *testing.T) {
	addr, _ := fakeMetrics(t)
	if err := DropCaches(addr); err != nil {
		t.Fatal(err)
	}
	if err := DropCaches("127.0.0.1:1"); err == nil {
		t.Fatal("an unreachable daemon should be an error")
	}
}

func TestUnknownWorkloadIsRejected(t *testing.T) {
	o := smallOptions(t.TempDir(), "x")
	o.Out = &bytes.Buffer{}
	if _, err := Run(context.Background(), o, []string{"fly"}); err == nil {
		t.Fatal("unknown workload accepted")
	}
}

func TestStressReportsCorruptionAsFailure(t *testing.T) {
	// A directory that silently loses writes must be reported, not timed.
	dir := t.TempDir()
	o := smallOptions(dir, "bad")
	o.Threads = 1
	if err := os.MkdirAll(filepath.Join(dir, scratch), 0o755); err != nil {
		t.Fatal(err)
	}
	// Make the scratch dir unwritable so every iteration fails.
	if err := os.Chmod(filepath.Join(dir, scratch), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(dir, scratch), 0o700)
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	r := stress(&o, 2, 1)
	if !r.Failed {
		t.Fatal("a workload that could not write should be marked failed")
	}
}

func TestParseMetricsIgnoresGarbage(t *testing.T) {
	in := "# comment\nnot a metric\ncloudfs_cache_hits_total abc\ncloudfs_cache_hits_total 12\ncloudfs_remote_calls_total{op=\"stat\",remote=\"a\"} 3\ncloudfs_remote_calls_total{op=\"stat\",remote=\"b\"} 4\n"
	s := parseMetrics(bufio.NewScanner(strings.NewReader(in)))
	if !s.ok || s.hits != 12 || s.calls["stat"] != 7 {
		t.Fatalf("parsed %+v", s)
	}
}
