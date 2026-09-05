// Package daemon assembles the whole system from a config file: providers,
// proxy routing, rate limiters, metadata store, block cache, write journal,
// uploader, VFS, FUSE mounts, the MCP server and the control endpoints. It is
// the one place that knows how the pieces fit together.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/proxy"
	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
	"cloudfs/internal/upload"
	"cloudfs/internal/vfs"
)

// Daemon holds every long-lived component.
type Daemon struct {
	Config    *config.Config
	Meta      *meta.Store
	Cache     *cache.Cache
	Journal   *journal.Journal
	Uploader  *upload.Uploader
	FS        *vfs.FS
	Refresher *vfs.Refresher
	Proxy     *proxy.Manager
	Limiters  *ratelimit.Registry
	// Providers maps remote name to backend.
	Providers map[string]provider.Provider
	// CallStats maps remote name to its provider call counter. Every remote
	// request passes through one, which is what makes "a warm listing costs
	// zero calls" a measurable claim rather than an assertion.
	CallStats map[string]*provider.Stats
	// DropCaches empties the caches for a cold measurement. It defaults to
	// the VFS's own; a mounting process wraps it to also drop the kernel's.
	DropCaches func(ctx context.Context) (int, error)

	version string
	started time.Time
	closers []func() error
}

// Options configures Open.
type Options struct {
	Config  *config.Config
	Version string
	// MountIndex selects which mount from the config to build the VFS for.
	// Most deployments have exactly one.
	MountIndex int
	// SkipWrite builds a read-only stack without a journal or uploader.
	SkipWrite bool
	// RequireOwner rejects offline cache management while another process owns
	// the storage, before constructing providers or opening the cache.
	RequireOwner bool
	// NoBackground is for a one-shot offline management command.
	NoBackground bool
}

