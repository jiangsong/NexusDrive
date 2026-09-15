package control

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloudfs/internal/i18n"
	"cloudfs/internal/vfs"
)

// Server exposes health, status and metrics over HTTP. It binds to a loopback
// address; nothing here is safe to expose beyond the machine.
type Server struct {
	collector *Collector
	mux       *http.ServeMux
	srv       *http.Server
	auth      *AuthStarter
	authReg   *authRegistry
	assets    map[string]asset
	// draining is set true once a restart is accepted; while it is set, the
	// guard turns away every mutation so nothing changes state the imminent
	// teardown is about to drop.
	draining atomic.Bool
	// reloadMu serializes the read-modify-write in reloadConfigView, so two
	// edits landing together cannot publish a view that is missing one of
	// them.
	reloadMu sync.Mutex
}

// NewServer builds the control HTTP server.
//
// Every route that changes anything, or that names something about the
// configuration, goes through privateRequest. The four that do not —
// /healthz, /readyz, /status and /metrics — are read-only, carry no
// credential-shaped field (Status is checked for that in tests), and are
// reachable only by processes on this machine, which can already read far
// more from /proc. Keeping them open is what lets a monitoring agent scrape
// them without a custom header. Anything new here is guarded unless it is
// argued into that list.
func NewServer(c *Collector) *Server {
	s := &Server{collector: c, mux: http.NewServeMux(), auth: c.Auth, authReg: newAuthRegistry()}
	for _, r := range s.routes() {
		s.mux.HandleFunc(r.pattern, controlOrigin(r.handler))
	}
	return s
}

// controlOrigin tags the request so every change a control route makes
// through the VFS (/fs/mkdir, /fs/rename, /fs/delete, /copy, an upload
// discard) is reported with vfs.OriginAPI rather than as background work.
func controlOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(vfs.WithOrigin(r.Context(), "control")))
	}
}

// route is one registered pattern. The table exists so a test can walk every
// route and prove the guard is on it: /cache/drop went unguarded for as long
// as it did because nothing enumerated the routes and asked.
type route struct {
	pattern string
	handler http.HandlerFunc
	// open marks the few read-only routes that deliberately skip
	// privateRequest; see NewServer.
	open bool
	// probe is a concrete path under a prefix pattern for the guard test to
	// hit, since "/cache/" itself is not a route anyone calls.
	probe string
}

