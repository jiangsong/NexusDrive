// Package daemon assembles the whole system from a config file: providers,
// proxy routing, rate limiters, metadata store, block cache, write journal,
// uploader, VFS, FUSE mounts, the MCP server and the control endpoints. It is
// the one place that knows how the pieces fit together.
package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/embed"
	"cloudfs/internal/export"
	"cloudfs/internal/index"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/proxy"
	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/pool"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
	"cloudfs/internal/service"
	"cloudfs/internal/trigger"
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
	// Export runs `cloudfs export` jobs out of its own queue.
	Export *export.Manager
	// Agent is agent.db: the principals, sessions and audit trail of the
	// agents that talk to this mount over MCP. Every process opens it, so a
	// stdio MCP server beside the daemon records its own calls; only the
	// owner runs audit retention.
	Agent *agent.Store
	// Sessions maps MCP connections to sessions in Agent.
	Sessions *agent.Sessions
	// Preimages keeps what MCP write tools overwrite, so a session can be
	// rolled back; the owner recovers orphan blobs at start and collects
	// expired sessions hourly.
	Preimages *agent.Preimages
	// Trigger runs the triggers[] and agents[] rules over the change
	// stream. Only the owner of agent.db with background work enabled
	// builds one, and only when the config has a rule or an agent; every
	// other process leaves it nil.
	Trigger *trigger.Engine
	// Index is the content indexer over index.db, nil unless index.enabled.
	// Every process opens it so a stdio MCP server beside the daemon can
	// search; only the owner of index.db runs the extraction worker.
	Index    *index.Indexer
	Proxy    *proxy.Manager
	Limiters *ratelimit.Registry
	// Providers maps remote name to backend.
	Providers map[string]provider.Provider
	// Pools maps the name of each pool remote to the pool behind it, for
	// the control plane; Providers holds the same object instrumented.
	Pools map[string]*pool.Pool
	// CallStats maps remote name to its provider call counter. Every remote
	// request passes through one, which is what makes "a warm listing costs
	// zero calls" a measurable claim rather than an assertion.
	CallStats map[string]*provider.Stats
	// DropCaches empties the caches for a cold measurement. It defaults to
	// the VFS's own; a mounting process wraps it to also drop the kernel's.
	DropCaches func(ctx context.Context) (int, error)

	// mcp is what the control plane reports about the MCP HTTP transport.
	mcp mcpHTTP

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
		Providers: map[string]provider.Provider{}, CallStats: map[string]*provider.Stats{}, Pools: map[string]*pool.Pool{}}
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

	// Providers. Pools are composite backends over other remotes, so they
	// are assembled in a second pass once every member exists.
	secrets := config.NewSecretStore(cfg)
	cacheDir := cfg.Cache.Dir
	if cacheDir == "" {
		cacheDir = config.ExpandHome("~/.cache/cloudfs")
	}
	for name, rc := range cfg.Remotes {
		if rc.Type == config.PoolType {
			continue
		}
		resolved, err := secrets.ResolveRemote(rc)
		if err != nil {
			d.Close()
			return nil, fmt.Errorf("daemon: remote %q: %w", name, err)
		}
		p, effectiveConns, closeProvider, err := buildProvider(name, resolved, pm, d.Limiters)
		if err != nil {
			d.Close()
			return nil, err
		}
		d.closers = append(d.closers, closeProvider)
		if setter, ok := p.(provider.TokenPersistenceSetter); ok && cfg.SourcePath != "" {
			setter.SetTokenPersister(config.TokenPersister(cfg, name))
		}
		// Caps must agree with the transport buildProvider already bounded,
		// so this reuses its effectiveConns rather than re-deriving the
		// fallback here. Goes before Instrument so both wrappers compose.
		p = provider.WithMaxConns(p, effectiveConns)
		st := provider.NewStats()
		d.CallStats[name] = st
		d.Providers[name] = provider.Instrument(p, st)
	}
	for name, rc := range cfg.Remotes {
		if rc.Type != config.PoolType {
			continue
		}
		p, err := buildPool(name, rc, cfg, d.Providers, filepath.Join(cacheDir, "pool"))
		if err != nil {
			d.Close()
			return nil, err
		}
		d.closers = append(d.closers, p.Close)
		st := provider.NewStats()
		d.CallStats[name] = st
		d.Providers[name] = provider.Instrument(p, st)
		d.Pools[name] = p
	}

	// Storage layers.
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
	mounts, err := buildMounts(mountCfg, d.Providers, bindings, cfg.Cache.Policy, int64(cfg.Cache.BlockSize))
	if err != nil {
		d.Close()
		return nil, err
	}
	d.closers = append(d.closers, ca.Close)
	prefetch := prefetchDepth()
	if opt.NoBackground {
		prefetch = 0
	}
	readaheadReq, err := readaheadRequest()
	if err != nil {
		d.Close()
		return nil, err
	}
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca, Mounts: mounts,
		DefaultDirTTL:    5 * time.Minute,
		AttrTTL:          30 * time.Second,
		NegativeTTL:      5 * time.Second,
		ReadAheadBlocks:  readAheadBlocks(),
		ReadaheadRequest: readaheadReq,
		PrefetchDepth:    prefetch,
		WriteSettle:      2 * time.Second,
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
	// Same reason for the pools: a rebalance move is a whole file in each
	// direction and must stand aside while a read is waiting.
	for _, pl := range d.Pools {
		pl.SetBusy(fsys.Busy)
	}
	d.closers = append(d.closers, fsys.Close)

	// Exports copy bytes out of the mount onto ordinary local storage. They
	// keep their own queue next to the cache: an export stages no blob and
	// owns no upload row, so putting it in the journal's schema would make
	// every daemon migrate a database it otherwise never touches.
	exportStore, err := export.OpenStore(cacheDir)
	if err != nil {
		d.Close()
		return nil, err
	}
	exports, err := export.New(export.Options{Store: exportStore, FS: fsys, Config: cfg.Export})
	if err != nil {
		exportStore.Close()
		d.Close()
		return nil, err
	}
	d.Export = exports
	// A copy to a drive is background work: it stands aside while the kernel
	// is waiting on a read, the same way hydration and rebalancing do.
	exports.SetBusy(fsys.Busy)
	d.closers = append(d.closers, exports.Close)

	// The content index keeps its own database next to the cache for the
	// same reason exports do: meta has one writer and FLUSH waits on its
	// lock, so extraction must not queue behind it. With index.enabled
	// false nothing is opened and no index.db appears.
	if cfg.Index.Enabled {
		embedder, err := buildEmbedder(cfg, secrets, d.Proxy)
		if err != nil {
			d.Close()
			return nil, err
		}
		indexStore, err := index.OpenStore(cacheDir)
		if err != nil {
			d.Close()
			return nil, fmt.Errorf("daemon: index store: %w", err)
		}
		x, err := index.New(index.Options{
			FS: fsys, Store: indexStore, Config: cfg.Index, Embedder: embedder,
			Unofficial: func(remote string) bool {
				p, ok := d.Providers[remote]
				return ok && p.Capabilities().Tier == provider.TierUnofficial
			},
		})
		if err != nil {
			indexStore.Close()
			d.Close()
			return nil, err
		}
		d.Index = x
		// The indexer subscribes to the VFS change feed, so it closes
		// before the VFS does: closers run in reverse and fsys.Close was
		// appended above.
		d.closers = append(d.closers, indexStore.Close, x.Close)
		if indexStore.Owner() && !opt.NoBackground {
			x.Start(ctx)
		}
	}

	// Remotes with a change feed follow the provider within a poll interval;
	// the rest rely on the directory TTL. Polling a full listing on a
	// rate-limited drive is exactly the pattern that trips risk control, so
	// only delta-capable remotes are polled.
	d.Refresher = vfs.NewRefresher(fsys, time.Minute)
	d.closers = append(d.closers, func() error { d.Refresher.Stop(); return nil })

	// The agent store opens before the non-owner early return below: a
	// stdio MCP process started while `cloudfs mount` owns the journal is
	// exactly the process whose calls must land in the shared audit trail.
	agentStore, err := agent.Open(filepath.Join(cacheDir, "agent"))
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("daemon: agent store: %w", err)
	}
	d.Agent = agentStore
	d.Sessions = agent.NewSessions(agentStore, agent.SessionOptions{Idle: cfg.MCP.Session.Idle})
	d.closers = append(d.closers, agentStore.Close)
	preimages, err := agent.NewPreimages(agentStore, filepath.Join(agentStore.Dir(), "preimages"), ca, int64(cfg.MCP.Session.MaxPreimageBytes))
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("daemon: %w", err)
	}
	d.Preimages = preimages
	if agentStore.Owner() {
		// A crash between linking a preimage and recording its row leaves
		// an orphan blob; sweeping them belongs to the one process that
		// knows no capture is in flight, before any tool runs.
		if n, err := preimages.Recover(ctx); err != nil {
			slog.Warn("daemon: preimage recovery failed", "err", err)
		} else if n > 0 {
			slog.Info("daemon: removed orphan preimages", "count", n)
		}
	}
	if agentStore.Owner() && !opt.NoBackground {
		// Retention purges on start and then daily; RunAuditRetention itself
		// returns at once in a non-owner, so two processes never race. The
		// preimage GC follows the same pattern hourly with the session
		// retention.
		retentionCtx, stopRetention := context.WithCancel(ctx)
		go agentStore.RunAuditRetention(retentionCtx, cfg.MCP.Audit.Retain, 24*time.Hour)
		go preimages.RunGC(retentionCtx, cfg.MCP.Session.Retain, time.Hour)
		d.closers = append(d.closers, func() error { stopRetention(); return nil })
	}

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
		for _, pl := range d.Pools {
			pl.Start(ctx)
		}
		d.Refresher.Start(ctx)
		fsys.StartPins(ctx, time.Minute)
		fsys.StartCopies(ctx, time.Minute)
		d.closers = append(d.closers, func() error { fsys.StopCopies(); return nil })
		exports.Start(ctx, time.Minute)
		// The manager is always installed so `warm --all` can hand a pass
		// to it; it only lists on its own when search.crawl.enabled.
		fsys.StartCrawl(ctx, vfs.CrawlOptionsFrom(cfg.Search.Crawl))
		d.closers = append(d.closers, func() error { fsys.StopCrawl(); return nil })
	}
	// The trigger engine starts last: the queue it drains is durable, and
	// every action it runs may read the VFS, so nothing above may still be
	// half-built. The owner check is what keeps a stdio MCP process beside
	// the daemon from running the same rules a second time.
	if agentStore.Owner() && !opt.NoBackground && len(cfg.Triggers)+len(cfg.Agents) > 0 {
		eng := trigger.New(trigger.Options{
			FS: fsys, Rules: cfg.Triggers, Agents: cfg.Agents, Store: agentStore,
			Secrets: secrets.Get, Proxy: pm,
		})
		if err := eng.Run(ctx); err != nil {
			d.Close()
			return nil, fmt.Errorf("daemon: triggers: %w", err)
		}
		d.Trigger = eng
		d.closers = append(d.closers, func() error { eng.Close(); return nil })
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
	col := &control.Collector{
		Config:  d.Config,
		Version: d.version, Started: d.started,
		FS: d.FS, Journal: d.Journal, Cache: d.Cache,
		Proxy: d.Proxy, Limiters: d.Limiters, Remotes: remotes,
		Providers: d.Providers,
		Pools:     d.Pools,

		// A saved proxy section takes effect without a restart: every
		// provider's client asks the manager per request.
		ReloadProxy: func(p config.Proxy) error {
			return d.Proxy.Reload(control.ProxyManagerOptions(p))
		},
		CallStats:     d.CallStats,
		DropCaches:    d.DropCaches,
		FlushUploads:  flush,
		CancelUpload:  cancelUpload,
		ResumeUpload:  d.FS.ResumeUpload,
		DiscardUpload: d.FS.DiscardUpload,
		CacheMaxBytes: int64(d.Config.Cache.MaxSize),
		FreeSpace:     cache.FreeSpace,
	}
	// A nil manager has to stay a nil interface: the export routes answer 503
	// on the strength of that field alone.
	if d.Export != nil {
		col.Export = d.Export
	}
	col.Agent = d.agentView()
	col.MCP = d.mcpView()
	col.Index = d.indexView()
	col.Trigger = d.triggerView()
	// Everything that reads the configuration reads it through the collector's
	// published view, never through the pointer this function was called with.
	// The control plane republishes a copy after every edit, so a hook that
	// captured the pointer would answer "unknown remote" for the account that
	// had just been added through the API — which is the add-a-drive flow. The
	// same applies to the hooks that read mounts rather than accounts: the
	// diagnostics and the service installer are built here, after the view
	// exists, for exactly that reason.
	view := func() *config.Config { return col.ConfigView() }
	// The runner without a FUSE probe: the mounting process, which knows the
	// kernel, replaces it with d.Doctor(col.ConfigView, fusefs.Supported).
	col.Doctor = d.Doctor(view, nil)
	col.Service = d.serviceControl(view)
	col.Auth = AuthStarterFor(view)
	col.AccountQuota = func(ctx context.Context, name string) (provider.Quota, bool, error) {
		return AccountQuota(ctx, view(), name)
	}
	col.CheckAccount = func(ctx context.Context, name string) error {
		return SanitizeAccountError(CheckAccount(ctx, view(), name))
	}
	return col
}

