package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/i18n"
	"cloudfs/internal/journal"
	"cloudfs/test/fakeprovider"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/proxy"
	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
)

type fixture struct {
	fake *fakeprovider.Fake
	dir   string
	meta  *meta.Store
	cache *cache.Cache
	j     *journal.Journal
	reg   *ratelimit.Registry
	coll  *Collector
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := cache.New(cache.Options{
		Dir: filepath.Join(dir, "cache"), BlockSize: 4096, MaxBytes: 1 << 20,
		FreeSpace: func(string) (int64, error) { return 100 << 30, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	reg := ratelimit.NewRegistry(
		func(ratelimit.Key) ratelimit.Options { return ratelimit.Options{Rate: 8, MinRate: 1} },
		ratelimit.BreakerOptions{Threshold: 1, Cooldown: time.Hour},
	)
	f := &fixture{dir: dir, meta: store, cache: ca, j: j, reg: reg}
	f.coll = &Collector{
		Version: "test", Started: time.Now().Add(-90 * time.Second),
		Journal: j, Cache: ca, Limiters: reg, Remotes: []string{"ali", "gdrive"},
		CacheMaxBytes: 1 << 20,
		FreeSpace:     func(string) (int64, error) { return 100 << 30, nil },
	}
	return f
}

// queue commits one upload so the journal has content.
func (f *fixture) queue(t *testing.T, name string, size int) journal.Upload {
	t.Helper()
	s, err := f.j.NewStaging([]provider.HashType{provider.HashSHA1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteAt(make([]byte, size), 0); err != nil {
		t.Fatal(err)
	}
	h, _ := s.Hashes()
	blob, err := f.j.CommitStaging(s, h)
	if err != nil {
		t.Fatal(err)
	}
	u := journal.Upload{
		ID: journal.NewID(), StagingID: s.ID, Remote: "ali", RemoteParentID: "root",
		Name: name, BlobPath: blob, Size: int64(size), Hashes: h,
	}
	if err := f.j.Commit(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestCollectReportsState(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.queue(t, "a.bin", 1024)
	f.cache.Put(cache.FileKey{Remote: "ali", RemoteID: "1", Version: "v"}, 0, make([]byte, 4096), 1<<20)
	f.cache.Get(cache.FileKey{Remote: "ali", RemoteID: "1", Version: "v"}, 0)
	f.cache.Get(cache.FileKey{Remote: "ali", RemoteID: "1", Version: "v"}, 9)

	st := f.coll.Collect(ctx, i18n.EN)
	if st.Version != "test" || st.UptimeStr == "" {
		t.Fatalf("status = %+v", st)
	}
	if st.Uploads.Pending != 1 || st.Uploads.QueuedBytes != 1024 {
		t.Fatalf("uploads = %+v", st.Uploads)
	}
	if st.Cache.Blocks != 1 || st.Cache.Bytes != 4096 {
		t.Fatalf("cache = %+v", st.Cache)
	}
	if st.Cache.HitRatio <= 0 || st.Cache.HitRatio >= 1 {
		t.Fatalf("hit ratio = %v, want a value between 0 and 1", st.Cache.HitRatio)
	}
	if st.Cache.BytesHuman != "4.0 KiB" {
		t.Fatalf("human bytes = %q", st.Cache.BytesHuman)
	}
	if len(st.Remotes) != 2 || st.Remotes[0].Remote != "ali" {
		t.Fatalf("remotes = %+v", st.Remotes)
	}
	if st.Remotes[0].MetaRate != 8 {
		t.Fatalf("meta rate = %v", st.Remotes[0].MetaRate)
	}
	if st.Remotes[0].TransferRate != 8 {
		t.Fatalf("transfer rate = %v", st.Remotes[0].TransferRate)
	}
}

func TestWarningsAreActionable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.queue(t, "dead.bin", 10)
	if err := f.j.Fail(ctx, u.ID, errors.New("permission denied")); err != nil {
		t.Fatal(err)
	}
	f.reg.Breaker("ali", "").Trip()

	st := f.coll.Collect(ctx, i18n.EN)
	joined := strings.Join(st.Warnings, "\n")
	if !strings.Contains(joined, "failed permanently") || !strings.Contains(joined, "uploads retry") {
		t.Fatalf("dead-letter warning should name the recovery command: %q", joined)
	}
	if !strings.Contains(joined, "paused until") || !strings.Contains(joined, "ali") {
		t.Fatalf("breaker warning missing: %q", joined)
	}
	// A healthy system has no warnings.
	f2 := newFixture(t)
	if w := f2.coll.Collect(ctx, i18n.EN).Warnings; len(w) != 0 {
		t.Fatalf("clean system produced warnings: %v", w)
	}
}

func TestCacheNearBudgetWarns(t *testing.T) {
	f := newFixture(t)
	// Fill past 90% of the 1 MiB budget.
	for i := int64(0); i < 240; i++ {
		f.cache.Put(cache.FileKey{Remote: "ali", RemoteID: "big", Version: "v"}, i, make([]byte, 4096), 1<<30)
	}
	st := f.coll.Collect(context.Background(), i18n.EN)
	joined := strings.Join(st.Warnings, "\n")
	if !strings.Contains(joined, "budget") {
		t.Fatalf("expected a cache budget warning, got %q", joined)
	}
}

func TestLowFreeSpaceWarns(t *testing.T) {
	f := newFixture(t)
	f.coll.FreeSpace = func(string) (int64, error) { return 100 << 20, nil }
	st := f.coll.Collect(context.Background(), i18n.EN)
	joined := strings.Join(st.Warnings, "\n")
	if !strings.Contains(joined, "ENOSPC") {
		t.Fatalf("low disk should warn about ENOSPC: %q", joined)
	}
}

func TestHTTPEndpoints(t *testing.T) {
	f := newFixture(t)
	srv := NewServer(f.coll)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz = %v, %v", resp, err)
	}
	resp.Body.Close()

	// readyz without a filesystem reports not ready rather than lying.
	resp, err = http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz without a mount = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Version != "test" {
		t.Fatalf("status json = %+v", st)
	}
}

func TestMetricsFormat(t *testing.T) {
	f := newFixture(t)
	f.queue(t, "m.bin", 2048)
	f.reg.Breaker("gdrive", "").Trip()
	srv := NewServer(f.coll)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 16384)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])

	for _, want := range []string{
		"# HELP cloudfs_cache_bytes",
		"# TYPE cloudfs_cache_bytes gauge",
		"cloudfs_uploads_pending 1",
		"cloudfs_uploads_queued_bytes 2048",
		`cloudfs_remote_rate_limit{class="meta",remote="ali"} 8`,
		`cloudfs_remote_rate_limit{class="transfer",remote="ali"} 8`,
		`cloudfs_remote_breaker_open{remote="gdrive"} 1`,
		`cloudfs_remote_breaker_open{remote="ali"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n---\n%s", want, body)
		}
	}
	// HELP must appear once per metric name, not once per sample.
	if strings.Count(body, "# HELP cloudfs_remote_rate_limit") != 1 {
		t.Error("HELP line repeated for a labelled metric")
	}
}

func TestCompleteFileAndWriteReservationsReachStatusAndMetrics(t *testing.T) {
	f := newFixture(t)
	defer f.cache.Close()
	p := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(p, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	k := cache.FileKey{Remote: "ali", RemoteID: "whole"}
	if err := f.cache.LinkFile(k, p, 7); err != nil {
		t.Fatal(err)
	}
	lease, err := f.cache.OpenWhole(k)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	release, err := f.cache.ReserveDisk(f.cache.Dir(), 99)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	st := f.coll.Collect(context.Background(), i18n.EN)
	if st.Cache.Bytes != 7 || st.Cache.WholeBytes != 7 || st.Cache.LeasedBytes != 7 || st.Cache.WriteReservedBytes != 99 {
		t.Fatalf("status: %+v", st.Cache)
	}
	rr := httptest.NewRecorder()
	NewServer(f.coll).Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{"cloudfs_cache_whole_bytes 7", "cloudfs_cache_leased_bytes 7", "cloudfs_journal_write_reserved_bytes 99"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("missing metric %s", want)
		}
	}
}

func TestDoctorReportsAndFixes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A dead upload and an orphan staging file give the doctor something to find.
	u := f.queue(t, "dead.bin", 10)
	if err := f.j.Fail(ctx, u.ID, errors.New("bad credentials")); err != nil {
		t.Fatal(err)
	}
	orphan, err := f.j.NewStaging(nil)
	if err != nil {
		t.Fatal(err)
	}
	orphan.WriteAt([]byte("partial"), 0)
	orphanPath := orphan.Path
	orphan.Close()

	d := &Doctor{
		CacheDir: filepath.Join(f.dir, "cache"), Journal: f.j, Meta: f.meta, Cache: f.cache,
		MinFree:       1 << 30,
		FreeSpace:     func(string) (int64, error) { return 100 << 30, nil },
		FUSESupported: func() (bool, string) { return true, "" },
	}
	checks := d.Run(ctx)
	byName := map[string]Check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if c := byName["cache_dir"]; c.Level != LevelOK {
		t.Errorf("cache_dir = %+v", c)
	}
	if c := byName["metadata_db"]; c.Level != LevelOK || !strings.Contains(c.Detail, "integrity ok") {
		t.Errorf("metadata_db = %+v", c)
	}
	if c := byName["upload_queue"]; c.Level != LevelFail || !strings.Contains(c.Fix, "uploads retry") {
		t.Errorf("upload_queue = %+v", c)
	}
	if c := byName["staging_files"]; c.Level != LevelWarn || !c.Fixable {
		t.Errorf("staging_files = %+v", c)
	}
	if c := byName["fuse"]; c.Level != LevelOK {
		t.Errorf("fuse = %+v", c)
	}
	_, warn, fail := Summary(checks)
	if warn == 0 || fail == 0 {
		t.Fatalf("summary should count the seeded problems: warn=%d fail=%d", warn, fail)
	}

	// --fix cleans up the orphan.
	done := d.Fix(ctx, i18n.EN)
	if len(done) == 0 {
		t.Fatal("fix reported nothing")
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatal("fix should have removed the orphan staging file")
	}
	joined := strings.Join(done, "; ")
	if !strings.Contains(joined, "staging") {
		t.Fatalf("fix output should mention the staging cleanup: %q", joined)
	}
}

func TestDoctorFlagsUnusableCacheDir(t *testing.T) {
	d := &Doctor{CacheDir: "/proc/cloudfs-cannot-create-this"}
	checks := d.Run(context.Background())
	var found bool
	for _, c := range checks {
		if c.Name == "cache_dir" {
			found = true
			if c.Level != LevelFail || c.Fix == "" {
				t.Fatalf("unusable cache dir = %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("cache_dir check missing")
	}
}

func TestDoctorFlagsMissingFUSE(t *testing.T) {
	d := &Doctor{FUSESupported: func() (bool, string) { return false, "/dev/fuse is missing" }}
	for _, c := range d.Run(context.Background()) {
		if c.Name == "fuse" {
			if c.Level != LevelFail || c.Fix == "" {
				t.Fatalf("missing FUSE = %+v", c)
			}
			return
		}
	}
	t.Fatal("fuse check missing")
}

func TestDoctorProxyChecks(t *testing.T) {
	mgr, err := proxy.NewManager(proxy.ManagerOptions{
		Outbounds: []proxy.Outbound{{Name: "px", Type: "http", Addr: "127.0.0.1:9"}},
		Groups:    []proxy.Group{{Name: "g", Members: []string{"px"}}},
		Check: func(context.Context, proxy.Outbound, string, time.Duration) (time.Duration, error) {
			return 0, errors.New("connection refused")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &Doctor{Proxy: mgr}
	for _, c := range d.Run(context.Background()) {
		if c.Name == "proxy/px" {
			if c.Level != LevelFail || !strings.Contains(c.Detail, "refused") {
				t.Fatalf("proxy check = %+v", c)
			}
			return
		}
	}
	t.Fatal("proxy check missing")
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0 B", 512: "512 B", 4096: "4.0 KiB",
		1 << 20: "1.0 MiB", 3 << 30: "3.0 GiB", 2 << 40: "2.0 TiB",
	} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestKernelAtLeast(t *testing.T) {
	cases := map[string]bool{
		"6.9.0":            true,
		"7.0.0-28-generic": true,
		"6.8.12":           false,
		"5.15.0":           false,
		"garbage":          false,
	}
	for rel, want := range cases {
		if got := kernelAtLeast(rel, 6, 9); got != want {
			t.Errorf("kernelAtLeast(%q) = %v, want %v", rel, got, want)
		}
	}
}

// TestProfilingOnlyOnALoopbackAddress: the profiling handlers hand out the
// process's memory, so asking for them is not enough — the daemon must also
// be listening somewhere only this machine can reach.
func TestProfilingOnlyOnALoopbackAddress(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:9101", true},
		{"localhost:9101", true},
		{"[::1]:9101", true},
		{"0.0.0.0:9101", false},
		{":9101", false},
		{"192.168.0.30:9101", false},
	} {
		if got := loopbackAddr(tc.addr); got != tc.want {
			t.Errorf("loopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}