func (s *Server) routes() []route {
	return []route{
		{pattern: "/healthz", handler: s.healthz, open: true},
		{pattern: "/readyz", handler: s.readyz, open: true},
		{pattern: "/status", handler: s.status, open: true},
		{pattern: "/metrics", handler: s.metrics, open: true},
		{pattern: "/cache/drop", handler: s.dropCaches},
		{pattern: "/cache/", handler: s.manageCache, probe: "/cache/gc"},
		{pattern: "/uploads", handler: s.uploads},
		{pattern: "/uploads/", handler: s.uploads, probe: "/uploads/retry"},
		{pattern: "/copy", handler: s.copyFile},
		{pattern: "/copies", handler: s.copies},
		{pattern: "/copies/", handler: s.mutateCopy, probe: "/copies/retry"},
		{pattern: "/export", handler: s.startExport},
		{pattern: "/exports", handler: s.exports},
		{pattern: "/exports/", handler: s.exportByPath, probe: "/exports/pause"},
		{pattern: "/accounts", handler: s.accounts},
		{pattern: "/accounts/", handler: s.accountByName, probe: "/accounts/demo"},
		{pattern: "/fs/list", handler: s.fsList},
		{pattern: "/fs/stat", handler: s.fsStat},
		{pattern: "/fs/preview", handler: s.fsPreview},
		{pattern: "/fs/download-url", handler: s.fsDownloadURL},
		{pattern: "/fs/mkdir", handler: s.fsMkdir},
		{pattern: "/fs/rename", handler: s.fsRename},
		{pattern: "/fs/delete", handler: s.fsDelete},
		{pattern: "/search", handler: s.search},
		{pattern: "/doctor/run", handler: s.doctorRun},
		{pattern: "/doctor/fix", handler: s.doctorFix},
		{pattern: "/events", handler: s.events},
		{pattern: "/proxy/explain", handler: s.proxyExplain},
		{pattern: "/proxy/check", handler: s.proxyCheck},
		{pattern: "/proxy/config", handler: s.proxyConfig},
		{pattern: "/mounts", handler: s.mounts},
		{pattern: "/pool/status", handler: s.poolStatus},
		{pattern: "/pool/create", handler: s.poolCreate},
		{pattern: "/pool/config", handler: s.poolConfig},
		{pattern: "/pool/preview", handler: s.poolPreview},
		{pattern: "/pool/members", handler: s.poolMembers},
		{pattern: "/pool/members/state", handler: s.poolMemberState},
		{pattern: "/pool/members/drain", handler: s.poolMemberState},
		{pattern: "/pool/members/remove", handler: s.poolMemberRemove},
		{pattern: "/pool/repair", handler: s.poolWork},
		{pattern: "/pool/scrub", handler: s.poolWork},
		{pattern: "/pool/rebuild", handler: s.poolWork},
		{pattern: "/pool/rebalance", handler: s.poolRebalance},
		{pattern: "/pool/divergences", handler: s.poolDivergences},
		{pattern: "/pool/join", handler: s.poolJoin},
		{pattern: "/daemon/restart", handler: s.daemonRestart},
		{pattern: "/service/status", handler: s.service},
		{pattern: "/service/install", handler: s.service},
		{pattern: "/service/uninstall", handler: s.service},
		{pattern: "/audit", handler: s.audit},
		{pattern: "/sessions", handler: s.sessions},
		{pattern: "/sessions/", handler: s.sessionByPath, probe: "/sessions/x/finish"},
		{pattern: "/mcp/connect", handler: s.mcpConnect},
		{pattern: "/mcp/tokens", handler: s.mcpTokens},
		{pattern: "/mcp/tokens/", handler: s.mcpTokenByPath, probe: "/mcp/tokens/x/revoke"},
		{pattern: "/index/status", handler: s.indexStatus},
		{pattern: "/index/rules", handler: s.indexRules},
		{pattern: "/index/add", handler: s.indexAdd},
		{pattern: "/index/remove", handler: s.indexRemove},
		{pattern: "/index/rebuild", handler: s.indexRebuild},
		{pattern: "/index/retry", handler: s.indexRetry},
		{pattern: "/index/failed", handler: s.indexFailed},
		{pattern: "/index/search", handler: s.indexSearch},
		{pattern: "/index/text", handler: s.indexText},
		{pattern: "/agent/prompt", handler: s.agentPrompt},
		{pattern: "/agent/endpoints", handler: s.agentEndpoints},
		{pattern: "/agent/invoke", handler: s.agentInvoke},
		{pattern: "/triggers", handler: s.triggers},
		{pattern: "/triggers/deliveries", handler: s.triggerDeliveries},
		{pattern: "/triggers/deliveries/", handler: s.triggerDeliveryByPath, probe: "/triggers/deliveries/1"},
		{pattern: "/triggers/test", handler: s.triggerTest},
		{pattern: "/triggers/retry", handler: s.triggerRetry},
	}
}

// EnableUI mounts the embedded web app. It must be called before the server
// starts. The app is a set of static assets under one origin that call the
// same control endpoints anything else does, so it adds no alternate
// privileged data path. Every path but "/" and the embedded assets is a 404,
// which is the contract the mux and its tests already keep.
func (s *Server) EnableUI() {
	s.assets = buildAssets()
	s.mux.HandleFunc("/", s.statusUI)
	s.mux.HandleFunc("/ui/", s.statusUI)
}