// serviceRuntime builds the Runtime the service hooks drive. It is a variable
// so a test can hand back a Runtime whose Run, Mounted and Unmount are captured
// instead of the machine's real service manager.
var serviceRuntime = func(out io.Writer) (service.Runtime, error) {
	rt, err := service.Real()
	if err != nil {
		return service.Runtime{}, err
	}
	if out != nil {
		rt.Out = out
	}
	return rt, nil
}

// serviceControl adapts internal/service to the control plane, so the settings
// page can install or remove the mount supervisor without a terminal. It is nil
// unless this daemon was started from a config file with a mount: the unit runs
// `cloudfs mount` against exactly that file. The daemon may install and
// uninstall as well as report status — a person who reached the dashboard has
// already shown they can reach the machine.
func (d *Daemon) serviceControl(view func() *config.Config) *control.ServiceControl {
	if view == nil {
		started := d.Config
		view = func() *config.Config { return started }
	}
	cfg := view()
	if cfg == nil || cfg.SourcePath == "" || len(cfg.Mounts) == 0 {
		return nil
	}
	// The file a unit points at cannot change under a running daemon; the
	// mount inside it can, so every use below reads view() rather than cfg.
	configPath := cfg.SourcePath
	newRuntime := serviceRuntime
	return &control.ServiceControl{
		Supported: func() (bool, string) {
			rt, err := service.Real()
			if err != nil {
				return false, err.Error()
			}
			return rt.Supported()
		},
		Installed: func() (bool, error) {
			rt, err := service.Real()
			if err != nil {
				return false, err
			}
			return rt.Installed()
		},
		Status: func() (string, error) {
			var buf bytes.Buffer
			rt, err := newRuntime(&buf)
			if err != nil {
				return "", err
			}
			if err := rt.Status(); err != nil {
				return "", err
			}
			return buf.String(), nil
		},
		Install: func() error {
			rt, err := newRuntime(io.Discard)
			if err != nil {
				return err
			}
			return rt.Install(view(), configPath)
		},
		Uninstall: func() error {
			rt, err := newRuntime(io.Discard)
			if err != nil {
				return err
			}
			return rt.Uninstall(view(), configPath)
		},
	}
}

