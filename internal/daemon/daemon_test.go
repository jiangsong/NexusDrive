package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"

	_ "cloudfs/internal/provider/webdav"

	"cloudfs/internal/i18n"
)

// tokenCapturingFake records whether SetTokenPersister was called, so a test
// can tell whether Open() actually wired the hook without depending on
// provider.TokenPersistence's private state.
type tokenCapturingFake struct {
	*fakeprovider.Fake
	persister provider.TokenPersister
}

func (f *tokenCapturingFake) SetTokenPersister(save provider.TokenPersister) {
	f.persister = save
}

func init() {
	// fake-nocap: a driver that advertises no MaxConnsPerHost of its own,
	// the way a not-yet-updated driver would. TestPerRemoteMaxConnsReachesCapabilities
	// uses it to check the no-override/no-declared-cap case.
	provider.Register("fake-nocap", func(name string, _ map[string]any) (provider.Provider, error) {
		f := fakeprovider.New(name)
		f.SetCaps(provider.Caps{})
		return f, nil
	})
	// fake-token: a driver that both persists tokens and (like gdrive,
	// onedrive, box) is a provider.TokenPersistenceSetter.
	// TestTokenPersistenceSurvivesMaxConnsOverride uses it to pin the
	// ordering in Open() between that check and provider.WithMaxConns.
	provider.Register("fake-token", func(name string, _ map[string]any) (provider.Provider, error) {
		return &tokenCapturingFake{Fake: fakeprovider.New(name)}, nil
	})
}

const baseConfig = `
cache:
  dir: %s
  max_size: 100MiB
  min_free: 1MiB
  block_size: 64KiB
proxy:
  outbounds:
    - { name: px, type: socks5, addr: 127.0.0.1:7890 }
  groups:
    - { name: proxy, type: fallback, members: [px] }
  rules:
    - DOMAIN-SUFFIX,googleapis.com,proxy
    - FINAL,direct
remotes:
  demo: { type: fake }
  slow: { type: fake, qps: { meta: 1, download: 2, upload: 1 } }
mounts:
  - path: /mnt/cloud
    layout:
      /demo: { remote: demo, root: root, mode: writeback }
      /slow: { remote: slow, root: root, mode: readonly, dir_ttl: 1h }
mcp:
  allow: [/demo]
`

func writeConfig(t *testing.T, body string) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(body, cacheDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, cacheDir
}

func TestOpenAssemblesEverything(t *testing.T) {
	cfg, cacheDir := writeConfig(t, baseConfig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if len(d.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(d.Providers))
	}
	if d.Meta == nil || d.Cache == nil || d.Journal == nil || d.Uploader == nil || d.FS == nil {
		t.Fatal("a component is missing")
	}
	// The mount layout became VFS mounts, deepest prefix first.
	mounts := d.FS.Mounts()
	if len(mounts) != 2 {
		t.Fatalf("mounts = %+v", mounts)
	}
	byPrefix := map[string]string{}
	for _, m := range mounts {
		byPrefix[m.Prefix] = string(m.Mode)
	}
	if byPrefix["/demo"] != "writeback" || byPrefix["/slow"] != "readonly" {
		t.Fatalf("modes = %+v", byPrefix)
	}
	// The per-mount TTL override reached the VFS.
	for _, m := range mounts {
		if m.Prefix == "/slow" && m.DirTTL != time.Hour {
			t.Fatalf("dir_ttl override lost: %v", m.DirTTL)
		}
	}
	// Blocks go under cache.dir; state that cannot be fetched again stays
	// beside the configuration file. See Config.StateDir.
	if _, err := os.Stat(filepath.Join(cacheDir, "blocks")); err != nil {
		t.Errorf("missing blocks: %v", err)
	}
	stateDir := filepath.Dir(cfg.SourcePath)
	for _, sub := range []string{"journal", "meta.db"} {
		if _, err := os.Stat(filepath.Join(stateDir, sub)); err != nil {
			t.Errorf("missing %s: %v", sub, err)
		}
	}
	// The block size from the config is in effect.
	if got := d.Cache.BlockSize(); got != 64<<10 {
		t.Errorf("block size = %d", got)
	}
}