// enablePprof mounts the profiling handlers. It is called only from
// ListenAndServe, and only for a loopback address: the decision needs the
// address the daemon is about to bind, which NewServer does not have, and
// these handlers hand out the process's memory to whoever can reach them.
func (s *Server) enablePprof() {
	s.mux.HandleFunc("/debug/pprof/", pprof.Index)
	s.mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	s.mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	s.mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	s.mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}

// loopbackAddr reports whether addr binds only to this machine.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false // ":9101" listens on every interface
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// dropCaches empties the block cache and stales every listing. It is a POST
// because it changes what the next reads cost, and like every other mutation
// it takes the same-origin guard: a plain HTML form on any site could
// otherwise post here and make the next hour of reads cold.
func (s *Server) dropCaches(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		httpErrorT(w, r, http.StatusMethodNotAllowed, "err.use_post")
		return
	}
	if s.collector.DropCaches == nil {
		httpErrorT(w, r, http.StatusNotImplemented, "err.drop_unwired")
		return
	}
	n, err := s.collector.DropCaches(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"files_dropped": n})
}

// Handler exposes the mux for tests and embedding. Every path that serves
// this server goes through serveHandler, so a test reaching Handler() sees
// the same chain the daemon does.
func (s *Server) Handler() http.Handler { return s.serveHandler() }

// serveHandler is the one chain the daemon serves. Start, ListenAndServe and
// Handler must not assemble their own: a middleware added to one and missed by
// the others is invisible until a request in production behaves differently
// from the same request in a test.
func (s *Server) serveHandler() http.Handler {
	return s.languageBoundary(s.drainingGuard(s.mux))
}

// languageBoundary resolves the reader's language once and removes the
// parameter that carried it, so no handler has to know the page appends one.
func (s *Server) languageBoundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, resolveLang(r))
	})
}

// drainingGuard turns away mutations once a restart has been accepted. Reads
// still answer, so the UI can show that the daemon is on its way down.
func (s *Server) drainingGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.draining.Load() && r.Method != http.MethodGet && r.Method != http.MethodHead {
			httpErrorT(w, r, http.StatusServiceUnavailable, "err.daemon_restarting")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ListenAndServe starts the server on addr until ctx ends.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	if !loopbackAddr(addr) {
		return fmt.Errorf("control: TCP address must be loopback: %s", addr)
	}
	if os.Getenv("CLOUDFS_PPROF") == "1" && loopbackAddr(addr) {
		s.enablePprof()
	}
	s.srv = &http.Server{Addr: addr, Handler: s.serveHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
	}()
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintln(w, "ok")
}

// readyz reports whether the daemon can actually serve: the metadata store
// answers, the cache directory is writable and no remote is fully broken.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	// Read by a probe, not a person: the language never reaches anyone, and
	// English is what a log or an alert rule will be read against.
	st := s.collector.Collect(r.Context(), i18n.EN)
	w.Header().Set("Content-Type", "text/plain")
	if s.collector.FS == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "not ready: no filesystem mounted")
		return
	}
	if _, err := s.collector.FS.Meta().Stats(r.Context()); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "not ready: metadata store: %v\n", err)
		return
	}
	fmt.Fprintln(w, "ready")
	for _, warn := range st.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	st := s.collector.Collect(r.Context(), LangFrom(r))
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(st)
}

// metric is one exported sample.
type metric struct {
	name   string
	help   string
	typ    string
	labels map[string]string
	value  float64
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	st := s.collector.Collect(r.Context(), i18n.EN)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	writeMetrics(w, st)
}