// Open builds the daemon. The caller must Close it.
func Open(ctx context.Context, opt Options) (*Daemon, error) {
	cfg := opt.Config
	if cfg == nil {
		return nil, errors.New("daemon: Config is required")
	}
	if len(cfg.Mounts) == 0 {
		return nil, errors.New("daemon: the config declares no mounts")
	}
	if opt.MountIndex < 0 || opt.MountIndex >= len(cfg.Mounts) {
		return nil, fmt.Errorf("daemon: mount index %d is out of range (%d configured)", opt.MountIndex, len(cfg.Mounts))
	}
	d := &Daemon{Config: cfg, version: opt.Version, started: time.Now(),
		Providers: map[string]provider.Provider{}, CallStats: map[string]*provider.Stats{}}
	if opt.RequireOwner {
		cacheDir := cfg.Cache.Dir
		if cacheDir == "" {
			cacheDir = config.ExpandHome("~/.cache/cloudfs")
		}
		j, err := journal.Open(journal.Options{Dir: filepath.Join(cacheDir, "journal"), Durability: journal.Durability(cfg.Journal.Durability)})
		if err != nil {
			return nil, err
		}
		if !j.Owner() {
			j.Close()
			return nil, errors.New("daemon: storage is owned by another process; use its control endpoint")
		}
		d.Journal = j
		d.closers = append(d.closers, j.Close)
	}

	// Proxy routing first: every provider's HTTP client goes through it.
	pm, err := buildProxy(cfg)
	if err != nil {
		d.Close()
		return nil, err
	}
	d.Proxy = pm
	if !opt.NoBackground {
		pm.StartHealthChecks(ctx)
	}
	d.closers = append(d.closers, func() error { pm.Stop(); return nil })

	// The matrix is read lazily: providers are built just below, and a bucket
	// is not created until the first request through it.
	d.Limiters = buildLimiters(cfg, func(remote string) (provider.Caps, bool) {
		p, ok := d.Providers[remote]
		if !ok {
			return provider.Caps{}, false
		}
		return p.Capabilities(), true
	})

	// Providers.
	secrets := config.NewSecretStore(cfg)
	for name, rc := range cfg.Remotes {
		resolved, err := secrets.ResolveRemote(rc)
		if err != nil {
			d.Close()
			return nil, fmt.Errorf("daemon: remote %q: %w", name, err)
		}
		p, closeProvider, err := buildProvider(name, resolved, pm, d.Limiters)
		if err != nil {
			d.Close()
			return nil, err
		}
		d.closers = append(d.closers, closeProvider)
		if setter, ok := p.(provider.TokenPersistenceSetter); ok && cfg.SourcePath != "" {
			setter.SetTokenPersister(config.TokenPersister(cfg, name))
		}
		st := provider.NewStats()
		d.CallStats[name] = st
		d.Providers[name] = provider.Instrument(p, st)
	}

	// Storage layers.
	cacheDir := cfg.Cache.Dir
	if cacheDir == "" {
		cacheDir = config.ExpandHome("~/.cache/cloudfs")
	}
	store, err := meta.Open(filepath.Join(cacheDir, "meta.db"), meta.Options{})
	if err != nil {
		d.Close()
		return nil, err
	}
	d.Meta = store
	d.closers = append(d.closers, store.Close)

	ca, err := cache.New(cache.Options{
		Dir:          filepath.Join(cacheDir, "blocks"),
		BlockSize:    int64(cfg.Cache.BlockSize),
		SubBlockSize: int64(cfg.Cache.SubBlockSize),
		WriteBehind:  int64(cfg.Cache.WriteBehind),
		MaxBytes:     int64(cfg.Cache.MaxSize),
		MinFree:      int64(cfg.Cache.MinFree),
		MaxAge:       cfg.Cache.MaxAge,
	})
	if err != nil {
		d.Close()
		return nil, err
	}
	d.Cache = ca

	// VFS mounts.
	mountCfg := cfg.Mounts[opt.MountIndex]
	bindings, err := remoteAccountBindings(cfg)
	if err != nil {
		d.Close()
		return nil, err
	}
	mounts, err := buildMounts(mountCfg, d.Providers, bindings)
	if err != nil {
		d.Close()
		return nil, err
	}
	d.closers = append(d.closers, ca.Close)
	prefetch := prefetchDepth()
	if opt.NoBackground {
		prefetch = 0
	}
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca, Mounts: mounts,
		DefaultDirTTL:   5 * time.Minute,
		AttrTTL:         30 * time.Second,
		NegativeTTL:     5 * time.Second,
		ReadAheadBlocks: readAheadBlocks(),
		PrefetchDepth:   prefetch,
		WriteSettle:     2 * time.Second,
	})
	if err != nil {
		d.Close()
		return nil, err
	}
	d.FS = fsys
	d.DropCaches = fsys.DropCaches
	// Hydration merges a whole file at once; it waits for the reads the
	// kernel is blocked on rather than sharing the disk with them.
	ca.SetBusy(fsys.Busy)
	d.closers = append(d.closers, fsys.Close)

	// Remotes with a change feed follow the provider within a poll interval;
	// the rest rely on the directory TTL. Polling a full listing on a
	// rate-limited drive is exactly the pattern that trips risk control, so
	// only delta-capable remotes are polled.
	d.Refresher = vfs.NewRefresher(fsys, time.Minute)
	d.closers = append(d.closers, func() error { d.Refresher.Stop(); return nil })

	if !opt.SkipWrite {
		j := d.Journal
		if j == nil {
			j, err = journal.Open(journal.Options{
				Dir:        filepath.Join(cacheDir, "journal"),
				Durability: journal.Durability(cfg.Journal.Durability),
			})
			if err != nil {
				d.Close()
				return nil, err
			}
			d.Journal = j
			d.closers = append(d.closers, j.Close)
		}

		j.SetSpaceReserver(ca.ReserveDisk)

		// Recover before accepting new work: anything interrupted by a crash
		// goes back on the queue, and partial writes are cleaned up.
		rec, err := j.Recover(ctx)
		if err != nil {
			d.Close()
			return nil, fmt.Errorf("daemon: journal recovery: %w", err)
		}
		if len(rec.Lost) > 0 && j.Owner() {
			// The tree must stop pointing at data that is not there; the
			// dead letter keeps what is left of it.
			fsys.RepairLost(ctx, j, rec.Lost)
		}
		if !j.Owner() {
			// Another process runs this queue. A second uploader would claim
			// its rows and send them through this process's own backend
			// objects — and a `cloudfs status` that quietly uploads files, or
			// takes rows with it when it exits, is not a status command.
			fsys.SetWriteBackend(j, nil)
			return d, nil
		}
		fsys.SetWriteBackend(j, nil)
		if err := fsys.RecoverUploadCleanups(ctx); err != nil {
			d.Close()
			return nil, fmt.Errorf("daemon: local upload cleanup recovery: %w", err)
		}
		if err := fsys.RecoverPublications(ctx, j); err != nil {
			d.Close()
			return nil, fmt.Errorf("daemon: local publication recovery: %w", err)
		}
		// A server-side copy interrupted between the provider's answer and the
		// local record leaves a question — does the destination exist? — that
		// only the provider can answer. Asking now is what keeps a restarted
		// daemon from either losing the object or making a second one. A
		// destination that cannot be reached must not stop the mount: the
		// intent stays and the warning says why.
		// The failure is not fatal and is not swallowed either: the VFS records
		// it, and status reports it as an unresolved copy that may exist on the
		// account.
		_ = fsys.ReconcileServerCopies(ctx)

		up, err := upload.New(upload.Options{
			Journal:   j,
			Providers: func(remote string) (provider.Provider, bool) { p, ok := d.Providers[remote]; return p, ok },
			Limiters:  d.Limiters,
			Workers:   2,
			// The backend knows how many parallel uploads it tolerates; a
			// LAN box takes several, a rate-limited drive one or two. An
			// explicit setting in the config wins.
			WorkersFor: func(remote string) int {
				if rc, ok := cfg.Remotes[remote]; ok && rc.UploadWorkers > 0 {
					return rc.UploadWorkers
				}
				if p, ok := d.Providers[remote]; ok {
					return p.Capabilities().UploadParallel
				}
				return 0
			},
			Hooks: fsys.UploadHooks(),
		})
		if err != nil {
			d.Close()
			return nil, err
		}
		d.Uploader = up
		fsys.SetWriteBackend(j, up)
		if !opt.NoBackground {
			up.Start(ctx)
		}
		d.closers = append(d.closers, func() error { up.Stop(); return nil })
	}
	if !opt.NoBackground {
		d.Refresher.Start(ctx)
		fsys.StartPins(ctx, time.Minute)
		fsys.StartCopies(ctx, time.Minute)
		d.closers = append(d.closers, func() error { fsys.StopCopies(); return nil })
	}
	return d, nil
}