// Doctor builds the diagnostic runner for this daemon.
//
// view is how the runner reads the configuration. It is a function rather than
// the pointer this daemon started with because the control plane republishes a
// copy after every edit: a runner holding the original would check the accounts
// that existed at start-up and report a clean bill of health for the drives
// added since, never having looked at them.
func (d *Daemon) Doctor(view func() *config.Config, fuseSupported func() (bool, string)) *control.Doctor {
	cacheDir := d.Config.Cache.Dir
	if view == nil {
		started := d.Config
		view = func() *config.Config { return started }
	}
	return &control.Doctor{
		Config:          view,
		CacheDir:        filepath.Join(cacheDir, "blocks"),
		Journal:         d.Journal,
		Meta:            d.Meta,
		Cache:           d.Cache,
		Proxy:           d.Proxy,
		MinFree:         int64(d.Config.Cache.MinFree),
		FreeSpace:       cache.FreeSpace,
		FUSESupported:   fuseSupported,
		Pools:           d.Pools,
		MemberProviders: d.Providers,
		HoldMaxBytes:    d.holdBudget(),
		Index:           d.indexView(),
		Agent:           d.Agent,
		AgentDir:        filepath.Join(cacheDir, "agent"),
	}
}

// holdBudget is the largest hold budget any pool declares, for doctor.
func (d *Daemon) holdBudget() int64 {
	var max int64
	for _, p := range d.Config.Pools {
		if int64(p.HoldMaxBytes) > max {
			max = int64(p.HoldMaxBytes)
		}
	}
	return max
}

