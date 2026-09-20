// Package control is the operator surface: a status snapshot, Prometheus
// metrics, health endpoints and the doctor checks that turn a vague "it is
// slow" into a specific cause (docs/DESIGN.md §4.8).
package control

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/i18n"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/proxy"
	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/pool"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
)

// Status is the whole-daemon snapshot behind `cloudfs status` and /status.
type Status struct {
	Version   string        `json:"version"`
	Uptime    time.Duration `json:"uptime_ns"`
	UptimeStr string        `json:"uptime"`

	Mounts  []MountStatus `json:"mounts"`
	Cache   CacheStatus   `json:"cache"`
	Uploads UploadStatus  `json:"uploads"`
	Meta    MetaStatus    `json:"meta"`
	// Crawl is the background directory crawler's progress, and Coverage
	// the listed/known directory pair the search answer also carries
	// (Meta.Dirs excludes the root and Meta.CompleteDirs does not, so the
	// pair is what a coverage card must show).
	Crawl    vfs.CrawlProgress `json:"crawl"`
	Coverage meta.Coverage     `json:"coverage"`
	Proxies  []ProxyStatus     `json:"proxies,omitempty"`
	Remotes  []RemoteStatus    `json:"remotes,omitempty"`
	// Fuse is present when a kernel mount is being served.
	Fuse *FuseStatus `json:"fuse,omitempty"`
	// Durability is what close(2) promises: "power" or "crash".
	Durability string `json:"durability,omitempty"`
	// Agent is present when this daemon keeps an agent store: how many MCP
	// sessions are active and whether the audit trail is keeping up.
	Agent *AgentStatus `json:"agent,omitempty"`
	// Index is present when this daemon has a content index: the counts
	// the overview cards show and the cloudfs_index_* metrics are read from.
	Index *IndexStatus `json:"index,omitempty"`
	// Triggers is present when this daemon runs the trigger engine: the
	// queue counts the console badge and the cloudfs_trigger_* metrics show.
	Triggers *TriggerStatus `json:"triggers,omitempty"`
	// Warnings names conditions an operator should act on, most urgent first.
	Warnings []string `json:"warnings,omitempty"`
}

// FuseStatus counts the requests the kernel sent the mount. A warm tree
// that the kernel answers by itself shows up here as zero, which is the
// number a cache is measured by.
type FuseStatus struct {
	Ops       map[string]int64 `json:"ops"`
	ReadBytes int64            `json:"read_bytes"`
	// ReadSizes is a cumulative histogram of READ sizes keyed by upper
	// bound in bytes ("4096" … "+Inf").
	ReadSizes map[string]int64 `json:"read_sizes"`
}

// MountStatus describes one mounted remote.
type MountStatus struct {
	Prefix string `json:"prefix"`
	Remote string `json:"remote"`
	Mode   string `json:"mode"`
}

// CacheStatus reports block-cache health.
type CacheStatus struct {
	Blocks             int     `json:"blocks"`
	Bytes              int64   `json:"bytes"`
	BytesHuman         string  `json:"bytes_human"`
	MaxBytes           int64   `json:"max_bytes"`
	MinFree            int64   `json:"min_free"`
	HitRatio           float64 `json:"hit_ratio"`
	Hits               int64   `json:"hits"`
	Misses             int64   `json:"misses"`
	Evictions          int64   `json:"evictions"`
	HydratedFiles      int     `json:"hydrated_files"`
	PinnedBlocks       int     `json:"pinned_blocks"`
	WholeBytes         int64   `json:"whole_bytes"`
	ReservedBytes      int64   `json:"reserved_bytes"`
	WriteReservedBytes int64   `json:"write_reserved_bytes"`
	LeasedBytes        int64   `json:"leased_bytes"`
	OrphanBytes        int64   `json:"orphan_bytes"`
	FreeBytes          int64   `json:"free_bytes"`
}

// UploadStatus reports the write queue.
type UploadStatus struct {
	Pending       int    `json:"pending"`
	Uploading     int    `json:"uploading"`
	Dead          int    `json:"dead"`
	Done          int    `json:"done"`
	Cancelling    int    `json:"cancelling"`
	Cancelled     int    `json:"cancelled"`
	Purging       int    `json:"purging"`
	RetainedBytes int64  `json:"retained_bytes"`
	QueuedBytes   int64  `json:"queued_bytes"`
	OldestAge     string `json:"oldest_age,omitempty"`
	OldestAgeNS   int64  `json:"oldest_age_ns"`
	// Batch is the overall progress of the current burst of work; see
	// UploadBatch.
	Batch UploadBatch `json:"batch"`
}