// Close releases every component in reverse order.
func (d *Daemon) Close() error {
	var firstErr error
	for i := len(d.closers) - 1; i >= 0; i-- {
		if err := d.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	d.closers = nil
	return firstErr
}

// Collector builds the status collector for this daemon.
func (d *Daemon) Collector() *control.Collector {
	remotes := make([]string, 0, len(d.Providers))
	for name := range d.Providers {
		remotes = append(remotes, name)
	}
	sort.Strings(remotes)
	var flush func(context.Context) (journal.Stats, error)
	var cancelUpload func(context.Context, string) (journal.State, error)
	if d.Uploader != nil {
		flush = d.Uploader.Flush
		cancelUpload = d.Uploader.Cancel
	}
	return &control.Collector{
		Config:  d.Config,
		Version: d.version, Started: d.started,
		FS: d.FS, Journal: d.Journal, Cache: d.Cache,
		Proxy: d.Proxy, Limiters: d.Limiters, Remotes: remotes,
		CallStats:     d.CallStats,
		DropCaches:    d.DropCaches,
		FlushUploads:  flush,
		CancelUpload:  cancelUpload,
		ResumeUpload:  d.FS.ResumeUpload,
		DiscardUpload: d.FS.DiscardUpload,
		CacheMaxBytes: int64(d.Config.Cache.MaxSize),
		FreeSpace:     cache.FreeSpace,
	}
}

// Doctor builds the diagnostic runner for this daemon.
func (d *Daemon) Doctor(fuseSupported func() (bool, string)) *control.Doctor {
	cacheDir := d.Config.Cache.Dir
	return &control.Doctor{
		Config:        d.Config,
		CacheDir:      filepath.Join(cacheDir, "blocks"),
		Journal:       d.Journal,
		Meta:          d.Meta,
		Cache:         d.Cache,
		Proxy:         d.Proxy,
		MinFree:       int64(d.Config.Cache.MinFree),
		FreeSpace:     cache.FreeSpace,
		FUSESupported: fuseSupported,
	}
}

func buildProxy(cfg *config.Config) (*proxy.Manager, error) {
	var outbounds []proxy.Outbound
	for _, o := range cfg.Proxy.Outbounds {
		outbounds = append(outbounds, proxy.Outbound{Name: o.Name, Type: o.Type, Addr: o.Addr})
	}
	var groups []proxy.Group
	for _, g := range cfg.Proxy.Groups {
		groups = append(groups, proxy.Group{
			Name: g.Name, Type: proxy.GroupType(g.Type), Members: g.Members,
			CheckURL: g.CheckURL, Interval: g.Interval, Timeout: g.Timeout,
		})
	}
	return proxy.NewManager(proxy.ManagerOptions{
		Outbounds: outbounds, Groups: groups, Rules: cfg.Proxy.Rules,
	})
}

// buildLimiters seeds each remote's buckets, in this order of precedence:
// the QPS written in the config, then the backend's own recommendation from
// its capability matrix, then a conservative built-in default.
//
// capsFor is consulted lazily, because a bucket is created on first use and
// the providers exist by then. Reading the matrix matters: a backend on the
// local network recommends tens of requests per second, and holding it to the
// default meant for a rate-limited public drive turns a directory walk into
// seconds of pure waiting.
func buildLimiters(cfg *config.Config, capsFor func(remote string) (provider.Caps, bool)) *ratelimit.Registry {
	overrides := map[string]config.QPS{}
	for name, r := range cfg.Remotes {
		if r.QPS != nil {
			overrides[name] = *r.QPS
		}
	}
	return ratelimit.NewRegistry(func(k ratelimit.Key) ratelimit.Options {
		rate := defaultRate(k.Class)
		if capsFor != nil {
			if c, ok := capsFor(k.Remote); ok {
				if r := qpsForClass(c.QPS, k.Class); r > 0 {
					rate = r
				}
			}
		}
		if q, ok := overrides[k.Remote]; ok {
			if r := qpsForClass(provider.QPS{Meta: q.Meta, Download: q.Download, Upload: q.Upload}, k.Class); r > 0 {
				rate = r
			}
		}
		return ratelimit.Options{Rate: rate, MinRate: rate / 8}
	}, ratelimit.BreakerOptions{Threshold: 3, Window: 10 * time.Minute, Cooldown: 30 * time.Minute})
}

// qpsForClass picks the recommendation for one request class.
func qpsForClass(q provider.QPS, c ratelimit.Class) float64 {
	switch c {
	case ratelimit.Meta:
		return q.Meta
	case ratelimit.Download:
		return q.Download
	case ratelimit.Upload:
		return q.Upload
	}
	return 0
}

func defaultRate(c ratelimit.Class) float64 {
	switch c {
	case ratelimit.Meta:
		return 4
	case ratelimit.Download:
		return 8
	case ratelimit.Upload:
		return 2
	}
	return 4
}

// buildProvider constructs one backend, wiring it to the shared proxy routing
// and rate limiter through the config the factory receives.
func buildProvider(name string, rc config.Remote, pm *proxy.Manager, limiters *ratelimit.Registry) (provider.Provider, func() error, error) {
	cfg := map[string]any{}
	for k, v := range rc.Extra {
		cfg[k] = v
	}
	// The factory may build its own client; give it a preconfigured one so the
	// proxy rules and limiter apply without every driver repeating the wiring.
	httpClient := pm.Client(rc.Proxy, 0)
	cfg["_http_client"] = httpx.New(httpx.Options{
		HTTP:     httpClient,
		Remote:   name,
		Limiters: limiters,
		Policy:   retry.Policy{Backoff: retry.DefaultBackoff, MaxAttempts: 4},
	})
	// Backends that do not speak HTTP cannot inherit the proxy rules and the
	// limiter through the client above, so hand them over directly.
	cfg[provider.ConfigLimiters] = limiters
	cfg[provider.ConfigDialer] = provider.DialFunc(
		func(ctx context.Context, network, addr string) (net.Conn, error) {
			return pm.DialContext(ctx, network, addr, rc.Proxy)
		})
	p, err := provider.New(rc.Type, name, cfg)
	if err != nil {
		httpClient.CloseIdleConnections()
		return nil, nil, fmt.Errorf("daemon: remote %q: %w", name, err)
	}
	return p, func() error {
		httpClient.CloseIdleConnections()
		if closer, ok := p.(interface{ Close() error }); ok {
			return closer.Close()
		}
		return nil
	}, nil
}

func remoteAccountBindings(cfg *config.Config) (map[string]string, error) {
	out := make(map[string]string, len(cfg.Remotes))
	for name, remote := range cfg.Remotes {
		binding, err := config.EffectiveAccountBinding(remote)
		if err != nil {
			return nil, fmt.Errorf("daemon: remote %q account binding: %w", name, err)
		}
		out[name] = binding
	}
	return out, nil
}

func buildMounts(m config.Mount, providers map[string]provider.Provider, bindings map[string]string) ([]vfs.Mount, error) {
	var out []vfs.Mount
	prefixes := make([]string, 0, len(m.Layout))
	for prefix := range m.Layout {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	for _, prefix := range prefixes {
		l := m.Layout[prefix]
		p, ok := providers[l.Remote]
		if !ok {
			return nil, fmt.Errorf("daemon: mount %s references unknown remote %q", prefix, l.Remote)
		}
		root := l.Root
		if root == "" {
			root = "/"
		}
		out = append(out, vfs.Mount{
			Prefix: prefix, Remote: l.Remote, RootID: root, AccountBinding: bindings[l.Remote], Provider: p,
			Mode: l.Mode, DirTTL: l.DirTTL, Pin: l.Pin,
		})
	}
	if len(out) == 0 {
		return nil, errors.New("daemon: the mount has an empty layout")
	}
	return out, nil
}

// EnsureDir creates a directory the daemon needs.
func EnsureDir(p string) error {
	if err := os.MkdirAll(p, 0o700); err != nil {
		return fmt.Errorf("daemon: create %s: %w", p, err)
	}
	return nil
}

// readAheadBlocks is the sequential prefetch window in blocks; the
// CLOUDFS_READAHEAD_BLOCKS environment variable overrides it for experiments.
func readAheadBlocks() int {
	if v := os.Getenv("CLOUDFS_READAHEAD_BLOCKS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 16
}

// prefetchDepth is how many directory levels below a listed directory are
// listed ahead of time; CLOUDFS_PREFETCH_DEPTH overrides it for experiments.
func prefetchDepth() int {
	if v := os.Getenv("CLOUDFS_PREFETCH_DEPTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 2
}
