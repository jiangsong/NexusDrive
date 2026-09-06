package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/pool"
	"cloudfs/internal/provider"
)

// The pool endpoints. Status and the file browser's availability are read
// from the running pools; member changes are configuration edits that take
// effect at the next start, said with restart_required like every other
// config edit here; repair, scrub, drain and rebuild act on the running
// pool at once. No reply carries a credential, a member's object id, or a
// cache path.

// PoolMemberView is one member as the page shows it.
type PoolMemberView struct {
	Remote       string  `json:"remote"`
	Root         string  `json:"root"`
	State        string  `json:"state"`
	LastOK       string  `json:"last_ok,omitempty"`
	LastError    string  `json:"last_error,omitempty"`
	DownSince    string  `json:"down_since,omitempty"`
	LatencyMS    float64 `json:"latency_ms"`
	Weight       float64 `json:"weight"`
	Total        int64   `json:"total,omitempty"`
	Used         int64   `json:"used,omitempty"`
	Free         int64   `json:"free,omitempty"`
	Files        int     `json:"files"`
	PendingOps   int     `json:"pending_ops"`
	NamingDenied int     `json:"naming_denied_count"`
}

// PoolView is one pool.
type PoolView struct {
	Name            string           `json:"name"`
	PoolID          string           `json:"pool_id"`
	Replicas        int              `json:"replicas"`
	MinReplicas     int              `json:"min_replicas"`
	Target          int              `json:"target"`
	TargetCapped    bool             `json:"target_capped"`
	Members         []PoolMemberView `json:"members"`
	Files           int              `json:"files"`
	UnderReplicated int              `json:"under_replicated"`
	Unavailable     int              `json:"unavailable"`
	Repair          struct {
		Queued  int `json:"queued"`
		Blocked int `json:"blocked"`
	} `json:"repair"`
	HoldsBytes  int64    `json:"holds_bytes"`
	Divergences int      `json:"divergences"`
	Notices     []string `json:"notices"`
	Total       int64    `json:"total,omitempty"`
	Used        int64    `json:"used,omitempty"`
	Free        int64    `json:"free,omitempty"`
}

// PoolStatusResponse is GET /pool/status.
type PoolStatusResponse struct {
	Pools []PoolView `json:"pools"`
	// Configurable is false without a config file: no pool can be made.
	Configurable bool `json:"configurable"`
	// Candidates lists remotes that could become members: ordinary remotes
	// not yet in any pool.
	Candidates []string `json:"candidates"`
}

// PoolMemberRequest is POST /pool/members and the drain/state actions.
type PoolMemberRequest struct {
	Pool     string  `json:"pool"`
	Remote   string  `json:"remote"`
	Root     string  `json:"root,omitempty"`
	Weight   float64 `json:"weight,omitempty"`
	Capacity int64   `json:"capacity,omitempty"`
	Adopt    *bool   `json:"adopt,omitempty"`
	State    string  `json:"state,omitempty"`
	Confirm  bool    `json:"confirm,omitempty"`
}

