package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/agent"
)

// AgentView is what the control plane needs from the agent store: the audit
// trail, the sessions, and a feed of both. The daemon builds one over
// agent.Store and agent.Sessions with NewAgentView; a daemon without a store
// leaves Collector.Agent nil and the routes answer 503.
type AgentView interface {
	Audit(ctx context.Context, q agent.AuditQuery) ([]agent.AuditRow, string, error)
	// Sessions lists sessions newest first with Writes filled in.
	Sessions(ctx context.Context, q agent.ListQuery) ([]agent.Session, string, error)
	// Session returns one session with Writes filled in.
	Session(ctx context.Context, id string) (agent.Session, error)
	// SessionAudit returns the newest audit rows of one session.
	SessionAudit(ctx context.Context, id string, limit int) ([]agent.AuditRow, error)
	FinishSession(ctx context.Context, id, summary string) (agent.Session, error)
	// SessionOps returns the writes one session recorded, in order.
	SessionOps(ctx context.Context, id string) ([]agent.Op, error)
	// Rollback undoes a session's writes (or, with dryRun, reports what it
	// would do). It returns agent.ErrRollbackUnavailable when no VFS is
	// wired, as in an offline CLI.
	Rollback(ctx context.Context, id string, dryRun bool) (agent.Plan, agent.Session, error)
	// Summary counts active sessions and today's writes and denials.
	Summary(ctx context.Context) (agent.Summary, error)
	Watch() (<-chan agent.Event, func())
	AuditWriteFailures() int64
	// Workspace is the delivery directory sessions write into; "" until the
	// workspace feature lands.
	Workspace() string
}

// AuditView is one audit row as the console reads it: the session's client
// name is joined in so a table row needs no second request.
type AuditView struct {
	ID         int64           `json:"id"`
	TS         time.Time       `json:"ts"`
	Client     string          `json:"client"`
	SessionID  string          `json:"session_id"`
	// Transport is how the call arrived (stdio, http-token, http-bridge,
	// …): the audit tab's origin column, since every audit row is MCP.
	Transport  string          `json:"transport,omitempty"`
	Tool       string          `json:"tool"`
	Paths      []string        `json:"paths"`
	Args       json.RawMessage `json:"args"`
	BytesIn    int64           `json:"bytes_in"`
	BytesOut   int64           `json:"bytes_out"`
	Result     string          `json:"result"`
	Error      string          `json:"error,omitempty"`
	DurationMS int64           `json:"duration_ms"`
}