// writeMetrics renders the snapshot in the Prometheus text format. Writing it
// by hand keeps the client library out of the dependency graph for what is a
// few dozen samples.
func writeMetrics(w interface{ Write([]byte) (int, error) }, st Status) {
	ms := []metric{
		{name: "cloudfs_cache_blocks", help: "Blocks currently in the local block cache", typ: "gauge", value: float64(st.Cache.Blocks)},
		{name: "cloudfs_cache_bytes", help: "Bytes held in the local block cache", typ: "gauge", value: float64(st.Cache.Bytes)},
		{name: "cloudfs_cache_max_bytes", help: "Configured block cache budget in bytes", typ: "gauge", value: float64(st.Cache.MaxBytes)},
		{name: "cloudfs_cache_free_bytes", help: "Free bytes on the cache filesystem", typ: "gauge", value: float64(st.Cache.FreeBytes)},
		{name: "cloudfs_cache_hits_total", help: "Block cache hits", typ: "counter", value: float64(st.Cache.Hits)},
		{name: "cloudfs_cache_misses_total", help: "Block cache misses", typ: "counter", value: float64(st.Cache.Misses)},
		{name: "cloudfs_cache_evictions_total", help: "Payload objects and abandoned temporaries evicted from the cache", typ: "counter", value: float64(st.Cache.Evictions)},
		{name: "cloudfs_cache_whole_bytes", help: "Unique complete-file bytes including open retired inodes", typ: "gauge", value: float64(st.Cache.WholeBytes)},
		{name: "cloudfs_cache_reserved_bytes", help: "Bytes reserved for active hydration or retired flushes", typ: "gauge", value: float64(st.Cache.ReservedBytes)},
		{name: "cloudfs_journal_write_reserved_bytes", help: "Disk headroom reserved for active journal write syscalls", typ: "gauge", value: float64(st.Cache.WriteReservedBytes)},
		{name: "cloudfs_cache_leased_bytes", help: "Complete-file bytes retained by open cache leases", typ: "gauge", value: float64(st.Cache.LeasedBytes)},
		{name: "cloudfs_cache_orphan_bytes", help: "Abandoned cache temporary bytes awaiting GC", typ: "gauge", value: float64(st.Cache.OrphanBytes)},
		{name: "cloudfs_cache_hit_ratio", help: "Block cache hit ratio since start", typ: "gauge", value: st.Cache.HitRatio},
		{name: "cloudfs_cache_hydrated_files", help: "Files fully present as a single cached file", typ: "gauge", value: float64(st.Cache.HydratedFiles)},
		{name: "cloudfs_cache_pinned_blocks", help: "Blocks exempt from eviction", typ: "gauge", value: float64(st.Cache.PinnedBlocks)},

		{name: "cloudfs_uploads_pending", help: "Uploads waiting in the journal", typ: "gauge", value: float64(st.Uploads.Pending)},
		{name: "cloudfs_uploads_cancelling", help: "Uploads waiting for cancellation acknowledgement", typ: "gauge", value: float64(st.Uploads.Cancelling)},
		{name: "cloudfs_uploads_cancelled", help: "Stopped uploads retaining local contents", typ: "gauge", value: float64(st.Uploads.Cancelled)},
		{name: "cloudfs_uploads_purging", help: "Uploads with unfinished local cleanup", typ: "gauge", value: float64(st.Uploads.Purging)},
		{name: "cloudfs_uploads_in_flight", help: "Uploads currently transferring", typ: "gauge", value: float64(st.Uploads.Uploading)},
		{name: "cloudfs_uploads_dead", help: "Uploads that failed permanently and kept their data", typ: "gauge", value: float64(st.Uploads.Dead)},
		{name: "cloudfs_uploads_queued_bytes", help: "Bytes waiting to be uploaded", typ: "gauge", value: float64(st.Uploads.QueuedBytes)},
		{name: "cloudfs_uploads_oldest_age_seconds", help: "Age of the oldest queued upload", typ: "gauge", value: time.Duration(st.Uploads.OldestAgeNS).Seconds()},

		{name: "cloudfs_meta_nodes", help: "Entries in the metadata cache", typ: "gauge", value: float64(st.Meta.Nodes)},
		{name: "cloudfs_meta_complete_dirs", help: "Directories with a complete cached listing", typ: "gauge", value: float64(st.Meta.CompleteDirs)},
		{name: "cloudfs_meta_negative_entries", help: "Live negative-cache entries", typ: "gauge", value: float64(st.Meta.NegativeCache)},
		{name: "cloudfs_pins", help: "Pinned paths", typ: "gauge", value: float64(st.Meta.Pins)},
		{name: "cloudfs_uptime_seconds", help: "Daemon uptime", typ: "gauge", value: time.Duration(st.Uptime).Seconds()},
	}
	if st.Agent != nil {
		ms = append(ms,
			metric{name: "cloudfs_agent_sessions_active", help: "MCP sessions currently active", typ: "gauge", value: float64(st.Agent.ActiveSessions)},
			metric{name: "cloudfs_audit_write_failures_total", help: "Audit rows that could not be written to agent.db since start", typ: "counter", value: float64(st.Agent.AuditWriteFailures)},
		)
	}
	if st.Index != nil {
		ms = append(ms,
			metric{name: "cloudfs_index_documents", help: "Documents in the content index, by state", typ: "gauge",
				labels: map[string]string{"state": "ok"}, value: float64(st.Index.Docs.OK)},
			metric{name: "cloudfs_index_documents", labels: map[string]string{"state": "dirty"}, value: float64(st.Index.Docs.Dirty)},
			metric{name: "cloudfs_index_documents", labels: map[string]string{"state": "failed"}, value: float64(st.Index.Docs.Failed)},
			metric{name: "cloudfs_index_pending", help: "Files waiting for text extraction", typ: "gauge", value: float64(st.Index.Pending)},
			metric{name: "cloudfs_index_chunks", help: "Chunks in the content index", typ: "gauge", value: float64(st.Index.Chunks)},
			metric{name: "cloudfs_index_text_bytes", help: "Extracted text held by the content index", typ: "gauge", value: float64(st.Index.TextBytes)},
			metric{name: "cloudfs_index_fetch_bytes_total", help: "Bytes the indexer downloaded from remotes since start", typ: "counter", value: float64(st.Index.FetchBytesTotal)},
			metric{name: "cloudfs_index_failures_total", help: "Extraction failures since start", typ: "counter", value: float64(st.Index.Failures)},
		)
	}
	if st.Triggers != nil {
		ms = append(ms,
			metric{name: "cloudfs_trigger_deliveries_pending", help: "Trigger deliveries waiting to run", typ: "gauge", value: float64(st.Triggers.Pending)},
			metric{name: "cloudfs_trigger_deliveries_dead", help: "Trigger deliveries that spent their retries and wait for a person", typ: "gauge", value: float64(st.Triggers.Dead)},
		)
	}
	for _, p := range st.Proxies {
		healthy := 0.0
		if p.Healthy {
			healthy = 1
		}
		ms = append(ms,
			metric{name: "cloudfs_proxy_healthy", help: "Whether a proxy outbound passed its last health check", typ: "gauge",
				labels: map[string]string{"outbound": p.Name}, value: healthy},
			metric{name: "cloudfs_proxy_latency_seconds", help: "Latency measured by the last proxy health check", typ: "gauge",
				labels: map[string]string{"outbound": p.Name}, value: float64(p.LatencyMS) / 1000},
		)
	}
	for _, r := range st.Remotes {
		open := 0.0
		if r.BreakerOpen {
			open = 1
		}
		ms = append(ms,
			metric{name: "cloudfs_remote_rate_limit", help: "Current adaptive rate limit in requests per second", typ: "gauge",
				labels: map[string]string{"remote": r.Remote, "class": "meta"}, value: r.MetaRate},
			metric{name: "cloudfs_remote_rate_limit", labels: map[string]string{"remote": r.Remote, "class": "download"}, value: r.DownRate},
			metric{name: "cloudfs_remote_rate_limit", labels: map[string]string{"remote": r.Remote, "class": "upload"}, value: r.UpRate},
			metric{name: "cloudfs_remote_rate_limit", labels: map[string]string{"remote": r.Remote, "class": "transfer"}, value: r.TransferRate},
			metric{name: "cloudfs_remote_breaker_open", help: "Whether a remote is circuit-broken after risk control", typ: "gauge",
				labels: map[string]string{"remote": r.Remote}, value: open},
			metric{name: "cloudfs_remote_calls_total", help: "Backend requests made, by operation", typ: "counter",
				labels: map[string]string{"remote": r.Remote, "op": "_all"}, value: float64(r.CallsTotal)},
			metric{name: "cloudfs_remote_read_bytes_total", help: "Bytes read from the backend", typ: "counter",
				labels: map[string]string{"remote": r.Remote}, value: float64(r.ReadBytes)},
			metric{name: "cloudfs_remote_write_bytes_total", help: "Bytes written to the backend", typ: "counter",
				labels: map[string]string{"remote": r.Remote}, value: float64(r.WriteBytes)},
		)
		// One series per operation, so a regression can be attributed to the
		// call that grew rather than to the total.
		ops := make([]string, 0, len(r.Calls))
		for op := range r.Calls {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		for _, op := range ops {
			ms = append(ms, metric{name: "cloudfs_remote_calls_total",
				labels: map[string]string{"remote": r.Remote, "op": op}, value: float64(r.Calls[op])})
			if ns, ok := r.Nanos[op]; ok {
				ms = append(ms, metric{name: "cloudfs_remote_seconds_total",
					help: "Time spent inside backend requests, by operation", typ: "counter",
					labels: map[string]string{"remote": r.Remote, "op": op}, value: float64(ns) / 1e9})
			}
		}
	}

	if st.Fuse != nil {
		ops := make([]string, 0, len(st.Fuse.Ops))
		for op := range st.Fuse.Ops {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		for i, op := range ops {
			m := metric{name: "cloudfs_fuse_ops_total", typ: "counter",
				labels: map[string]string{"op": op}, value: float64(st.Fuse.Ops[op])}
			if i == 0 {
				m.help = "FUSE requests served by this process, by operation"
			}
			ms = append(ms, m)
		}
		ms = append(ms, metric{name: "cloudfs_fuse_read_bytes_total", help: "Bytes the kernel asked for through READ", typ: "counter", value: float64(st.Fuse.ReadBytes)})
		les := make([]string, 0, len(st.Fuse.ReadSizes))
		for le := range st.Fuse.ReadSizes {
			les = append(les, le)
		}
		sort.Slice(les, func(i, j int) bool { return bucketOrder(les[i]) < bucketOrder(les[j]) })
		for i, le := range les {
			m := metric{name: "cloudfs_fuse_read_size_bucket", typ: "counter",
				labels: map[string]string{"le": le}, value: float64(st.Fuse.ReadSizes[le])}
			if i == 0 {
				m.help = "READ requests by size, cumulative"
			}
			ms = append(ms, m)
		}
	}

	seen := map[string]bool{}
	for _, m := range ms {
		if !seen[m.name] && m.help != "" {
			fmt.Fprintf(w, "# HELP %s %s\n", m.name, m.help)
			fmt.Fprintf(w, "# TYPE %s %s\n", m.name, m.typ)
			seen[m.name] = true
		}
		if len(m.labels) == 0 {
			fmt.Fprintf(w, "%s %s\n", m.name, formatFloat(m.value))
			continue
		}
		keys := make([]string, 0, len(m.labels))
		for k := range m.labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%q", k, m.labels[k]))
		}
		fmt.Fprintf(w, "%s{%s} %s\n", m.name, strings.Join(parts, ","), formatFloat(m.value))
	}
}

func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

// bucketOrder sorts histogram bounds numerically with +Inf last.
func bucketOrder(le string) float64 {
	if le == "+Inf" {
		return math.Inf(1)
	}
	v, _ := strconv.ParseFloat(le, 64)
	return v
}
