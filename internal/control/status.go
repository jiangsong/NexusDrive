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
	"cloudfs/internal/upload"
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
	// Blocked counts queued rows that cannot run: they wait on a directory
	// creation that is dead, cancelled or gone. The journal has counted them
	// for the flush path all along; nothing showed the number, so a queue
	// that was busy and going nowhere looked exactly like a slow one.
	Blocked int `json:"blocked"`
	// InFlightBytes is the size of the rows being transferred right now.
	InFlightBytes int64 `json:"in_flight_bytes"`
	// Batch is the overall progress of the current burst of work; see
	// UploadBatch.
	Batch UploadBatch `json:"batch"`
}

// UploadBatch is upload.Progress on the wire: the burst of work the queue is
// currently getting through, from the moment it stopped being empty to the
// moment it is empty again.
//
// The first seven fields are a published contract with
// internal/control/web/transfer_progress.js, which reads them off the status
// document by name; upload_batch_keys_test.go pins them. The rest were added
// afterwards and the page ignores what it does not know.
//
// The arithmetic lives in internal/upload, beside the counters it is made of.
// This is only the rendering: times as RFC 3339, the estimate in seconds.
type UploadBatch struct {
	Active     bool   `json:"active"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	FilesTotal int64  `json:"files_total"`
	FilesDone  int64  `json:"files_done"`
	BytesTotal int64  `json:"bytes_total"`
	BytesDone  int64  `json:"bytes_done"`
	// Percent is how far along the batch is, computed once by the daemon
	// (upload.PercentOf) rather than by each reader from the four numbers
	// above. The console prefers it and keeps its own formula only as a
	// fallback for an older daemon: two hand-written copies of one formula
	// drift, and a browser and a terminal disagreeing by a digit about the
	// same batch is the failure this whole surface exists to remove.
	Percent float64 `json:"percent"`
	// Seq changes when a new burst begins, so a reader following the queue
	// knows the figures it holds belong to a batch that has ended.
	Seq int64 `json:"seq"`
	// Resumed marks a batch that was already queued when the daemon started:
	// its done counts run from the restart, not from the copy.
	Resumed bool `json:"resumed,omitempty"`
	// Rate is bytes per second and ETASeconds what is left at that rate.
	// Both are zero on a batch that is not moving bytes.
	Rate       float64 `json:"rate,omitempty"`
	ETASeconds float64 `json:"eta_seconds,omitempty"`
	// FilesDoneTotal and BytesDoneTotal are the daemon's lifetime counts.
	// The Prometheus completion counters are fed from these and never from
	// the batch delta, which returns to zero at every boundary.
	FilesDoneTotal int64 `json:"files_done_total"`
	BytesDoneTotal int64 `json:"bytes_done_total"`
}

// uploadBatchOf renders one progress sample for the wire.
func uploadBatchOf(p upload.Progress) UploadBatch {
	b := UploadBatch{
		Active: p.Active, Seq: p.Seq, Resumed: p.Resumed, Percent: p.Percent(),
		FilesTotal: p.FilesTotal, FilesDone: p.FilesDone,
		BytesTotal: p.BytesTotal, BytesDone: p.BytesDone,
		Rate: p.Rate, ETASeconds: p.ETA.Seconds(),
		FilesDoneTotal: p.FilesDoneTotal, BytesDoneTotal: p.BytesDoneTotal,
	}
	if !p.StartedAt.IsZero() {
		b.StartedAt = p.StartedAt.UTC().Format(time.RFC3339)
	}
	if !p.FinishedAt.IsZero() {
		b.FinishedAt = p.FinishedAt.UTC().Format(time.RFC3339)
	}
	return b
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
	// UploadProgress reports the upload queue's overall progress — the
	// uploader's own last sample, which costs no query. nil when there is no
	// uploader, and the batch is then empty rather than invented here.
	//
	// It stays an injected function rather than a reach through FS so that a
	// CLI or metrics test can stand a collector up with no filesystem behind
	// it, which is most of what this package's tests do.
	UploadProgress func() upload.Progress
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
				// From this reading rather than from the batch sample, so the
				// counts on one page agree with each other.
				Blocked: js.Blocked,
			}
			if js.OldestAge > 0 {
				s.Uploads.OldestAge = js.OldestAge.Round(time.Second).String()
			}
		}
	}
	if c.UploadProgress != nil {
		p := c.UploadProgress()
		s.Uploads.Batch = uploadBatchOf(p)
		s.Uploads.InFlightBytes = p.InFlightBytes
		if c.Journal == nil {
			// No queue reading of our own: the sample is all there is.
			s.Uploads.Blocked = p.Blocked
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
	if s.Uploads.Blocked > 0 {
		add("status.upload_blocked", s.Uploads.Blocked)
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
	// An upload batch in flight is deliberately NOT a warning. A copy into the
	// mount returns long before its bytes do, so a batch is the most ordinary
	// state this filesystem has; listing it here would put a warning on every
	// healthy copy and teach people that the warning list is noise. Warnings
	// are for what is wrong — a queue that cannot move (status.blocked above),
	// dead letters, a tripped breaker.
	//
	// The batch still has to be discoverable, and it is: `cloudfs status`
	// prints it as a line of its own and `cloudfs uploads watch` follows it,
	// the console draws it on #/transfers, and the agent turn-start hook says
	// it in a sentence. Those surfaces describe a state; this one raises an
	// alarm, and the two should not be confused.
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