func TestPerRemoteQPSOverridesReachTheLimiter(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// "slow" declared meta: 1; "demo" did not, so it gets the default.
	slow := d.Limiters.Limiter(ratelimit.Key{Remote: "slow", Class: ratelimit.Meta}).Rate()
	demo := d.Limiters.Limiter(ratelimit.Key{Remote: "demo", Class: ratelimit.Meta}).Rate()
	if slow != 1 {
		t.Fatalf("slow meta rate = %v, want the configured 1", slow)
	}
	if demo <= slow {
		t.Fatalf("demo (%v) should use the higher default, not the override (%v)", demo, slow)
	}
	if up := d.Limiters.Limiter(ratelimit.Key{Remote: "slow", Class: ratelimit.Upload}).Rate(); up != 1 {
		t.Fatalf("slow upload rate = %v", up)
	}
}

func TestPerRemoteMaxConnsReachesCapabilities(t *testing.T) {
	body := strings.Replace(baseConfig,
		"  demo: { type: fake }",
		"  demo: { type: fake, max_conns: 2 }\n  nocap: { type: fake-nocap }", 1)
	cfg, _ := writeConfig(t, body)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if got := d.Providers["demo"].Capabilities().MaxConnsPerHost; got != 2 {
		t.Fatalf("demo (max_conns: 2) caps.MaxConnsPerHost = %d, want 2", got)
	}
	// "slow" declared no override, so the driver's own default (fakeprovider
	// advertises 8) stands.
	if got := d.Providers["slow"].Capabilities().MaxConnsPerHost; got != 8 {
		t.Fatalf("slow (no override) caps.MaxConnsPerHost = %d, want the driver's own 8", got)
	}
	// "nocap" declares no MaxConnsPerHost and has no override, so the
	// three-way fallback (override, declared cap, 8) lands on 8 for the
	// transport (buildProvider's setConns) — Capabilities() must report the
	// same 8, not the driver's own undeclared 0, or a caller sizing work off
	// Capabilities() would think the connection is unbounded when the
	// transport actually caps it.
	if got := d.Providers["nocap"].Capabilities().MaxConnsPerHost; got != 8 {
		t.Fatalf("nocap (no declared cap, no override) caps.MaxConnsPerHost = %d, want the same 8 the transport uses", got)
	}
}

// TestTokenPersistenceSurvivesMaxConnsOverride pins the ordering in Open():
// the provider.TokenPersistenceSetter check must run before
// provider.WithMaxConns wraps p, because that wrapper does not forward
// interfaces outside this package's own optional set. Wrapping first would
// silently drop OAuth token persistence for gdrive/onedrive/box whenever
// max_conns is configured on those remotes.
func TestTokenPersistenceSurvivesMaxConnsOverride(t *testing.T) {
	body := strings.Replace(baseConfig,
		"  demo: { type: fake }",
		"  demo: { type: fake }\n  tok: { type: fake-token, max_conns: 3 }", 1)
	cfg, _ := writeConfig(t, body)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	tok, ok := provider.Unwrap(d.Providers["tok"]).(*tokenCapturingFake)
	if !ok {
		t.Fatalf("Unwrap did not reach *tokenCapturingFake, got %T", provider.Unwrap(d.Providers["tok"]))
	}
	if tok.persister == nil {
		t.Fatal("Open() did not install the token persister once max_conns was set on the remote")
	}
	// The override still took effect alongside it.
	if got := d.Providers["tok"].Capabilities().MaxConnsPerHost; got != 3 {
		t.Fatalf("tok caps.MaxConnsPerHost = %d, want 3", got)
	}
}

func TestProxyRoutingIsBuilt(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	o, err := d.Proxy.OutboundFor("www.googleapis.com")
	if err != nil {
		t.Fatal(err)
	}
	if o.Name != "px" {
		t.Fatalf("googleapis should route through the proxy group, got %+v", o)
	}
	o, err = d.Proxy.OutboundFor("openapi.alipan.com")
	if err != nil {
		t.Fatal(err)
	}
	if o.Type != "direct" {
		t.Fatalf("a Chinese host should go direct, got %+v", o)
	}
}