// MetaStatus reports the metadata cache.
type MetaStatus struct {
	Nodes         int64 `json:"nodes"`
	Dirs          int64 `json:"dirs"`
	CompleteDirs  int64 `json:"complete_dirs"`
	NegativeCache int64 `json:"negative_cache"`
	Pins          int64 `json:"pins"`
	// LastCrawl is when a listing last extended the index, by the crawler
	// or by a readdir; zero when nothing has been listed.
	LastCrawl time.Time `json:"last_crawl"`
}

// ProxyStatus is one outbound's health.
type ProxyStatus struct {
	Name      string `json:"name"`
	Healthy   bool   `json:"healthy"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// RemoteStatus reports per-remote rate limiting and circuit-breaker state.
type RemoteStatus struct {
	Remote   string  `json:"remote"`
	MetaRate float64 `json:"meta_rate"`
	DownRate float64 `json:"download_rate"`
	UpRate   float64 `json:"upload_rate"`
	// TransferRate is the CDN byte-stream bucket's current adaptive rate.
	// When a remote has not opted a Transfer class in (see ratelimit.Class),
	// this mirrors DownRate, since the two share one bucket.
	TransferRate float64 `json:"transfer_rate"`
	BreakerOpen  bool    `json:"breaker_open"`
	// BreakerUntil says when the remote becomes usable again.
	BreakerUntil string `json:"breaker_until,omitempty"`
	// State is the remote's reachability as seen from the calls made to
	// it: up, degraded, down, out, disabled or draining. The breaker says
	// whether the account is being protected; this says whether the drive
	// answers at all — a remote whose API is unreachable never trips the
	// breaker and would otherwise look healthy.
	State     string `json:"state"`
	LastOK    string `json:"last_ok,omitempty"`
	LastError string `json:"last_error,omitempty"`
	DownSince string `json:"down_since,omitempty"`
	// Calls counts backend requests by operation since the daemon started,
	// and Bytes the payload those requests moved.
	Calls map[string]int64 `json:"calls,omitempty"`
	// Nanos is the time spent inside those calls, by operation.
	Nanos      map[string]int64 `json:"nanos,omitempty"`
	CallsTotal int64            `json:"calls_total"`
	ReadBytes  int64            `json:"read_bytes"`
	WriteBytes int64            `json:"write_bytes"`
}

// Collector gathers status from the running components.
type Collector struct {
	// Config, when set, is the configuration this daemon was started from.
	// The accounts endpoint needs its path to add a remote; nothing else here
	// reads it, and no credential is ever served from it.
	//
	// The editing routes republish it after every write, so a handler must
	// take it through ConfigView rather than read the field: the value is
	// replaced whole, never mutated in place, and cfgMu makes the swap and
	// the read agree on which whole.
	Config   *config.Config
	cfgMu    sync.RWMutex
	Version  string
	Started  time.Time
	FS       *vfs.FS
	Journal  *journal.Journal
	Cache    *cache.Cache
	Proxy    *proxy.Manager
	Limiters *ratelimit.Registry
	// FreeSpace measures what the cache filesystem has free.
	FreeSpace func(string) (int64, error)
	// Remotes lists the configured remote names, for limiter reporting.
	Remotes []string
	// CallStats maps remote name to its provider call counter.
	CallStats map[string]*provider.Stats
	// DropCaches, when set, empties the caches on request (POST /cache/drop)
	// and returns how many files were dropped. Benchmarks use it to get a
	// cold start without restarting the daemon.
	DropCaches func(ctx context.Context) (int, error)
	// UploadTotals reports how many rows the uploader has finished since
	// the daemon started and their bytes; nil when there is no uploader.
	UploadTotals func() (files, bytes int64)
	batch        uploadBatchTracker
	// FlushUploads waits for delayed and in-flight uploads, without starting
	// extra workers or bypassing the provider's backoff policy.
	FlushUploads  func(ctx context.Context) (journal.Stats, error)
	CancelUpload  func(ctx context.Context, id string) (journal.State, error)
	ResumeUpload  func(ctx context.Context, id string, confirm bool) error
	DiscardUpload func(ctx context.Context, id string, confirm bool) error
	// FuseStats, when set, reports the kernel request counters of the
	// live mount.
	FuseStats func() FuseStatus
	// Pools are the running storage pools by the name of the remote that
	// exposes each; nil when none is configured.
	Pools map[string]*pool.Pool
	// Export runs `cloudfs export` jobs. nil on a daemon that owns no export
	// queue, and the export routes then answer 503 rather than pretending.
	Export ExportManager
	// Providers are the live backends by remote name, for the capability
	// table an account page shows. Nothing is served from them but Caps.
	Providers map[string]provider.Provider
	// CheckAccount performs one sanitised root listing of a configured
	// remote; nil means the daemon does not offer it.
	CheckAccount func(ctx context.Context, name string) error
	// AccountQuota reports one account's own space, and whether the backend
	// could say. It is separate from CheckAccount because a backend that
	// reports no quota is working perfectly well; conflating the two would
	// turn "dropbox does not publish a figure" into "this drive is broken".
	AccountQuota func(ctx context.Context, name string) (provider.Quota, bool, error)
	// ReloadProxy applies a saved proxy section to the running daemon. nil
	// means a proxy change needs a restart to take effect.
	ReloadProxy func(p config.Proxy) error
	// Auth drives daemon-side OAuth and device logins; nil means the browser
	// authorization flow is unavailable and credentials go through the CLI.
	Auth *AuthStarter
	// Doctor, when set, answers /doctor/run and /doctor/fix. It is the same
	// runner `cloudfs doctor` uses, on the live daemon's own stores — which
	// is the only place it can run while this process owns the journal.
	Doctor *Doctor
	// Lifecycle, when set, lets the control plane restart this process. nil in
	// a process that has nothing to restart (an offline management command).
	Lifecycle *Lifecycle
	// Service, when set, installs and removes the per-user mount supervisor.
	// nil when this daemon does not offer service management.
	Service *ServiceControl
	// Agent, when set, serves /audit and /sessions and feeds the audit and
	// session events. nil on a daemon without agent.db, and those routes
	// then answer 503 rather than pretending.
	Agent AgentView
	// MCP, when set, serves /mcp/connect and /mcp/tokens. nil on a daemon
	// without agent.db, and those routes answer 503.
	MCP MCPView
	// Index, when set, serves /index/* and feeds the index progress event.
	// nil on a daemon started with index.enabled false; /index/status then
	// answers {"enabled":false} and the other index routes 404.
	Index IndexControl
	// Trigger, when set, serves /triggers/* and counts the delivery queue
	// for /status. nil on a daemon that runs no engine (no rules, or not
	// the owner of agent.db); /triggers then answers {"enabled":false} and
	// the other trigger routes 404.
	Trigger TriggerControl
	// Memory, when set, serves /memory/*: the same store the memory_* MCP
	// tools use. nil in a process without one; /memory/agents then
	// answers {"enabled":false} and the other memory routes 404.
	Memory MemoryControl
	// HookStore and ReadHeat serve the agent-client hooks
	// (docs/agent-first-design.md §7): the change record the turn-start
	// hook reads and the heat table the read hook reports to. nil on a
	// daemon without agent.db; the hook routes then answer empty.
	HookStore HookStore
	ReadHeat  HookReadHeat
	// HeatStore serves GET /agent/heat; nil answers {"enabled": false}.
	HeatStore HeatStore
	// Changes serves GET /changes; nil answers {"enabled": false}.
	Changes ChangeStore
	// RenderLinks and RenderBase serve POST /share/render-link: the token
	// table of the LAN render service and its base URL; nil / "" when
	// share.render is off, and the route says so.
	RenderLinks *RenderLinks
	RenderBase  string
	Now         func() time.Time
}

// ConfigView returns the configuration as it stands now. The returned value
// is never modified afterwards, so a caller may hold it for the length of a
// request without holding a lock.
func (c *Collector) ConfigView() *config.Config {
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	return c.Config
}

// publishConfigView swaps in a new configuration view.
func (c *Collector) publishConfigView(next *config.Config) { c.PublishConfigView(next) }

// PublishConfigView replaces the published configuration whole. It is exported
// because the daemon has to be able to hand a reloaded configuration in, and
// because everything that reads one — the account check, the authorization
// hooks, the quota probe — must read it through ConfigView rather than capture
// the pointer it was built with. Capturing is how an account added through the
// API became invisible to the very next call about it.
func (c *Collector) PublishConfigView(next *config.Config) {
	c.cfgMu.Lock()
	c.Config = next
	c.cfgMu.Unlock()
}

// Collect builds a Status snapshot.
func (c *Collector) Collect(ctx context.Context, lang i18n.Lang) Status {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	s := Status{Version: c.Version}
	if !c.Started.IsZero() {
		s.Uptime = now().Sub(c.Started)
		s.UptimeStr = s.Uptime.Round(time.Second).String()
	}
	if c.FuseStats != nil {
		fst := c.FuseStats()
		s.Fuse = &fst
	}
	if c.Journal != nil {
		s.Durability = string(c.Journal.Durability())
	}
	if c.Agent != nil {
		s.Agent = &AgentStatus{Workspace: c.Agent.Workspace(), AuditWriteFailures: c.Agent.AuditWriteFailures()}
		if sum, err := c.Agent.Summary(ctx); err == nil {
			s.Agent.ActiveSessions = sum.Active
		}
	}
	if c.Index != nil {
		s.Index = &IndexStatus{Enabled: true}
		if st, err := c.Index.Status(ctx, ""); err == nil {
			s.Index = indexStatusOf(st)
		}
	}
	if c.Trigger != nil {
		s.Triggers = &TriggerStatus{Enabled: true}
		if pending, dead, err := c.Trigger.Counts(ctx); err == nil {
			s.Triggers.Pending, s.Triggers.Dead = pending, dead
		}
	}

	if c.FS != nil {
		for _, m := range c.FS.Mounts() {
			s.Mounts = append(s.Mounts, MountStatus{Prefix: m.Prefix, Remote: m.Remote, Mode: string(m.Mode)})
		}
		if st, err := c.FS.Meta().Stats(ctx); err == nil {
			s.Meta = MetaStatus{
				Nodes: st.Nodes, Dirs: st.Dirs, CompleteDirs: st.CompleteDs,
				NegativeCache: st.Absent, Pins: st.Pins, LastCrawl: st.LastCrawl,
			}
		}
		s.Crawl = c.FS.CrawlProgress()
		if cov, err := c.FS.Meta().Coverage(ctx); err == nil {
			s.Coverage = cov
		}
	}

	if c.Cache != nil {
		cs := c.Cache.Stats()
		// The live budget, not the configured one: the console can change
		// it while the daemon runs.
		maxBytes, minFree := c.Cache.Budget()
		s.Cache = CacheStatus{
			Blocks: cs.Blocks, Bytes: cs.Bytes, BytesHuman: humanBytes(cs.Bytes),
			MaxBytes: maxBytes, MinFree: minFree, HitRatio: cs.HitRatio(), Hits: cs.Hits,
			Misses: cs.Misses, Evictions: cs.Evictions,
			HydratedFiles: cs.HydratedFiles, PinnedBlocks: cs.PinnedBlocks,
			WholeBytes: cs.WholeBytes, ReservedBytes: cs.ReservedBytes, WriteReservedBytes: cs.WriteReservedBytes, LeasedBytes: cs.LeasedBytes, OrphanBytes: cs.OrphanBytes,
		}
		if c.FreeSpace != nil {
			if free, err := c.FreeSpace(c.Cache.Dir()); err == nil {
				s.Cache.FreeBytes = free
			}
		}
	}

	if c.Journal != nil {
		if js, err := c.Journal.Stats(ctx); err == nil {
			s.Uploads = UploadStatus{
				Pending: js.Pending, Uploading: js.Uploading, Dead: js.Dead, Done: js.Done,
				Cancelling: js.Cancelling, Cancelled: js.Cancelled, Purging: js.Purging, RetainedBytes: js.RetainedBytes,
				QueuedBytes: js.Bytes, OldestAgeNS: int64(js.OldestAge),
			}
			if js.OldestAge > 0 {
				s.Uploads.OldestAge = js.OldestAge.Round(time.Second).String()
			}
			if c.UploadTotals != nil {
				files, bytes := c.UploadTotals()
				s.Uploads.Batch = c.batch.observe(now(), js.Pending+js.Uploading, js.Bytes, files, bytes)
			}
		}
	}

	if c.Proxy != nil {
		for _, h := range c.Proxy.Health() {
			s.Proxies = append(s.Proxies, ProxyStatus{
				Name: h.Name, Healthy: h.Healthy,
				LatencyMS: h.Latency.Milliseconds(), Error: h.Err,
			})
		}
		sort.Slice(s.Proxies, func(i, j int) bool { return s.Proxies[i].Name < s.Proxies[j].Name })
	}

	for _, r := range c.Remotes {
		rs := RemoteStatus{Remote: r}
		if c.Limiters != nil {
			rs.MetaRate = c.Limiters.Limiter(ratelimit.Key{Remote: r, Class: ratelimit.Meta}).Rate()
			rs.DownRate = c.Limiters.Limiter(ratelimit.Key{Remote: r, Class: ratelimit.Download}).Rate()
			rs.UpRate = c.Limiters.Limiter(ratelimit.Key{Remote: r, Class: ratelimit.Upload}).Rate()
			rs.TransferRate = c.Limiters.Limiter(ratelimit.Key{Remote: r, Class: ratelimit.Transfer}).Rate()
			b := c.Limiters.Breaker(r, "")
			rs.BreakerOpen = b.Open()
			if until := b.OpenUntil(); !until.IsZero() {
				rs.BreakerUntil = until.Format(time.RFC3339)
			}
		}
		rs.State = string(provider.HealthUp)
		if st := c.CallStats[r]; st != nil {
			rs.Calls, rs.Nanos, rs.CallsTotal = st.Report()
			rs.ReadBytes = st.ReadBytes()
			rs.WriteBytes = st.WriteBytes()
			h := st.Health()
			rs.State = string(h.State)
			if !h.LastOK.IsZero() {
				rs.LastOK = h.LastOK.Format(time.RFC3339)
			}
			if !h.DownSince.IsZero() {
				rs.DownSince = h.DownSince.Format(time.RFC3339)
			}
			rs.LastError = h.LastError
		}
		if rs.BreakerOpen && rs.State == string(provider.HealthUp) {
			rs.State = string(provider.HealthDown)
		}
		s.Remotes = append(s.Remotes, rs)
	}
	sort.Slice(s.Remotes, func(i, j int) bool { return s.Remotes[i].Remote < s.Remotes[j].Remote })

	warnings(&s, lang)
	if c.FS != nil {
		// These arrive as prose from the filesystem: no catalog entry, so
		// they are shown exactly as they came.
		if warning := c.FS.PinWarning(); warning != "" {
			s.Warnings = append(s.Warnings, warning)
		}
		if warning := c.FS.CopyWarning(); warning != "" {
			s.Warnings = append(s.Warnings, warning)
		}
	}
	return s
}

// warnings turns the snapshot into the short list an operator should act on,
// rendered in lang. Status carries the finished sentences and not the catalog
// keys behind them, so the language has to be decided before Collect runs —
// FetchStatusInLanguage negotiates it rather than translating strings back.
func warnings(s *Status, lang i18n.Lang) {
	add := func(key string, args ...any) {
		s.Warnings = append(s.Warnings, i18n.T(lang, key, args...))
	}
	if s.Uploads.Purging > 0 {
		add("status.purging", s.Uploads.Purging)
	}
	if s.Uploads.Cancelled+s.Uploads.Cancelling > 0 {
		add("status.cancelled", s.Uploads.Cancelling, s.Uploads.Cancelled)
	}
	if s.Uploads.Dead > 0 {
		add("status.dead", s.Uploads.Dead)
	}
	for _, r := range s.Remotes {
		if r.BreakerOpen {
			add("status.breaker_open", r.Remote, r.BreakerUntil)
		}
	}
	if s.Uploads.OldestAgeNS > int64(30*time.Minute) {
		add("status.queue_slow", s.Uploads.OldestAge)
	}
	if s.Cache.MaxBytes > 0 && s.Cache.Bytes > s.Cache.MaxBytes*9/10 {
		add("status.cache_budget", humanBytes(s.Cache.Bytes), humanBytes(s.Cache.MaxBytes))
	}
	if s.Cache.FreeBytes > 0 && s.Cache.FreeBytes < 1<<30 {
		add("status.free_low", humanBytes(s.Cache.FreeBytes))
	}
	for _, p := range s.Proxies {
		if !p.Healthy {
			add("status.proxy_unhealthy", p.Name, p.Error)
		}
	}
}

// humanBytes renders a byte count at the largest unit that keeps it readable.
func humanBytes(b int64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1f TiB", float64(b)/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}
