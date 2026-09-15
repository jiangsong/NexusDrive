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
	Remote       string   `json:"remote"`
	Root         string   `json:"root"`
	State        string   `json:"state"`
	LastOK       string   `json:"last_ok,omitempty"`
	LastError    string   `json:"last_error,omitempty"`
	DownSince    string   `json:"down_since,omitempty"`
	LatencyMS    float64  `json:"latency_ms"`
	Weight       float64  `json:"weight"`
	Total        int64    `json:"total,omitempty"`
	Used         int64    `json:"used,omitempty"`
	Free         int64    `json:"free,omitempty"`
	Files        int      `json:"files"`
	PendingOps   int      `json:"pending_ops"`
	NamingDenied int      `json:"naming_denied_count"`
	Class        []string `json:"class"`
	// PendingRestart is "add" or "remove" when the saved configuration and
	// the running pool intentionally differ until the next daemon restart.
	PendingRestart string `json:"pending_restart,omitempty"`
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
	BelowMin        int              `json:"below_min_replicas"`
	Unavailable     int              `json:"unavailable"`
	Repair          struct {
		Queued  int `json:"queued"`
		Blocked int `json:"blocked"`
	} `json:"repair"`
	Rebalance struct {
		Queued     int     `json:"queued"`
		Done       int     `json:"done"`
		Failed     int     `json:"failed"`
		BytesMoved int64   `json:"bytes_moved"`
		Skew       float64 `json:"skew"`
	} `json:"rebalance"`
	HoldsBytes  int64           `json:"holds_bytes"`
	Divergences int             `json:"divergences"`
	Notices     []string        `json:"notices"`
	Total       int64           `json:"total,omitempty"`
	Used        int64           `json:"used,omitempty"`
	Free        int64           `json:"free,omitempty"`
	Config      *PoolConfigView `json:"config,omitempty"`
}

type PoolRuleView struct {
	Prefix   string   `json:"prefix"`
	Replicas int      `json:"replicas,omitempty"`
	Prefer   []string `json:"prefer"`
	Avoid    []string `json:"avoid"`
	Require  []string `json:"require"`
}

type PoolConfigView struct {
	FailureDomain      string         `json:"failure_domain"`
	WriteMode          string         `json:"write_mode"`
	MinReplicasTimeout string         `json:"min_replicas_timeout"`
	OutAfter           string         `json:"out_after"`
	RepairConcurrency  int            `json:"repair_concurrency"`
	TargetSkew         float64        `json:"target_skew"`
	AutoBackfill       bool           `json:"auto_backfill"`
	RebalanceMaxRate   int64          `json:"rebalance_max_rate"`
	PauseBetween       string         `json:"pause_between"`
	Rules              []PoolRuleView `json:"rules"`
	RestartRequired    bool           `json:"restart_required"`
}

type PoolConfigRequest struct {
	Pool               string              `json:"pool"`
	Replicas           int                 `json:"replicas"`
	MinReplicas        int                 `json:"min_replicas"`
	FailureDomain      string              `json:"failure_domain"`
	WriteMode          string              `json:"write_mode"`
	MinReplicasTimeout string              `json:"min_replicas_timeout"`
	OutAfter           string              `json:"out_after"`
	RepairConcurrency  int                 `json:"repair_concurrency"`
	TargetSkew         float64             `json:"target_skew"`
	AutoBackfill       *bool               `json:"auto_backfill"`
	RebalanceMaxRate   int64               `json:"rebalance_max_rate"`
	PauseBetween       string              `json:"pause_between"`
	MemberClasses      map[string][]string `json:"member_classes"`
	Rules              []PoolRuleView      `json:"rules"`
}

type PoolPreviewRequest struct {
	Pool string `json:"pool"`
	Path string `json:"path"`
}

type PoolPreviewCandidate struct {
	Remote   string   `json:"remote"`
	Domain   string   `json:"domain"`
	Class    []string `json:"class"`
	Eligible bool     `json:"eligible"`
	Selected bool     `json:"selected"`
	Reasons  []string `json:"reasons"`
}