func TestSharedHTTPClientReachesDrivers(t *testing.T) {
	// A WebDAV remote proves the injection path: the factory must pick up the
	// daemon's client rather than building an unrouted one.
	body := strings.Replace(baseConfig,
		"  demo: { type: fake }",
		"  demo: { type: fake }\n  nas: { type: webdav, url: 'https://nas.example/dav', proxy: proxy }", 1)
	cfg, _ := writeConfig(t, body)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, ok := d.Providers["nas"]; !ok {
		t.Fatal("the webdav remote was not built")
	}
	// It also implements the post-construction injection route.
	if _, ok := provider.Unwrap(d.Providers["nas"]).(provider.Transporter); !ok {
		t.Error("the webdav driver should implement provider.Transporter")
	}
}

func TestRecoveryRunsBeforeAcceptingWork(t *testing.T) {
	cfg, cacheDir := writeConfig(t, baseConfig)
	ctx, cancel := context.WithCancel(context.Background())
	d, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	// Leave a partial write behind, as a crash would.
	s, err := d.Journal.NewStaging(nil)
	if err != nil {
		t.Fatal(err)
	}
	s.WriteAt([]byte("partial"), 0)
	orphan := s.Path
	s.Close()
	cancel()
	d.Close()

	// The next Open must clean it up before serving.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	d2, err := Open(ctx2, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("recovery should have removed the partial write at startup")
	}
	_ = cacheDir
}

func TestSkipWriteBuildsReadOnlyStack(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test", SkipWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Journal != nil || d.Uploader != nil {
		t.Fatal("SkipWrite should not build a journal or uploader")
	}
	// Reads still work.
	fake := provider.Unwrap(d.Providers["demo"]).(*fakeprovider.Fake)
	fake.Seed("readme.md", []byte("hello"))
	if _, err := d.FS.ReadDirPath(ctx, "/demo"); err != nil {
		t.Fatal(err)
	}
	got, err := d.FS.ReadFileRange(ctx, "/demo/readme.md", 0, 0)
	if err != nil || string(got) != "hello" {
		t.Fatalf("read = %q, %v", got, err)
	}
}