// buildEmbedder builds the embedding client index.embedding names, or nil
// for provider none. The api_key reference is resolved through the secret
// store the way a remote's credentials are; a reference that cannot be
// resolved fails the start with the same "run cloudfs index auth" advice,
// since an index that silently ran keyword-only would hide the mistake.
// The client goes through the proxy manager so proxy rules apply to the
// endpoint. A non-owner process (a stdio MCP server) builds one too: it
// runs no worker, but its searches embed their queries.
func buildEmbedder(cfg *config.Config, secrets *config.SecretStore, pm *proxy.Manager) (embed.Embedder, error) {
	ec := cfg.Index.Embedding
	if !ec.Enabled() {
		return nil, nil
	}
	apiKey := ""
	if ec.APIKey != "" {
		v, err := secrets.Get(ec.APIKey)
		if err != nil {
			return nil, fmt.Errorf("daemon: index.embedding.api_key: %w", err)
		}
		apiKey = v
	}
	c, err := embed.New(embed.Options{Config: ec, APIKey: apiKey, Proxy: pm})
	if err != nil {
		return nil, fmt.Errorf("daemon: index.embedding: %w", err)
	}
	if c == nil {
		return nil, nil
	}
	return c, nil
}

func buildProxy(cfg *config.Config) (*proxy.Manager, error) {
	return proxy.NewManager(control.ProxyManagerOptions(cfg.Proxy))
}