// PoolCreateRequest is POST /pool/create.
type PoolCreateRequest struct {
	Name        string   `json:"name"`
	Members     []string `json:"members"`
	Replicas    int      `json:"replicas,omitempty"`
	MinReplicas int      `json:"min_replicas,omitempty"`
	// Mount, when set, also mounts the pool at Prefix ("/" by default).
	Mount   string `json:"mount,omitempty"`
	Prefix  string `json:"prefix,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

// PoolPathRequest is POST /pool/repair and /pool/scrub.
type PoolPathRequest struct {
	Pool    string `json:"pool"`
	Path    string `json:"path,omitempty"`
	Full    bool   `json:"full,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

// PoolDivergenceView is one thing a person must decide.
type PoolDivergenceView struct {
	Pool   string `json:"pool"`
	Path   string `json:"path"`
	Member string `json:"member"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	SeenAt string `json:"seen_at"`
}

// PoolDivergenceRequest is POST /pool/divergences: keep the pool's view
// (clear the record) or re-list the path so the members' state wins.
type PoolDivergenceRequest struct {
	Pool    string `json:"pool"`
	Path    string `json:"path"`
	Member  string `json:"member"`
	Kind    string `json:"kind"`
	Action  string `json:"action"` // "clear" or "relist"
	Confirm bool   `json:"confirm,omitempty"`
}

// PoolJoinRequest is POST /pool/join: read the marker on a remote and
// create the pool it names from the members this config has.
type PoolJoinRequest struct {
	Remote  string `json:"remote"`
	Root    string `json:"root,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

// PoolMutationResponse follows every configuration edit.
type PoolMutationResponse struct {
	Pool            string `json:"pool"`
	RestartRequired bool   `json:"restart_required"`
	Detail          string `json:"detail,omitempty"`
}

func (s *Server) poolByName(w http.ResponseWriter, name string) (*pool.Pool, bool) {
	if name == "" && len(s.collector.Pools) == 1 {
		for _, p := range s.collector.Pools {
			return p, true
		}
	}
	p, ok := s.collector.Pools[name]
	if !ok {
		http.Error(w, fmt.Sprintf("no pool %q is running here", name), http.StatusNotFound)
		return nil, false
	}
	return p, true
}

func poolView(ctx context.Context, name string, p *pool.Pool) (PoolView, error) {
	r, err := p.StatusReport(ctx)
	if err != nil {
		return PoolView{}, err
	}
	v := PoolView{Name: name, PoolID: r.PoolID, Replicas: r.Replicas, MinReplicas: r.MinReplicas, Target: r.Target, TargetCapped: r.TargetCapped,
		Files: r.Files, UnderReplicated: r.UnderReplicated, Unavailable: r.Unavailable, HoldsBytes: r.HoldsBytes, Divergences: r.Divergences, Notices: r.Notices}
	if v.Notices == nil {
		v.Notices = []string{}
	}
	v.Repair.Queued, v.Repair.Blocked = r.Repair.Queued, r.Repair.Blocked
	if r.QuotaKnown {
		v.Total, v.Used, v.Free = r.Quota.Total, r.Quota.Used, r.Quota.Free()
	}
	v.Members = []PoolMemberView{}
	for _, m := range r.Members {
		mv := PoolMemberView{Remote: m.Name, Root: m.Root, State: string(m.State), LastError: m.LastError, LatencyMS: m.LatencyMS, Weight: m.Weight, Files: m.Files, PendingOps: m.PendingOps, NamingDenied: m.NamingDenied}
		if !m.LastOK.IsZero() {
			mv.LastOK = m.LastOK.Format(time.RFC3339)
		}
		if !m.DownSince.IsZero() {
			mv.DownSince = m.DownSince.Format(time.RFC3339)
		}
		if m.QuotaKnown {
			mv.Total, mv.Used, mv.Free = m.Quota.Total, m.Quota.Used, m.Quota.Free()
		}
		v.Members = append(v.Members, mv)
	}
	return v, nil
}

// GET /pool/status
func (s *Server) poolStatus(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	out := PoolStatusResponse{Pools: []PoolView{}, Candidates: []string{}}
	names := make([]string, 0, len(s.collector.Pools))
	for name := range s.collector.Pools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v, err := poolView(r.Context(), name, s.collector.Pools[name])
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out.Pools = append(out.Pools, v)
	}
	if cfg := s.collector.Config; cfg != nil && cfg.SourcePath != "" {
		out.Configurable = true
		inPool := map[string]bool{}
		for _, p := range cfg.Pools {
			for _, m := range p.Members {
				inPool[m.Remote] = true
			}
		}
		for name, rc := range cfg.Remotes {
			if rc.Type != config.PoolType && !inPool[name] {
				out.Candidates = append(out.Candidates, name)
			}
		}
		sort.Strings(out.Candidates)
	}
	writeJSON(w, out)
}

// POST /pool/create
func (s *Server) poolCreate(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	var in PoolCreateRequest
	if !decodeMutation(w, r, &in) {
		return
	}
	cfg := s.collector.Config
	if cfg == nil || cfg.SourcePath == "" {
		http.Error(w, "this daemon has no config file to write to", http.StatusConflict)
		return
	}
	var members []config.PoolMember
	for _, m := range in.Members {
		members = append(members, config.PoolMember{Remote: m})
	}
	if err := config.CreatePool(cfg.SourcePath, in.Name, members, in.Replicas, in.MinReplicas, ""); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	detail := ""
	if in.Mount != "" {
		prefix := in.Prefix
		if prefix == "" {
			prefix = "/"
		}
		if err := config.AddMount(cfg.SourcePath, in.Mount, prefix, config.Layout{Remote: in.Name, Mode: config.ModeWriteback}); err != nil {
			detail = "pool written; mount not added: " + err.Error()
		}
	}
	writeJSON(w, PoolMutationResponse{Pool: in.Name, RestartRequired: true, Detail: detail})
}

// POST /pool/members
func (s *Server) poolMembers(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	var in PoolMemberRequest
	if !decodeMutation(w, r, &in) {
		return
	}
	cfg := s.collector.Config
	if cfg == nil || cfg.SourcePath == "" {
		http.Error(w, "this daemon has no config file to write to", http.StatusConflict)
		return
	}
	if in.Root != "" {
		if _, err := canonicalPath(in.Root); err != nil {
			http.Error(w, "root: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	m := config.PoolMember{Remote: in.Remote, Root: in.Root, Weight: in.Weight, Capacity: config.Size(in.Capacity), Adopt: in.Adopt}
	if err := config.AddPoolMember(cfg.SourcePath, in.Pool, m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, PoolMutationResponse{Pool: in.Pool, RestartRequired: true})
}

// POST /pool/members/state — enable, disable or drain a member of a
// running pool. Draining moves its files away; when it reports empty the
// member can be removed with DELETE.
func (s *Server) poolMemberState(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	var in PoolMemberRequest
	if !decodeMutation(w, r, &in) {
		return
	}
	if strings.HasSuffix(r.URL.Path, "/drain") {
		in.State = "draining"
	}
	if in.State == "draining" && !in.Confirm {
		http.Error(w, "draining moves every file off the member; pass confirm=true", http.StatusBadRequest)
		return
	}
	p, ok := s.poolByName(w, in.Pool)
	if !ok {
		return
	}
	if err := p.SetMemberState(in.Remote, in.State); err != nil {
		http.Error(w, err.Error(), poolStatusCode(err))
		return
	}
	writeJSON(w, PoolMutationResponse{Pool: in.Pool, RestartRequired: false})
}

// POST /pool/members/remove — after a drain reports the member empty,
// drop it from the configuration.
func (s *Server) poolMemberRemove(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	var in PoolMemberRequest
	if !decodeMutation(w, r, &in) {
		return
	}
	if !in.Confirm {
		http.Error(w, "removing a member drops its copies from the pool; pass confirm=true", http.StatusBadRequest)
		return
	}
	cfg := s.collector.Config
	if cfg == nil || cfg.SourcePath == "" {
		http.Error(w, "this daemon has no config file to write to", http.StatusConflict)
		return
	}
	if p, ok := s.collector.Pools[in.Pool]; ok {
		if _, empty, err := p.DrainOnce(r.Context()); err == nil && !empty {
			for _, m := range p.Status() {
				if m.Name == in.Remote && m.Health.State == provider.HealthDraining {
					http.Error(w, "the member still holds copies; let the drain finish first", http.StatusConflict)
					return
				}
			}
		}
	}
	if err := config.RemovePoolMember(cfg.SourcePath, in.Pool, in.Remote); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, PoolMutationResponse{Pool: in.Pool, RestartRequired: true})
}

// POST /pool/repair and /pool/scrub
func (s *Server) poolWork(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	var in PoolPathRequest
	if !decodeMutation(w, r, &in) {
		return
	}
	p, ok := s.poolByName(w, in.Pool)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	var out map[string]any
	switch {
	case strings.HasSuffix(r.URL.Path, "/repair"):
		queued, err := p.ScanOnce(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		made, err := p.RepairOnce(ctx)
		if err != nil && ctx.Err() != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out = map[string]any{"queued": queued, "made": made}
	case strings.HasSuffix(r.URL.Path, "/scrub"):
		if in.Path != "" {
			if _, err := canonicalPath(in.Path); err != nil {
				http.Error(w, "path: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := p.ScrubPath(ctx, in.Path); err != nil {
				http.Error(w, err.Error(), poolStatusCode(err))
				return
			}
			out = map[string]any{"looked": 1}
		} else {
			looked, err := p.ScrubOnce(ctx)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			out = map[string]any{"looked": looked}
		}
	case strings.HasSuffix(r.URL.Path, "/rebuild"):
		if !in.Confirm {
			http.Error(w, "rebuilding drops the index and re-lists every member; pass confirm=true", http.StatusBadRequest)
			return
		}
		if err := p.Rebuild(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out = map[string]any{"rebuilt": true}
	default:
		http.NotFound(w, r)
		return
	}
	writeJSON(w, out)
}

// GET/POST /pool/divergences
func (s *Server) poolDivergences(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		out := []PoolDivergenceView{}
		names := make([]string, 0, len(s.collector.Pools))
		for name := range s.collector.Pools {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			divs, err := s.collector.Pools[name].Divergences(r.Context(), 200)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			for _, d := range divs {
				out = append(out, PoolDivergenceView{Pool: name, Path: d.Path, Member: d.Member, Kind: d.Kind, Detail: d.Detail, SeenAt: d.SeenAt.Format(time.RFC3339)})
			}
		}
		writeJSON(w, map[string]any{"divergences": out})
	case http.MethodPost:
		var in PoolDivergenceRequest
		if !decodeMutation(w, r, &in) {
			return
		}
		p, ok := s.poolByName(w, in.Pool)
		if !ok {
			return
		}
		if _, err := canonicalPath(in.Path); err != nil {
			http.Error(w, "path: "+err.Error(), http.StatusBadRequest)
			return
		}
		switch in.Action {
		case "clear":
			if err := p.ClearDivergence(r.Context(), in.Path, in.Member, in.Kind); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		case "relist":
			if err := p.ScrubPath(r.Context(), in.Path); err != nil {
				http.Error(w, err.Error(), poolStatusCode(err))
				return
			}
			_ = p.ClearDivergence(r.Context(), in.Path, in.Member, in.Kind)
		default:
			http.Error(w, "action must be clear or relist", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

// POST /pool/join — read the marker a remote carries and write the pool it
// describes into this config, with every named member this config has.
func (s *Server) poolJoin(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	var in PoolJoinRequest
	if !decodeMutation(w, r, &in) {
		return
	}
	cfg := s.collector.Config
	if cfg == nil || cfg.SourcePath == "" {
		http.Error(w, "this daemon has no config file to write to", http.StatusConflict)
		return
	}
	p, ok := s.collector.Providers[in.Remote]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown remote %q", in.Remote), http.StatusNotFound)
		return
	}
	root := in.Root
	if root == "" {
		root = "/"
	}
	if _, err := canonicalPath(root); err != nil {
		http.Error(w, "root: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	m, err := pool.ReadMarker(ctx, p, root)
	if err != nil {
		http.Error(w, "no pool marker found: "+SanitizeError(err), poolStatusCode(err))
		return
	}
	if !in.Confirm {
		writeJSON(w, map[string]any{"pool": m.PoolName, "pool_id": m.PoolID, "members": m.Members, "settings": m.Settings, "written_by": m.WrittenBy, "restart_required": false, "confirm_required": true})
		return
	}
	var members []config.PoolMember
	missing := []string{}
	for _, name := range m.Members {
		if _, ok := cfg.Remotes[name]; ok {
			members = append(members, config.PoolMember{Remote: name, Root: root})
		} else {
			missing = append(missing, name)
		}
	}
	if len(members) == 0 {
		members = []config.PoolMember{{Remote: in.Remote, Root: root}}
	}
	if err := config.CreatePool(cfg.SourcePath, m.PoolName, members, m.Settings.Replicas, m.Settings.MinReplicas, ""); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	detail := ""
	if len(missing) > 0 {
		detail = "members not configured here yet: " + strings.Join(missing, ", ")
	}
	writeJSON(w, PoolMutationResponse{Pool: m.PoolName, RestartRequired: true, Detail: detail})
}

func poolStatusCode(err error) int {
	switch {
	case errors.Is(err, provider.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, provider.ErrUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, provider.ErrUnsupported):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// SanitizeError reduces a provider error to text safe for a browser: the
// sentinel's kind, not the driver's message, which can carry a URL.
func SanitizeError(err error) string {
	switch {
	case errors.Is(err, provider.ErrNotFound):
		return "not found"
	case errors.Is(err, provider.ErrUnavailable):
		return "the remote cannot be reached"
	case errors.Is(err, provider.ErrAuth):
		return "authentication rejected"
	}
	return "remote error"
}

// poolAvailability finds the pool serving a VFS path and asks it. It
// returns false when the path is not under a pool mount.
func (s *Server) poolAvailability(ctx context.Context, vpath string) (pool.Availability, bool) {
	if s.collector.FS == nil || len(s.collector.Pools) == 0 {
		return pool.Availability{}, false
	}
	var best struct {
		prefix string
		remote string
		found  bool
	}
	for _, m := range s.collector.FS.Mounts() {
		if m.Prefix == "/" || vpath == m.Prefix || strings.HasPrefix(vpath, m.Prefix+"/") {
			if !best.found || len(m.Prefix) > len(best.prefix) {
				best.prefix, best.remote, best.found = m.Prefix, m.Remote, true
			}
		}
	}
	if !best.found {
		return pool.Availability{}, false
	}
	p, ok := s.collector.Pools[best.remote]
	if !ok {
		return pool.Availability{}, false
	}
	rel := strings.TrimPrefix(vpath, best.prefix)
	if rel == "" {
		rel = "/"
	}
	a, err := p.Availability(ctx, rel)
	if err != nil {
		return pool.Availability{}, false
	}
	return a, true
}

var _ = json.Marshal