func TestBadConfigsAreRejectedClearly(t *testing.T) {
	cases := map[string]string{
		"unknown remote type": `
cache: { dir: %s, block_size: 64KiB }
remotes:
  ghost: { type: not-a-real-backend }
mounts:
  - path: /m
    layout:
      /: { remote: ghost }
`,
		"empty layout": `
cache: { dir: %s, block_size: 64KiB }
remotes:
  demo: { type: fake }
mounts:
  - path: /m
    layout: {}
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, _ := writeConfig(t, body)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d, err := Open(ctx, Options{Config: cfg, Version: "test"})
			if err == nil {
				d.Close()
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), "ghost") && !strings.Contains(err.Error(), "layout") {
				t.Fatalf("error should name the problem: %v", err)
			}
		})
	}
	// No mounts at all.
	if _, err := Open(context.Background(), Options{Config: &config.Config{}}); err == nil {
		t.Fatal("a config with no mounts should be rejected")
	}
	// A mount index out of range.
	cfg, _ := writeConfig(t, baseConfig)
	if _, err := Open(context.Background(), Options{Config: cfg, MountIndex: 9}); err == nil {
		t.Fatal("an out-of-range mount index should be rejected")
	}
}

func TestCollectorAndDoctorAreWired(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "wired"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	st := d.Collector().Collect(ctx, i18n.EN)
	if st.Version != "wired" || len(st.Mounts) != 2 {
		t.Fatalf("collector = %+v", st)
	}
	if len(st.Remotes) != 2 {
		t.Fatalf("the collector should report both remotes: %+v", st.Remotes)
	}
	checks := d.Doctor(nil, func() (bool, string) { return true, "" }).Run(ctx)
	names := map[string]bool{}
	for _, c := range checks {
		names[c.Name] = true
	}
	for _, want := range []string{"platform", "cache_dir", "metadata_db", "upload_queue"} {
		if !names[want] {
			t.Errorf("doctor is missing the %s check", want)
		}
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("second Close should be a no-op, got %v", err)
	}
}

// TestLimiterSeedsFromTheCapabilityMatrix is the regression for a default that
// silently throttled fast backends: the buckets were seeded from a built-in
// constant, so a backend on the local network that recommends 64 requests per
// second was held to 4, and a directory walk spent seconds waiting on a
// limiter rather than on the network.
func TestLimiterSeedsFromTheCapabilityMatrix(t *testing.T) {
	caps := map[string]provider.Caps{
		"fast": {QPS: provider.QPS{Meta: 64, Download: 64, Upload: 32}},
		"slow": {QPS: provider.QPS{Meta: 1, Download: 2, Upload: 1}},
	}
	cfg := &config.Config{Remotes: map[string]config.Remote{
		"fast": {Type: "fake"},
		"slow": {Type: "fake"},
		// An explicit setting must still win over the matrix.
		"pinned": {Type: "fake", QPS: &config.QPS{Meta: 3}},
	}}
	caps["pinned"] = provider.Caps{QPS: provider.QPS{Meta: 64}}
	reg := buildLimiters(cfg, func(remote string) (provider.Caps, bool) {
		c, ok := caps[remote]
		return c, ok
	})

	cases := []struct {
		remote string
		class  ratelimit.Class
		want   float64
	}{
		{"fast", ratelimit.Meta, 64},
		{"fast", ratelimit.Upload, 32},
		{"slow", ratelimit.Meta, 1},
		{"slow", ratelimit.Download, 2},
		{"pinned", ratelimit.Meta, 3},
	}
	for _, c := range cases {
		got := reg.Limiter(ratelimit.Key{Remote: c.remote, Class: c.class}).Rate()
		if got != c.want {
			t.Errorf("%s/%v rate = %v, want %v", c.remote, c.class, got, c.want)
		}
	}
	// A remote with no matrix entry still gets the conservative default.
	if got := reg.Limiter(ratelimit.Key{Remote: "unknown", Class: ratelimit.Meta}).Rate(); got != defaultRate(ratelimit.Meta) {
		t.Errorf("unknown remote rate = %v, want the built-in default %v", got, defaultRate(ratelimit.Meta))
	}
}

// TestTransferQPSFollowsTheSamePrecedenceAndFallsBackToDownload: Transfer
// resolves through the same config > Caps.QPS > built-in precedence as every
// other class, except its built-in default is 0 ("share Download") rather
// than a conservative constant.
func TestTransferQPSFollowsTheSamePrecedenceAndFallsBackToDownload(t *testing.T) {
	caps := map[string]provider.Caps{
		// aliyun-shaped: an explicit CDN rate.
		"cdn": {QPS: provider.QPS{Download: 4, Transfer: 16}},
		// a backend that has not opted in: Transfer defaults to 0 in Caps,
		// so reads should keep sharing Download.
		"nocdn": {QPS: provider.QPS{Download: 4}},
	}
	cfg := &config.Config{Remotes: map[string]config.Remote{
		"cdn":   {Type: "fake"},
		"nocdn": {Type: "fake"},
		// A config override still wins over Caps.
		"pinned": {Type: "fake", QPS: &config.QPS{Download: 4, Transfer: 8}},
	}}
	caps["pinned"] = provider.Caps{QPS: provider.QPS{Download: 4, Transfer: 16}}
	reg := buildLimiters(cfg, func(remote string) (provider.Caps, bool) {
		c, ok := caps[remote]
		return c, ok
	})

	if got := reg.Limiter(ratelimit.Key{Remote: "cdn", Class: ratelimit.Transfer}).Rate(); got != 16 {
		t.Errorf("cdn transfer rate = %v, want 16", got)
	}
	if got := reg.Limiter(ratelimit.Key{Remote: "pinned", Class: ratelimit.Transfer}).Rate(); got != 8 {
		t.Errorf("pinned transfer rate = %v, want the config override 8", got)
	}
	down := reg.Limiter(ratelimit.Key{Remote: "nocdn", Class: ratelimit.Download})
	xfer := reg.Limiter(ratelimit.Key{Remote: "nocdn", Class: ratelimit.Transfer})
	if down != xfer {
		t.Error("a backend with no Transfer recommendation should share the Download bucket")
	}
}

// TestCollectorCarriesTheCacheDropHook: the benchmark's cold mode depends on
// POST /cache/drop, which answers 501 when the collector is built without the
// hook — as happened once after a formatting change moved the field.
func TestCollectorCarriesTheCacheDropHook(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c := d.Collector()
	if c.DropCaches == nil {
		t.Fatal("collector has no DropCaches hook")
	}
	if _, err := c.DropCaches(ctx); err != nil {
		t.Fatalf("drop: %v", err)
	}
}

// TestSecondOpenDoesNotRunAnUploader: a process that does not own the queue
// must not upload from it. A status command that did would claim rows and
// push them through its own backend objects.
func TestSecondOpenDoesNotRunAnUploader(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if owner.Uploader == nil || !owner.Journal.Owner() {
		t.Fatal("the first process should own the queue and run the uploader")
	}
	second, err := Open(ctx, Options{Config: cfg, Version: "status"})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.Journal.Owner() {
		t.Fatal("the second process must not own the queue")
	}
	if second.Uploader != nil {
		t.Fatal("the second process must not run an uploader")
	}
	// It can still read the queue.
	if _, err := second.Journal.Stats(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMountDiscoversProviderRootWithoutExplicitConfig(t *testing.T) {
	// A layout that does not spell out `root:` must still mount. buildMounts
	// discovers the provider's own root (the fake's is "root", not "/") through
	// the same interface CheckAccount uses, reaching it via provider.Unwrap
	// because the mount provider is instrumented. Before the fix this listed
	// from a hardcoded "/", so every access to a real drive mounted without an
	// explicit root 404'd — the whole baseConfig only worked by spelling out
	// `root: root`.
	const body = `
cache: { dir: %s, block_size: 64KiB }
remotes:
  demo: { type: fake }
mounts:
  - path: /mnt/cloud
    layout:
      /demo: { remote: demo }
`
	cfg, _ := writeConfig(t, body)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	fake := provider.Unwrap(d.Providers["demo"]).(*fakeprovider.Fake)
	fake.Seed("readme.md", []byte("hi"))
	entries, err := d.FS.ReadDirPath(ctx, "/demo")
	if err != nil {
		t.Fatalf("listing a fake mounted without an explicit root: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "readme.md" {
		t.Fatalf("entries = %+v", entries)
	}
}

// TestPoolRemoteIsAssembledAfterItsMembers: a pool is a remote over other
// remotes. The daemon builds the members first, hands them to the pool, and
// mounts the pool like any drive — next to a direct view of one member.
func TestPoolRemoteIsAssembledAfterItsMembers(t *testing.T) {
	cfg, _ := writeConfig(t, `
cache:
  dir: %s
remotes:
  a: { type: fake }
  b: { type: fake }
  home: { type: pool, pool: home }
pools:
  home:
    members: [{remote: a}, {remote: b}]
    replicas: 2
mounts:
  - path: /mnt/cloud
    layout:
      /: { remote: home }
      /raw/a: { remote: a, mode: readonly }
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if len(d.Providers) != 3 || d.Pools["home"] == nil {
		t.Fatalf("providers = %d, pools = %v", len(d.Providers), d.Pools)
	}
	fa := provider.Unwrap(d.Providers["a"]).(*fakeprovider.Fake)
	fb := provider.Unwrap(d.Providers["b"]).(*fakeprovider.Fake)
	fa.Seed("/from-a.txt", []byte("a"))
	fb.Seed("/from-b.txt", []byte("b"))

	entries, err := d.FS.ReadDirPath(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name)
	}
	if strings.Join(got, ",") != "from-a.txt,from-b.txt,raw" {
		t.Fatalf("pool root through the VFS = %v", got)
	}
	data, err := d.FS.ReadFileRange(ctx, "/from-b.txt", 0, 0)
	if err != nil || string(data) != "b" {
		t.Fatalf("read through the pool = %q, %v", data, err)
	}
	// The member's traffic is counted on the member, so /status can show
	// what the pool sends each drive.
	if d.CallStats["b"].Total() == 0 {
		t.Fatal("the pool's calls to b are not counted on b")
	}
	// The direct view of a member coexists with the pool. It shows the
	// member as it is — the pool's marker file included, since that really
	// is on the drive.
	raw, err := d.FS.ReadDirPath(ctx, "/raw/a")
	if err != nil {
		t.Fatal(err)
	}
	var rawNames []string
	for _, e := range raw {
		rawNames = append(rawNames, e.Name)
	}
	if strings.Join(rawNames, ",") != ".cloudfs-pool.json,from-a.txt" && strings.Join(rawNames, ",") != "from-a.txt" {
		t.Fatalf("direct view = %v", rawNames)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.SourcePath), "pool", "pool-home.db")); err != nil {
		t.Fatalf("pool index not in the state dir: %v", err)
	}
}