// authStarter adapts the daemon's authorization flows to the control server's
// AuthStarter, so the control package need not import daemon. The daemon saves
// the credential in every flow; nothing about a token reaches the caller.
// AuthStarterFor builds the control plane's authorization hooks from a
// configuration alone. It takes no daemon because none is needed: adding and
// authorizing accounts is what someone does *before* there is a filesystem to
// mount, and the setup flow serves the control plane with nothing else running.
//
// It takes a function rather than a configuration because the control plane
// republishes the configuration after every edit, as a copy. A hook that
// captured the pointer would answer "unknown remote" for the account that had
// just been added through the API — which is the add-a-drive flow itself.
func AuthStarterFor(view func() *config.Config) *control.AuthStarter {
	if view == nil {
		return nil
	}
	if cfg := view(); cfg == nil || cfg.SourcePath == "" {
		return nil
	}
	return &control.AuthStarter{
		Supported:    SupportsDaemonAuth,
		FillClientID: FillBuiltinClientID,
		OAuth: func(ctx context.Context, name string, present func(url string)) (string, func(context.Context) error, error) {
			p, wait, err := StartOAuthFlow(ctx, view(), name, "", func(_ context.Context, url string) error {
				present(url)
				return nil
			})
			if err != nil {
				return "", nil, err
			}
			return p.RedirectURI, wait, nil
		},
		Device: func(ctx context.Context, name string, present func(qr string), scanned func()) (func(context.Context) error, error) {
			if cfg := view(); cfg != nil {
				if remote, ok := cfg.Remotes[name]; ok && remote.Type == "quark" {
					return StartDeviceQuarkFlow(ctx, cfg, name, func(_ context.Context, qr string) error {
						present(qr)
						return nil
					})
				}
			}
			return StartDevice115Flow(ctx, view(), name, func(_ context.Context, qr string) error {
				present(qr)
				return nil
			}, scanned)
		},
	}
}

// buildLimiters seeds each remote's buckets, in this order of precedence:
// the QPS written in the config, then the backend's own recommendation from
// its capability matrix, then a conservative built-in default.
//
// capsFor is consulted lazily, because a bucket is created on first use and
// the providers exist by then: holding a fast local backend to the default
// meant for a rate-limited public drive turns a directory walk into a wait.
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
			if r := qpsForClass(provider.QPS{Meta: q.Meta, Download: q.Download, Upload: q.Upload, Transfer: q.Transfer}, k.Class); r > 0 {
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
	case ratelimit.Transfer:
		return q.Transfer
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
	case ratelimit.Transfer:
		return 0 // the built-in default: alias onto Download (ratelimit.Registry.Limiter)
	}
	return 4
}