type PoolPreviewResponse struct {
	Pool       string                 `json:"pool"`
	Path       string                 `json:"path"`
	Rule       string                 `json:"rule,omitempty"`
	Replicas   int                    `json:"replicas"`
	Candidates []PoolPreviewCandidate `json:"candidates"`
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
	// MemberCapacity gives a total size, in bytes, for members whose backend
	// cannot report one. Placement ranks members with known free space ahead
	// of members without, so a drive that answers nothing quietly stops
	// receiving files; this is where someone setting a pool up can say how big
	// it is. Members that report their own space are left out of this map.
	MemberCapacity map[string]int64   `json:"member_capacity,omitempty"`
	Settings       *PoolConfigRequest `json:"settings,omitempty"`
}

// PoolPathRequest is POST /pool/repair and /pool/scrub.
type PoolPathRequest struct {
	Pool    string `json:"pool"`
	Path    string `json:"path,omitempty"`
	Full    bool   `json:"full,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

// PoolRebalanceRequest is POST /pool/rebalance.
type PoolRebalanceRequest struct {
	Pool string `json:"pool"`
	// TargetSkew is the fill-ratio spread to aim for, 0 meaning the
	// pool's configured rebalance.target_skew.
	TargetSkew float64 `json:"target_skew,omitempty"`
	// DryRun plans without queueing anything.
	DryRun  bool `json:"dry_run,omitempty"`
	Confirm bool `json:"confirm,omitempty"`
}

// PoolRebalanceResponse is the plan, whether or not it was queued.
type PoolRebalanceResponse struct {
	Pool   string              `json:"pool"`
	PlanID string              `json:"plan_id,omitempty"`
	Skew   float64             `json:"skew"`
	Target float64             `json:"target_skew"`
	Moves  []PoolRebalanceMove `json:"moves"`
	Bytes  int64               `json:"bytes"`
	Queued bool                `json:"queued"`
	Reason string              `json:"reason,omitempty"`
	DryRun bool                `json:"dry_run"`
}

// PoolRebalanceMove is one file changing members.
type PoolRebalanceMove struct {
	Path string `json:"path"`
	From string `json:"from"`
	To   string `json:"to"`
	Size int64  `json:"size"`
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
		Files: r.Files, UnderReplicated: r.UnderReplicated, BelowMin: r.BelowMin, Unavailable: r.Unavailable, HoldsBytes: r.HoldsBytes, Divergences: r.Divergences, Notices: r.Notices}
	if v.Notices == nil {
		v.Notices = []string{}
	}
	v.Repair.Queued, v.Repair.Blocked = r.Repair.Queued, r.Repair.Blocked
	v.Rebalance.Queued, v.Rebalance.Done = r.Rebalance.Queued, r.Rebalance.Done
	v.Rebalance.Failed, v.Rebalance.BytesMoved, v.Rebalance.Skew = r.Rebalance.Failed, r.Rebalance.BytesMoved, r.Rebalance.Skew
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

// markPendingPoolMembers reconciles runtime truth (health, copies and space)
// with desired configuration. A member edit takes effect only after restart,
// so omitting this distinction made a successfully removed member look
// removable again and made a newly added member disappear from the page.
func markPendingPoolMembers(views []PoolView, cfg *config.Config) {
	for pi := range views {
		configured := map[string]config.PoolMember{}
		if p, ok := cfg.Pools[views[pi].Name]; ok {
			views[pi].Config = poolConfigView(p)
			for _, member := range p.Members {
				configured[member.Remote] = member
			}
		}
		running := make(map[string]bool, len(views[pi].Members))
		for mi := range views[pi].Members {
			name := views[pi].Members[mi].Remote
			views[pi].Members[mi].Class = append([]string{}, configured[name].Class...)
			running[name] = true
			if _, ok := configured[name]; !ok {
				views[pi].Members[mi].PendingRestart = "remove"
			}
		}
		for _, member := range cfg.Pools[views[pi].Name].Members {
			if running[member.Remote] {
				continue
			}
			views[pi].Members = append(views[pi].Members, PoolMemberView{
				Remote: member.Remote, Root: member.Root, Weight: member.Weight,
				Class:          append([]string{}, member.Class...),
				PendingRestart: "add",
			})
		}
	}
}

func poolConfigView(p config.Pool) *PoolConfigView {
	v := &PoolConfigView{
		FailureDomain: p.FailureDomain, WriteMode: p.WriteMode,
		MinReplicasTimeout: p.MinReplicasTimeout.String(), OutAfter: p.OutAfter.String(),
		RepairConcurrency: p.RepairConcurrency, TargetSkew: p.Rebalance.TargetSkew,
		RebalanceMaxRate: int64(p.Rebalance.MaxRate), PauseBetween: p.Rebalance.PauseBetween.String(),
		Rules: []PoolRuleView{}, RestartRequired: true,
	}
	if p.Rebalance.AutoBackfill != nil {
		v.AutoBackfill = *p.Rebalance.AutoBackfill
	}
	for _, rule := range p.Rules {
		v.Rules = append(v.Rules, PoolRuleView{Prefix: rule.Prefix, Replicas: rule.Replicas,
			Prefer: append([]string{}, rule.Prefer...), Avoid: append([]string{}, rule.Avoid...), Require: append([]string{}, rule.Require...)})
	}
	return v
}

// GET /pool/status
func (s *Server) poolStatus(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		httpErrorT(w, r, http.StatusMethodNotAllowed, "err.get_only")
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
	if cfg := s.collector.ConfigView(); cfg != nil && cfg.SourcePath != "" {
		out.Configurable = true
		markPendingPoolMembers(out.Pools, cfg)
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
	cfg := s.collector.ConfigView()
	if cfg == nil || cfg.SourcePath == "" {
		httpErrorT(w, r, http.StatusConflict, "err.no_config_to_write")
		return
	}
	var members []config.PoolMember
	for _, m := range in.Members {
		member := config.PoolMember{Remote: m}
		if size, ok := in.MemberCapacity[m]; ok && size > 0 {
			member.Capacity = config.Size(size)
		}
		members = append(members, member)
	}
	var createErr error
	if in.Settings == nil {
		createErr = config.CreatePool(cfg.SourcePath, in.Name, members, in.Replicas, in.MinReplicas, "")
	} else {
		settings := config.Pool{Members: members, Replicas: in.Replicas, MinReplicas: in.MinReplicas}
		q := in.Settings
		settings.FailureDomain, settings.WriteMode = q.FailureDomain, q.WriteMode
		settings.RepairConcurrency = q.RepairConcurrency
		settings.Rebalance.TargetSkew, settings.Rebalance.AutoBackfill = q.TargetSkew, q.AutoBackfill
		settings.Rebalance.MaxRate = config.Size(q.RebalanceMaxRate)
		if q.MinReplicasTimeout != "" {
			settings.MinReplicasTimeout, createErr = time.ParseDuration(q.MinReplicasTimeout)
		}
		if createErr == nil && q.OutAfter != "" {
			settings.OutAfter, createErr = time.ParseDuration(q.OutAfter)
		}
		if createErr == nil && q.PauseBetween != "" {
			settings.Rebalance.PauseBetween, createErr = time.ParseDuration(q.PauseBetween)
		}
		if createErr == nil {
			for i := range settings.Members {
				settings.Members[i].Class = append([]string{}, q.MemberClasses[settings.Members[i].Remote]...)
			}
			for _, rule := range q.Rules {
				settings.Rules = append(settings.Rules, config.PoolRule{Prefix: rule.Prefix, Replicas: rule.Replicas, Prefer: rule.Prefer, Avoid: rule.Avoid, Require: rule.Require})
			}
			createErr = config.CreatePoolAdvanced(cfg.SourcePath, in.Name, settings.Members, in.Replicas, in.MinReplicas, "", settings)
		}
	}
	if createErr != nil {
		http.Error(w, createErr.Error(), http.StatusBadRequest)
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
	s.reloadConfigView()
	writeJSON(w, PoolMutationResponse{Pool: in.Name, RestartRequired: true, Detail: detail})
}

// POST /pool/config atomically updates the editable placement policy. These
// fields shape a running Pool at construction time, so the response is always
// explicit that a restart is required.
func (s *Server) poolConfig(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	var in PoolConfigRequest
	if !decodeMutation(w, r, &in) {
		return
	}
	cfg := s.collector.ConfigView()
	if cfg == nil || cfg.SourcePath == "" {
		httpErrorT(w, r, http.StatusConflict, "err.no_config_to_write")
		return
	}
	next, ok := cfg.Pools[in.Pool]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown pool %q", in.Pool), http.StatusNotFound)
		return
	}
	if in.Replicas > 0 {
		next.Replicas = in.Replicas
	}
	if in.MinReplicas > 0 {
		next.MinReplicas = in.MinReplicas
	}
	if in.RepairConcurrency > 0 {
		next.RepairConcurrency = in.RepairConcurrency
	}
	if in.FailureDomain != "" {
		next.FailureDomain = in.FailureDomain
	}
	if in.WriteMode != "" {
		next.WriteMode = in.WriteMode
	}
	if in.MinReplicasTimeout != "" {
		d, err := time.ParseDuration(in.MinReplicasTimeout)
		if err != nil {
			http.Error(w, "min_replicas_timeout: "+err.Error(), http.StatusBadRequest)
			return
		}
		next.MinReplicasTimeout = d
	}
	if in.OutAfter != "" {
		d, err := time.ParseDuration(in.OutAfter)
		if err != nil {
			http.Error(w, "out_after: "+err.Error(), http.StatusBadRequest)
			return
		}
		next.OutAfter = d
	}
	if in.TargetSkew > 0 {
		next.Rebalance.TargetSkew = in.TargetSkew
	}
	if in.AutoBackfill != nil {
		next.Rebalance.AutoBackfill = in.AutoBackfill
	}
	if in.RebalanceMaxRate > 0 {
		next.Rebalance.MaxRate = config.Size(in.RebalanceMaxRate)
	}
	if in.PauseBetween != "" {
		d, err := time.ParseDuration(in.PauseBetween)
		if err != nil {
			http.Error(w, "pause_between: "+err.Error(), http.StatusBadRequest)
			return
		}
		next.Rebalance.PauseBetween = d
	}
	if in.MemberClasses != nil {
		for i := range next.Members {
			if classes, exists := in.MemberClasses[next.Members[i].Remote]; exists {
				next.Members[i].Class = append([]string{}, classes...)
			}
		}
	}
	if in.Rules != nil {
		next.Rules = make([]config.PoolRule, 0, len(in.Rules))
		for _, rule := range in.Rules {
			next.Rules = append(next.Rules, config.PoolRule{Prefix: rule.Prefix, Replicas: rule.Replicas,
				Prefer: rule.Prefer, Avoid: rule.Avoid, Require: rule.Require})
		}
	}
	if err := config.UpdatePoolSettings(cfg.SourcePath, in.Pool, next); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.reloadConfigView()
	writeJSON(w, PoolMutationResponse{Pool: in.Pool, RestartRequired: true})
}

// POST /pool/preview explains the configured placement policy without
// changing data. It is intentionally based on the saved configuration so a
// person can preview a pending edit before restarting the daemon.
func (s *Server) poolPreview(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	var in PoolPreviewRequest
	if !decodeMutation(w, r, &in) {
		return
	}
	clean, err := canonicalPath(in.Path)
	if err != nil {
		http.Error(w, "path: "+err.Error(), http.StatusBadRequest)
		return
	}
	cfg := s.collector.ConfigView()
	if cfg == nil {
		httpErrorT(w, r, http.StatusConflict, "err.no_config_to_write")
		return
	}
	p, ok := cfg.Pools[in.Pool]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown pool %q", in.Pool), http.StatusNotFound)
		return
	}
	var matched *config.PoolRule
	for i := range p.Rules {
		rule := &p.Rules[i]
		if (clean == rule.Prefix || strings.HasPrefix(clean, strings.TrimSuffix(rule.Prefix, "/")+"/")) &&
			(matched == nil || len(rule.Prefix) > len(matched.Prefix)) {
			matched = rule
		}
	}
	replicas := p.Replicas
	if matched != nil && matched.Replicas > 0 {
		replicas = matched.Replicas
	}
	out := PoolPreviewResponse{Pool: in.Pool, Path: clean, Replicas: replicas, Candidates: []PoolPreviewCandidate{}}
	if matched != nil {
		out.Rule = matched.Prefix
	}
	for _, member := range p.Members {
		candidate := PoolPreviewCandidate{Remote: member.Remote, Class: append([]string{}, member.Class...), Eligible: true, Reasons: []string{}}
		rc := cfg.Remotes[member.Remote]
		switch p.FailureDomain {
		case config.FailureDomainProvider:
			candidate.Domain = rc.Type
		case config.FailureDomainMember:
			candidate.Domain = member.Remote
		default:
			candidate.Domain = rc.AccountBinding
			if candidate.Domain == "" {
				candidate.Domain = member.Remote
			}
		}
		if matched != nil {
			for _, required := range matched.Require {
				if !hasPoolClass(member.Class, required) {
					candidate.Eligible = false
					candidate.Reasons = append(candidate.Reasons, "missing required class "+required)
				}
			}
			for _, preferred := range matched.Prefer {
				if hasPoolClass(member.Class, preferred) {
					candidate.Reasons = append(candidate.Reasons, "preferred class "+preferred)
				}
			}
			for _, avoided := range matched.Avoid {
				if hasPoolClass(member.Class, avoided) {
					candidate.Reasons = append(candidate.Reasons, "avoided class "+avoided)
				}
			}
		}
		if len(candidate.Reasons) == 0 {
			candidate.Reasons = append(candidate.Reasons, "eligible")
		}
		out.Candidates = append(out.Candidates, candidate)
	}
	sort.SliceStable(out.Candidates, func(i, j int) bool {
		a, b := out.Candidates[i], out.Candidates[j]
		if a.Eligible != b.Eligible {
			return a.Eligible
		}
		score := func(c PoolPreviewCandidate) int {
			n := 0
			for _, reason := range c.Reasons {
				if strings.HasPrefix(reason, "preferred") {
					n += 2
				}
				if strings.HasPrefix(reason, "avoided") {
					n--
				}
			}
			return n
		}
		if score(a) != score(b) {
			return score(a) > score(b)
		}
		return a.Remote < b.Remote
	})
	usedDomains := map[string]bool{}
	selected := 0
	for i := range out.Candidates {
		if !out.Candidates[i].Eligible || selected >= replicas || usedDomains[out.Candidates[i].Domain] {
			continue
		}
		out.Candidates[i].Selected = true
		usedDomains[out.Candidates[i].Domain] = true
		selected++
	}
	for i := range out.Candidates {
		if !out.Candidates[i].Eligible || out.Candidates[i].Selected || selected >= replicas {
			continue
		}
		out.Candidates[i].Selected = true
		selected++
		out.Candidates[i].Reasons = append(out.Candidates[i].Reasons, "reused failure domain")
	}
	writeJSON(w, out)
}

func hasPoolClass(classes []string, want string) bool {
	for _, class := range classes {
		if class == want {
			return true
		}
	}
	return false
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
	cfg := s.collector.ConfigView()
	if cfg == nil || cfg.SourcePath == "" {
		httpErrorT(w, r, http.StatusConflict, "err.no_config_to_write")
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
	s.reloadConfigView()
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
		httpErrorT(w, r, http.StatusBadRequest, "err.confirm_drain")
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
		httpErrorT(w, r, http.StatusBadRequest, "err.confirm_remove_member")
		return
	}
	cfg := s.collector.ConfigView()
	if cfg == nil || cfg.SourcePath == "" {
		httpErrorT(w, r, http.StatusConflict, "err.no_config_to_write")
		return
	}
	configuredPool, poolConfigured := cfg.Pools[in.Pool]
	if !poolConfigured {
		httpErrorT(w, r, http.StatusBadRequest, "err.pool_unknown", in.Pool)
		return
	}
	// Removing a configured member is an idempotent desired-state change.
	// Until restart, the running pool still contains it and an impatient retry
	// must not turn the successful first request into a misleading 400.
	configured := false
	for _, member := range configuredPool.Members {
		if member.Remote == in.Remote {
			configured = true
			break
		}
	}
	if !configured {
		if p, ok := s.collector.Pools[in.Pool]; ok {
			for _, member := range p.Status() {
				if member.Name == in.Remote {
					writeJSON(w, PoolMutationResponse{Pool: in.Pool, RestartRequired: true})
					return
				}
			}
		}
		httpErrorT(w, r, http.StatusBadRequest, "err.pool_member_not_configured", in.Remote, in.Pool)
		return
	}
	if len(configuredPool.Members) == 1 {
		httpErrorT(w, r, http.StatusConflict, "err.pool_last_member", in.Remote, in.Pool)
		return
	}
	if p, ok := s.collector.Pools[in.Pool]; ok {
		if _, empty, err := p.DrainOnce(r.Context()); err == nil && !empty {
			for _, m := range p.Status() {
				if m.Name == in.Remote && m.Health.State == provider.HealthDraining {
					httpErrorT(w, r, http.StatusConflict, "err.member_holds_copies")
					return
				}
			}
		}
	}
	if err := config.RemovePoolMember(cfg.SourcePath, in.Pool, in.Remote); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.reloadConfigView()
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
			httpErrorT(w, r, http.StatusBadRequest, "err.confirm_rebuild")
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

// POST /pool/rebalance plans a set of moves that levels the members out,
// and queues it unless the caller only wanted to see it. Moving data
// between drives costs upload bandwidth on both, so a real run needs
// confirm the way the other expensive pool operations do.
func (s *Server) poolRebalance(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	var in PoolRebalanceRequest
	if !decodeMutation(w, r, &in) {
		return
	}
	p, ok := s.poolByName(w, in.Pool)
	if !ok {
		return
	}
	if !in.DryRun && !in.Confirm {
		httpErrorT(w, r, http.StatusBadRequest, "err.confirm_rebalance")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	plan, err := p.PlanRebalance(ctx, in.TargetSkew, in.DryRun)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := PoolRebalanceResponse{Pool: in.Pool, PlanID: plan.PlanID, Skew: plan.Skew, Target: plan.Target,
		Bytes: plan.Bytes, Queued: plan.Planned, Reason: plan.Reason, DryRun: in.DryRun, Moves: []PoolRebalanceMove{}}
	for _, mv := range plan.Moves {
		out.Moves = append(out.Moves, PoolRebalanceMove{Path: mv.Path, From: mv.From, To: mv.To, Size: mv.Size})
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
			httpErrorT(w, r, http.StatusBadRequest, "err.action_invalid")
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		httpErrorT(w, r, http.StatusMethodNotAllowed, "err.get_or_post")
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
	cfg := s.collector.ConfigView()
	if cfg == nil || cfg.SourcePath == "" {
		httpErrorT(w, r, http.StatusConflict, "err.no_config_to_write")
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
	s.reloadConfigView()
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