// AuditResponse is GET /audit.
type AuditResponse struct {
	Rows       []AuditView `json:"rows"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// SessionView is one session as the console reads it. FinishedAt is a
// pointer so an unfinished session carries no zero date.
type SessionView struct {
	ID     string `json:"id"`
	Client string `json:"client"`
	// Principal is the id of the principal the session runs as; a hook
	// session's is hook:<client>, which the list marks.
	Principal     string      `json:"principal,omitempty"`
	ClientVersion string      `json:"client_version,omitempty"`
	Transport     string      `json:"transport"`
	State         string      `json:"state"`
	Scope         agent.Scope `json:"scope"`
	Workspace     string      `json:"workspace,omitempty"`
	Sandbox       bool        `json:"sandbox"`
	StartedAt     time.Time   `json:"started_at"`
	LastSeenAt    time.Time   `json:"last_seen_at"`
	FinishedAt    *time.Time  `json:"finished_at,omitempty"`
	Writes        int         `json:"writes"`
	ArtifactCount int         `json:"artifacts"`
	Summary       string      `json:"summary,omitempty"`
	// OpsCount is how many writes the session recorded for rollback.
	OpsCount int `json:"ops_count"`
	// RolledBackAt is set once the session has been rolled back.
	RolledBackAt *time.Time `json:"rolled_back_at,omitempty"`
	// LastChangeSeen is the changes cursor the session last pulled up to.
	LastChangeSeen int64 `json:"last_change_seen,omitempty"`
}

// SessionsResponse is GET /sessions.
type SessionsResponse struct {
	Sessions   []SessionView `json:"sessions"`
	NextCursor string        `json:"next_cursor,omitempty"`
	Summary    agent.Summary `json:"summary"`
}

// SessionDetail is GET /sessions/{id}: the session, its newest audit rows,
// the artifacts it declared and the writes it recorded for rollback.
type SessionDetail struct {
	Session   SessionView      `json:"session"`
	Audit     []AuditView      `json:"audit"`
	Artifacts []agent.Artifact `json:"artifacts"`
	Ops       []agent.Op       `json:"ops"`
}

// SessionFinishRequest is POST /sessions/{id}/finish.
type SessionFinishRequest struct {
	Summary string `json:"summary,omitempty"`
}

// AgentStatus is the agent line of /status.
type AgentStatus struct {
	ActiveSessions int    `json:"active_sessions"`
	Workspace      string `json:"workspace,omitempty"`
	// AuditWriteFailures counts audit rows lost since the daemon started;
	// it backs cloudfs_audit_write_failures_total.
	AuditWriteFailures int64 `json:"audit_write_failures"`
}

const (
	defaultAuditLimit    = 200
	maxAuditLimit        = 1000
	defaultSessionsLimit = 50
	maxSessionsLimit     = 200
	// sessionAuditTail is how many rows a session detail carries.
	sessionAuditTail  = 50
	maxSessionSummary = 4 << 10
)

// storeAgentView is AgentView over the store and its session mapper.
type storeAgentView struct {
	st        *agent.Store
	m         *agent.Sessions
	workspace string
	rollback  RollbackDeps
}

// NewAgentView adapts an open agent.db to the control plane. workspace is
// the delivery directory reported by /status; "" when none is configured.
// rb carries what a rollback writes through; its zero value makes the
// rollback route answer 503.
func NewAgentView(st *agent.Store, m *agent.Sessions, workspace string, rb RollbackDeps) AgentView {
	return &storeAgentView{st: st, m: m, workspace: workspace, rollback: rb}
}

func (v *storeAgentView) Audit(ctx context.Context, q agent.AuditQuery) ([]agent.AuditRow, string, error) {
	return v.st.Audit(ctx, q)
}

func (v *storeAgentView) Sessions(ctx context.Context, q agent.ListQuery) ([]agent.Session, string, error) {
	sessions, next, err := v.m.List(ctx, q)
	if err != nil {
		return nil, "", err
	}
	if err := v.fillWrites(ctx, sessions); err != nil {
		return nil, "", err
	}
	return sessions, next, nil
}

func (v *storeAgentView) Session(ctx context.Context, id string) (agent.Session, error) {
	s, err := v.m.Get(ctx, id)
	if err != nil {
		return agent.Session{}, err
	}
	one := []agent.Session{s}
	if err := v.fillWrites(ctx, one); err != nil {
		return agent.Session{}, err
	}
	return one[0], nil
}

// fillWrites counts each session's successful writes from the audit trail,
// in one query for the whole page.
func (v *storeAgentView) fillWrites(ctx context.Context, sessions []agent.Session) error {
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	counts, err := v.m.WriteCounts(ctx, ids)
	if err != nil {
		return err
	}
	for i := range sessions {
		sessions[i].Writes = counts[sessions[i].ID]
	}
	return nil
}

func (v *storeAgentView) SessionAudit(ctx context.Context, id string, limit int) ([]agent.AuditRow, error) {
	return v.st.AuditForSession(ctx, id, limit)
}

func (v *storeAgentView) FinishSession(ctx context.Context, id, summary string) (agent.Session, error) {
	return v.m.Finish(ctx, id, summary)
}

func (v *storeAgentView) SessionOps(ctx context.Context, id string) ([]agent.Op, error) {
	return v.st.OpsOf(ctx, id)
}

func (v *storeAgentView) Rollback(ctx context.Context, id string, dryRun bool) (agent.Plan, agent.Session, error) {
	if v.rollback.FS == nil || v.rollback.Preimages == nil {
		return agent.Plan{}, agent.Session{}, agent.ErrRollbackUnavailable
	}
	return v.m.Rollback(ctx, v.rollback.FS, v.rollback.Preimages, id, dryRun)
}

// Summary counts from local midnight: "today" is the day the person looking
// at the console is in.
func (v *storeAgentView) Summary(ctx context.Context) (agent.Summary, error) {
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return v.m.Summary(ctx, midnight)
}

func (v *storeAgentView) Watch() (<-chan agent.Event, func()) { return v.st.Watch() }

// AppendAudit lets the control plane's own actions (an agent run started
// from the console) leave audit rows next to the MCP tools' rows.
func (v *storeAgentView) AppendAudit(ctx context.Context, row agent.AuditRow) (int64, error) {
	return v.st.AppendAudit(ctx, row)
}
func (v *storeAgentView) AuditWriteFailures() int64 { return v.st.AuditWriteFailures() }
func (v *storeAgentView) Workspace() string         { return v.workspace }

// agentView answers the request itself when no store is wired.
func (s *Server) agentView(w http.ResponseWriter, r *http.Request) (AgentView, bool) {
	if s.collector.Agent == nil {
		httpErrorT(w, r, http.StatusServiceUnavailable, "err.agent_unavailable")
		return nil, false
	}
	return s.collector.Agent, true
}

// clientNames resolves session ids to client names for a page of audit
// rows, asking the store once per distinct session.
type clientNames struct {
	view  AgentView
	names map[string]string
}

func newClientNames(v AgentView) *clientNames {
	return &clientNames{view: v, names: map[string]string{}}
}

func (c *clientNames) lookup(ctx context.Context, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	if name, ok := c.names[sessionID]; ok {
		return name
	}
	name := ""
	if s, err := c.view.Session(ctx, sessionID); err == nil {
		name = s.ClientName
	}
	c.names[sessionID] = name
	return name
}

func (c *clientNames) auditView(ctx context.Context, row agent.AuditRow) AuditView {
	paths := row.Paths
	if paths == nil {
		paths = []string{}
	}
	args := row.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	return AuditView{
		ID: row.ID, TS: row.TS, Client: c.lookup(ctx, row.SessionID), SessionID: row.SessionID,
		Transport: row.Transport, Tool: row.Tool, Paths: paths, Args: args, BytesIn: row.BytesIn, BytesOut: row.BytesOut,
		Result: row.Result, Error: row.Error, DurationMS: row.DurationMS,
	}
}

func (c *clientNames) auditViews(ctx context.Context, rows []agent.AuditRow) []AuditView {
	out := make([]AuditView, 0, len(rows))
	for _, row := range rows {
		out = append(out, c.auditView(ctx, row))
	}
	return out
}

// AuditViews joins client names onto a page of audit rows. The offline CLI
// uses it so `cloudfs audit` prints the same rows with or without a daemon.
func AuditViews(ctx context.Context, v AgentView, rows []agent.AuditRow) []AuditView {
	return newClientNames(v).auditViews(ctx, rows)
}

// SessionViewOf is one session as the console reads it.
func SessionViewOf(s agent.Session) SessionView {
	v := SessionView{
		ID: s.ID, Client: s.ClientName, Principal: s.PrincipalID, ClientVersion: s.ClientVersion, Transport: s.Transport,
		State: s.State, Scope: s.Scope, Workspace: s.Workspace, Sandbox: s.Sandbox,
		StartedAt: s.StartedAt, LastSeenAt: s.LastSeenAt, Writes: s.Writes,
		ArtifactCount: len(s.Artifacts), Summary: s.Summary, OpsCount: s.OpsCount, LastChangeSeen: s.LastChangeSeen,
	}
	if !s.FinishedAt.IsZero() {
		finished := s.FinishedAt
		v.FinishedAt = &finished
	}
	if !s.RolledBackAt.IsZero() {
		rolledBack := s.RolledBackAt
		v.RolledBackAt = &rolledBack
	}
	return v
}

func sessionView(s agent.Session) SessionView { return SessionViewOf(s) }

// SessionDetailOf is one session with its newest audit rows and artifacts,
// as GET /sessions/{id} answers and `cloudfs sessions show` prints offline.
func SessionDetailOf(ctx context.Context, v AgentView, id string) (SessionDetail, error) {
	sess, err := v.Session(ctx, id)
	if err != nil {
		return SessionDetail{}, err
	}
	rows, err := v.SessionAudit(ctx, id, sessionAuditTail)
	if err != nil {
		return SessionDetail{}, err
	}
	artifacts := sess.Artifacts
	if artifacts == nil {
		artifacts = []agent.Artifact{}
	}
	ops, err := v.SessionOps(ctx, id)
	if err != nil {
		return SessionDetail{}, err
	}
	if ops == nil {
		ops = []agent.Op{}
	}
	names := newClientNames(v)
	names.names[sess.ID] = sess.ClientName
	return SessionDetail{Session: SessionViewOf(sess), Audit: names.auditViews(ctx, rows), Artifacts: artifacts, Ops: ops}, nil
}

// queryLimit reads ?limit= with a default and a cap, answering the request
// itself when the value is not a number or is out of range.
func queryLimit(w http.ResponseWriter, r *http.Request, def, max int) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_limit")
		return 0, false
	}
	if n < 1 || n > max {
		httpErrorT(w, r, http.StatusBadRequest, "err.limit_range", max)
		return 0, false
	}
	return n, true
}

// agentStatus maps a store error onto a status code.
func agentStatus(err error) int {
	switch {
	case errors.Is(err, agent.ErrSessionNotFound):
		return http.StatusNotFound
	case errors.Is(err, agent.ErrInvalidCursor):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// GET /audit?cursor=&limit=&session=&tool=&result=&since=
func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	v, ok := s.agentView(w, r)
	if !ok {
		return
	}
	params := r.URL.Query()
	q := agent.AuditQuery{
		Cursor: params.Get("cursor"), Session: params.Get("session"),
		Tool: params.Get("tool"), Result: params.Get("result"),
	}
	switch q.Result {
	case "", "ok", "denied", "error", "forwarded", "oversize":
	default:
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_query_param")
		return
	}
	if raw := params.Get("since"); raw != "" {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httpErrorT(w, r, http.StatusBadRequest, "err.invalid_query_param")
			return
		}
		q.Since = since
	}
	if q.Limit, ok = queryLimit(w, r, defaultAuditLimit, maxAuditLimit); !ok {
		return
	}
	rows, next, err := v.Audit(r.Context(), q)
	if err != nil {
		httpErrorT(w, r, agentStatus(err), "err.audit_list_failed")
		return
	}
	writeJSON(w, AuditResponse{Rows: newClientNames(v).auditViews(r.Context(), rows), NextCursor: next})
}

// GET /sessions?cursor=&limit=&state=&sandbox=&path=&since=
func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	v, ok := s.agentView(w, r)
	if !ok {
		return
	}
	params := r.URL.Query()
	q := agent.ListQuery{Cursor: params.Get("cursor"), State: params.Get("state"), Path: params.Get("path")}
	switch q.State {
	case "", "active", "finished", "expired", "rolled_back":
	default:
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_query_param")
		return
	}
	if raw := params.Get("since"); raw != "" {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httpErrorT(w, r, http.StatusBadRequest, "err.invalid_query_param")
			return
		}
		q.Since = since
	}
	switch params.Get("sandbox") {
	case "", "0", "false":
	case "1", "true":
		q.Sandbox = true
	default:
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_query_param")
		return
	}
	if q.Limit, ok = queryLimit(w, r, defaultSessionsLimit, maxSessionsLimit); !ok {
		return
	}
	sessions, next, err := v.Sessions(r.Context(), q)
	if err != nil {
		httpErrorT(w, r, agentStatus(err), "err.sessions_list_failed")
		return
	}
	out := SessionsResponse{Sessions: make([]SessionView, 0, len(sessions)), NextCursor: next}
	for _, sess := range sessions {
		out.Sessions = append(out.Sessions, sessionView(sess))
	}
	if out.Summary, err = v.Summary(r.Context()); err != nil {
		httpErrorT(w, r, agentStatus(err), "err.sessions_list_failed")
		return
	}
	writeJSON(w, out)
}

// GET /sessions/<id>, POST /sessions/<id>/finish and POST
// /sessions/<id>/rollback.
func (s *Server) sessionByPath(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/sessions/")
	if id, ok := strings.CutSuffix(tail, "/finish"); ok {
		s.finishSession(w, r, id)
		return
	}
	if id, ok := strings.CutSuffix(tail, "/rollback"); ok {
		s.rollbackSession(w, r, id)
		return
	}
	if tail == "" || strings.Contains(tail, "/") {
		http.NotFound(w, r)
		return
	}
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	v, ok := s.agentView(w, r)
	if !ok {
		return
	}
	detail, err := SessionDetailOf(r.Context(), v, tail)
	if errors.Is(err, agent.ErrSessionNotFound) {
		httpErrorT(w, r, http.StatusNotFound, "err.session_not_found")
		return
	}
	if err != nil {
		httpErrorT(w, r, agentStatus(err), "err.audit_list_failed")
		return
	}
	writeJSON(w, detail)
}

func (s *Server) finishSession(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	v, ok := s.agentView(w, r)
	if !ok {
		return
	}
	var q SessionFinishRequest
	if !decodeMutationLimit(w, r, &q, maxSessionSummary) {
		return
	}
	sess, err := v.FinishSession(r.Context(), id, q.Summary)
	if err != nil {
		httpErrorT(w, r, agentStatus(err), "err.session_not_found")
		return
	}
	writeJSON(w, sessionView(sess))
}