// buildProvider constructs one backend, wiring it to the shared proxy routing
// and rate limiter through the config the factory receives. The returned
// effective value is the connection limit it bounded the transport to —
// callers must feed it back into provider.WithMaxConns rather than
// re-deriving the fallback from rc.MaxConns alone, or a driver that leaves
// MaxConnsPerHost undeclared could have Capabilities() (0, "unbounded")
// silently disagree with the transport (8, actually bounded).
func buildProvider(name string, rc config.Remote, pm *proxy.Manager, limiters *ratelimit.Registry) (p provider.Provider, effective int, closeFn func() error, err error) {
	cfg := map[string]any{}
	for k, v := range rc.Extra {
		cfg[k] = v
	}
	// The factory may build its own client; give it a preconfigured one so the
	// proxy rules and limiter apply without every driver repeating the wiring.
	httpClient, setConns := pm.ClientWithLimit(rc.Proxy, 0)
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
	p, err = provider.New(rc.Type, name, cfg)
	if err != nil {
		httpClient.CloseIdleConnections()
		return nil, 0, nil, fmt.Errorf("daemon: remote %q: %w", name, err)
	}
	// The connection limit follows an explicit per-remote override, else the
	// backend's own recommendation, else the historical default of 8. This
	// is the one place that fallback is computed; both the transport
	// (setConns, right below) and Caps (the caller's provider.WithMaxConns)
	// must use this same number.
	effective = rc.MaxConns
	if effective <= 0 {
		effective = p.Capabilities().MaxConnsPerHost
	}
	if effective <= 0 {
		effective = 8
	}
	setConns(effective)
	return p, effective, func() error {
		httpClient.CloseIdleConnections()
		if closer, ok := p.(interface{ Close() error }); ok {
			return closer.Close()
		}
		return nil
	}, nil
}

// buildPool assembles one pool remote over its already-built members. The
// members are handed over instrumented, so each drive's call counter shows
// the traffic the pool sends it.
func buildPool(name string, rc config.Remote, cfg *config.Config, providers map[string]provider.Provider, stateDir string) (*pool.Pool, error) {
	settings, ok := cfg.Pools[rc.PoolOf()]
	if !ok {
		return nil, fmt.Errorf("daemon: remote %q references unknown pool %q", name, rc.PoolOf())
	}
	members := map[string]provider.Provider{}
	for _, m := range settings.Members {
		mp, ok := providers[m.Remote]
		if !ok {
			return nil, fmt.Errorf("daemon: pool %q member %q was not built", name, m.Remote)
		}
		members[m.Remote] = mp
	}
	domains, err := poolDomains(settings, cfg)
	if err != nil {
		return nil, fmt.Errorf("daemon: pool %q: %w", name, err)
	}
	p, err := provider.New(config.PoolType, name, map[string]any{
		pool.ConfigMembers:  members,
		pool.ConfigSettings: settings,
		pool.ConfigStateDir: stateDir,
		pool.ConfigDomains:  domains,
	})
	if err != nil {
		return nil, fmt.Errorf("daemon: remote %q: %w", name, err)
	}
	pl, ok := p.(*pool.Pool)
	if !ok {
		return nil, fmt.Errorf("daemon: remote %q: the pool factory returned a %T", name, p)
	}
	return pl, nil
}

// poolDomains maps each member remote to its failure-domain identity, the
// thing replicas of one file are spread across. The daemon computes it
// because it alone resolves account bindings and remote types; the pool
// only compares the strings.
func poolDomains(settings config.Pool, cfg *config.Config) (map[string]string, error) {
	out := make(map[string]string, len(settings.Members))
	for _, m := range settings.Members {
		switch settings.FailureDomain {
		case config.FailureDomainMember:
			out[m.Remote] = m.Remote
			continue
		}
		rc, ok := cfg.Remotes[m.Remote]
		if !ok {
			return nil, fmt.Errorf("member %q is not a configured remote", m.Remote)
		}
		if settings.FailureDomain == config.FailureDomainProvider {
			out[m.Remote] = rc.Type
			continue
		}
		// FailureDomainAccount, the default: two members of the same
		// account share a quota and a ban, so they are one domain.
		binding, err := config.EffectiveAccountBinding(rc)
		if err != nil {
			return nil, fmt.Errorf("member %q account binding: %w", m.Remote, err)
		}
		out[m.Remote] = binding
	}
	return out, nil
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

// EnsureDir creates a directory the daemon needs.
func EnsureDir(p string) error {
	if err := os.MkdirAll(p, 0o700); err != nil {
		return fmt.Errorf("daemon: create %s: %w", p, err)
	}
	return nil
}
