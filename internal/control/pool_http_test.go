package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/meta"
	"cloudfs/internal/pool"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

// poolFixture is a control server over a VFS whose root is a pool of two
// fakes, plus a config file the endpoints can edit.
type poolFixture struct {
	srv  *Server
	pool *pool.Pool
	a, b *fakeprovider.Fake
	cfg  string
}

func newPoolFixture(t *testing.T) *poolFixture {
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
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.SetQuota(1000, 100)
	b.SetQuota(1000, 300)
	p, err := pool.New(pool.Options{Name: "home", StateDir: filepath.Join(dir, "pool"), Settings: config.Pool{Replicas: 2, MinReplicas: 1, ProbeInterval: time.Hour},
		Members: []pool.Member{{Name: "a", Provider: a}, {Name: "b", Provider: b}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	fsys, err := vfs.New(vfs.Options{Meta: store, Cache: ca, AttrTTL: time.Minute, DefaultDirTTL: time.Minute,
		Mounts: []vfs.Mount{{Prefix: "/", Remote: "home", RootID: p.RootID(), Provider: p, Mode: config.ModeWriteback, DirTTL: time.Minute}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`
remotes:
  a: {type: fake}
  b: {type: fake}
  c: {type: fake}
  home: {type: pool, pool: home}
pools:
  home:
    members: [{remote: a}, {remote: b}]
    replicas: 2
mounts:
  - path: /mnt/cloud
    layout:
      /: {remote: home}
`), 0o600)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	coll := &Collector{Version: "pool-test", Config: cfg, FS: fsys, Pools: map[string]*pool.Pool{"home": p}}
	return &poolFixture{srv: NewServer(coll), pool: p, a: a, b: b, cfg: cfgPath}
}

func (f *poolFixture) do(t *testing.T, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1:9101"+target, strings.NewReader(body))
	req.Host = "127.0.0.1:9101"
	req.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet {
		req.Header.Set("X-CloudFS-Control", "1")
	}
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	return w
}

func TestPoolStatusShowsMembersAndSpaceWithoutSecrets(t *testing.T) {
	f := newPoolFixture(t)
	f.a.Seed("/only-a.txt", []byte("a"))
	f.b.Seed("/only-a.txt", []byte("a"))
	f.a.Seed("/single.txt", []byte("one copy"))
	if _, err := f.srv.collector.FS.ReadDirPath(context.Background(), "/"); err != nil {
		t.Fatal(err)
	}
	w := f.do(t, http.MethodGet, "/pool/status", "")
	if w.Code != 200 {
		t.Fatalf("status = %d %s", w.Code, w.Body.String())
	}
	var out PoolStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Pools) != 1 || out.Pools[0].Name != "home" || len(out.Pools[0].Members) != 2 {
		t.Fatalf("pools = %+v", out.Pools)
	}
	p := out.Pools[0]
	if p.Total != 1000 || p.Used != 200 || p.Members[1].Free != 700 || p.Members[0].State != "up" {
		t.Fatalf("space/health = %+v", p)
	}
	if p.Files != 2 || p.UnderReplicated != 1 {
		t.Fatalf("counts = files %d under %d", p.Files, p.UnderReplicated)
	}
	if strings.Join(out.Candidates, ",") != "c" || !out.Configurable {
		t.Fatalf("candidates = %v configurable=%v", out.Candidates, out.Configurable)
	}
	for _, secret := range []string{"refresh_token", "client_secret", "password", "cookie", `"remote_id"`, "n1"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("status leaks %q", secret)
		}
	}
}

func TestPoolMemberEditsShowPendingRestartAndRemovalIsIdempotent(t *testing.T) {
	f := newPoolFixture(t)

	w := f.do(t, http.MethodPost, "/pool/members", `{"pool":"home","remote":"c"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("add c = %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodPost, "/pool/members/drain", `{"pool":"home","remote":"b","confirm":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("drain b = %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodPost, "/pool/members/remove", `{"pool":"home","remote":"b","confirm":true}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"restart_required":true`) {
		t.Fatalf("remove b = %d %s", w.Code, w.Body.String())
	}

	// The in-memory configuration view must follow the saved desired state,
	// while the running pool deliberately keeps b and lacks c until restart.
	configured := f.srv.collector.ConfigView().Pools["home"].Members
	if len(configured) != 2 || configured[0].Remote != "a" || configured[1].Remote != "c" {
		t.Fatalf("configured members = %+v", configured)
	}
	w = f.do(t, http.MethodGet, "/pool/status", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d %s", w.Code, w.Body.String())
	}
	var out PoolStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	pending := map[string]string{}
	for _, member := range out.Pools[0].Members {
		pending[member.Remote] = member.PendingRestart
	}
	if pending["a"] != "" || pending["b"] != "remove" || pending["c"] != "add" {
		t.Fatalf("pending member changes = %+v", pending)
	}

	// A repeated click/request before restart asks for the same desired state;
	// it is a success, not a false "not a member" error.
	w = f.do(t, http.MethodPost, "/pool/members/remove", `{"pool":"home","remote":"b","confirm":true}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"restart_required":true`) {
		t.Fatalf("repeat remove b = %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodPost, "/pool/members/remove?lang=zh", `{"pool":"home","remote":"ghost","confirm":true}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"ghost" 不是存储池 "home" 的成员`) {
		t.Fatalf("localized missing member = %d %s", w.Code, w.Body.String())
	}
}

func TestFsListReportsAvailability(t *testing.T) {
	f := newPoolFixture(t)
	f.a.Seed("/both.txt", []byte("x"))
	f.b.Seed("/both.txt", []byte("x"))
	f.a.Seed("/only-a.txt", []byte("y"))
	ctx := context.Background()
	if _, err := f.srv.collector.FS.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	get := func() map[string]FSEntry {
		w := f.do(t, http.MethodGet, "/fs/list?path=/", "")
		if w.Code != 200 {
			t.Fatalf("list = %d %s", w.Code, w.Body.String())
		}
		var out FSListResponse
		json.Unmarshal(w.Body.Bytes(), &out)
		m := map[string]FSEntry{}
		for _, e := range out.Entries {
			m[e.Name] = e
		}
		return m
	}
	m := get()
	if m["both.txt"].Availability != "full" || m["both.txt"].ReplicasLive != 2 {
		t.Fatalf("both = %+v", m["both.txt"])
	}
	if m["only-a.txt"].Availability != "degraded" || m["only-a.txt"].ReplicasLive != 1 || m["only-a.txt"].ReplicasTarget != 2 {
		t.Fatalf("single = %+v", m["only-a.txt"])
	}
	// a goes down: its lone file is unavailable, the replicated one is
	// degraded (one member down) but readable.
	f.a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	for i := 0; i < 3; i++ {
		f.srv.collector.FS.DropCaches(ctx)
		f.srv.collector.FS.ReadDirPath(ctx, "/")
	}
	m = get()
	if m["only-a.txt"].Availability != "unavailable" || m["only-a.txt"].DegradedReason == "" {
		t.Fatalf("with a down: %+v", m["only-a.txt"])
	}
	if m["both.txt"].Availability != "degraded" {
		t.Fatalf("replicated with a down: %+v", m["both.txt"])
	}
	w := f.do(t, http.MethodGet, "/fs/stat?path=/only-a.txt", "")
	var e FSEntry
	json.Unmarshal(w.Body.Bytes(), &e)
	if e.Availability != "unavailable" {
		t.Fatalf("stat = %+v", e)
	}
}

func TestPoolConfigEditsSayRestartRequired(t *testing.T) {
	f := newPoolFixture(t)
	// A drive added without a pool of its own, then made a member.
	w := f.do(t, http.MethodPost, "/accounts", `{"name":"e","type":"fake","fields":{}}`)
	if w.Code != 200 {
		t.Fatalf("add account = %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodPost, "/pool/members", `{"pool":"home","remote":"e","root":"/pool"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"restart_required":true`) {
		t.Fatalf("add member = %d %s", w.Code, w.Body.String())
	}
	cfg, _ := config.Load(f.cfg)
	if len(cfg.Pools["home"].Members) != 3 || cfg.Pools["home"].Members[2].Root != "/pool" {
		t.Fatalf("config = %+v", cfg.Pools["home"].Members)
	}
	w = f.do(t, http.MethodPost, "/pool/members", `{"pool":"home","remote":"ghost"}`)
	if w.Code != 400 {
		t.Fatalf("unknown member = %d", w.Code)
	}
	w = f.do(t, http.MethodPost, "/pool/members/drain", `{"pool":"home","remote":"b"}`)
	if w.Code != 400 {
		t.Fatalf("drain without confirm = %d", w.Code)
	}
	w = f.do(t, http.MethodPost, "/pool/members/drain", `{"pool":"home","remote":"b","confirm":true}`)
	if w.Code != 200 {
		t.Fatalf("drain = %d %s", w.Code, w.Body.String())
	}
	if st := f.pool.Status()[1].Health.State; st != "draining" {
		t.Fatalf("b = %s", st)
	}
	w = f.do(t, http.MethodPost, "/pool/members/state", `{"pool":"home","remote":"b","state":"enabled"}`)
	if w.Code != 200 {
		t.Fatalf("enable = %d", w.Code)
	}
	w = f.do(t, http.MethodPost, "/pool/repair", `{"pool":"home"}`)
	if w.Code != 200 {
		t.Fatalf("repair = %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodPost, "/pool/scrub", `{"pool":"home","path":"/a\u0000b"}`)
	if w.Code != 400 {
		t.Fatalf("bad path = %d", w.Code)
	}
	w = f.do(t, http.MethodPost, "/pool/scrub", `{"pool":"home","path":"/nope"}`)
	if w.Code != 404 {
		t.Fatalf("scrub of a missing path = %d", w.Code)
	}
	w = f.do(t, http.MethodPost, "/pool/create", `{"name":"second","members":["c"],"replicas":1,"mount":"/mnt/cloud","prefix":"/second"}`)
	if w.Code != 200 {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	cfg, _ = config.Load(f.cfg)
	if cfg.Remotes["second"].PoolOf() != "second" || cfg.Mounts[0].Layout["/second"].Remote != "second" {
		t.Fatalf("created pool not in config: %+v %+v", cfg.Remotes["second"], cfg.Mounts[0].Layout)
	}
	// Adding an account into a pool in one request.
	w = f.do(t, http.MethodPost, "/accounts", `{"name":"d","type":"fake","fields":{},"pool":"home"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"joined_pool":"home"`) {
		t.Fatalf("add account into pool = %d %s", w.Code, w.Body.String())
	}
	cfg, _ = config.Load(f.cfg)
	if len(cfg.Pools["home"].Members) != 4 {
		t.Fatalf("members after add = %+v", cfg.Pools["home"].Members)
	}
}

func TestPoolJoinReadsTheMarker(t *testing.T) {
	f := newPoolFixture(t)
	if err := f.pool.WriteMarkers(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A config that has the drives but no pool yet.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("remotes:\n  a: {type: fake}\n  b: {type: fake}\nmounts: []\n"), 0o600)
	cfg, _ := config.Load(cfgPath)
	srv := NewServer(&Collector{Config: cfg, Providers: map[string]provider.Provider{"a": f.a, "b": f.b}})
	do := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9101/pool/join", strings.NewReader(body))
		req.Host = "127.0.0.1:9101"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CloudFS-Control", "1")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}
	w := do(`{"remote":"b"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"confirm_required":true`) || !strings.Contains(w.Body.String(), `"pool":"home"`) {
		t.Fatalf("preview = %d %s", w.Code, w.Body.String())
	}
	w = do(`{"remote":"b","confirm":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"restart_required":true`) {
		t.Fatalf("join = %d %s", w.Code, w.Body.String())
	}
	joined, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p := joined.Pools["home"]
	if len(p.Members) != 2 || p.Replicas != 2 || joined.Remotes["home"].PoolOf() != "home" {
		t.Fatalf("joined config = %+v", joined.Pools)
	}
}
