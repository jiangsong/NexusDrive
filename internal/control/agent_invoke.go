package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/trigger"
	"cloudfs/internal/vfs"
)

// The run half of "send to agent" (docs/agent-roadmap.md §5.7). The panel
// asks /agent/endpoints which agents the configuration file names and
// posts /agent/invoke to hand one of them a prompt and some paths. The
// engine runs the configured argv without a shell, {prompt} as one element
// and every path appended as its own element; this file only decides what
// reaches the engine. A daemon with no engine or no agent of that name
// answers 404 before anything could start; the confirmation is the same
// typed one a deletion needs, because a local command is about to run; and
// every attempt that reaches the engine is an audit row for the console
// principal — naming the agent and the paths, never the prompt, which is
// the person's own text and may say anything.

// AgentEndpointsResponse is GET /agent/endpoints: names only. What each
// name runs stays in the configuration file, exactly as on /triggers.
type AgentEndpointsResponse struct {
	Agents []AgentName `json:"agents"`
}

// AgentInvokeRequest is POST /agent/invoke.
type AgentInvokeRequest struct {
	Agent   string   `json:"agent"`
	Paths   []string `json:"paths"`
	Prompt  string   `json:"prompt,omitempty"`
	Confirm bool     `json:"confirm"`
}

// Audit identity of a run started from the console. There is no MCP
// session behind it, so the session id is empty and the transport names
// the console itself.
const (
	auditPrincipalConsole = "console"
	auditTransportConsole = "console"
	auditToolInvoke       = "agent.invoke"
)

// maxInvokeRequest bounds the body: a prompt is a paragraph or a page, and
// the paths a handful. Anything bigger is not something a person typed.
const maxInvokeRequest = 64 << 10

// auditAppender is what the audit row needs from Collector.Agent; the
// store-backed view has it, a view without a store simply leaves no row.
type auditAppender interface {
	AppendAudit(ctx context.Context, row agent.AuditRow) (int64, error)
}

// GET /agent/endpoints
func (s *Server) agentEndpoints(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	out := AgentEndpointsResponse{Agents: []AgentName{}}
	if tc := s.collector.Trigger; tc != nil {
		for _, a := range tc.Agents() {
			out.Agents = append(out.Agents, AgentName{Name: a.Name})
		}
	}
	writeJSON(w, out)
}

// POST /agent/invoke {"agent":"claude","paths":["/work/a.md"],"prompt":"...","confirm":true}
func (s *Server) agentInvoke(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	var q AgentInvokeRequest
	if !decodeMutationLimit(w, r, &q, maxInvokeRequest) {
		return
	}
	// The agent first: with nothing configured there is nothing to
	// confirm, and no path lookup or process is worth doing.
	tc := s.collector.Trigger
	if tc == nil || !agentConfigured(tc, q.Agent) {
		httpErrorT(w, r, http.StatusNotFound, "err.agent_not_configured")
		return
	}
	if s.collector.FS == nil {
		httpErrorT(w, r, http.StatusServiceUnavailable, "err.no_filesystem")
		return
	}
	if len(q.Paths) == 0 {
		httpErrorT(w, r, http.StatusBadRequest, "err.agent_no_paths")
		return
	}
	paths := make([]string, 0, len(q.Paths))
	for _, raw := range q.Paths {
		p, ok := s.fsPath(w, raw)
		if !ok {
			return
		}
		if _, err := s.collector.FS.StatPath(r.Context(), p); err != nil {
			// As on /agent/prompt: the VFS's error text names paths and
			// remotes, and a missing path is just "not found".
			status := fsStatus(err)
			if errors.Is(err, vfs.ErrNotFound) || status == http.StatusInternalServerError {
				status = http.StatusNotFound
			}
			httpErrorT(w, r, status, "err.prompt_path")
			return
		}
		paths = append(paths, p)
	}
	if !confirmed(w, r, q.Confirm, "confirm.agent.invoke", q.Agent, strings.Join(paths, ", ")) {
		return
	}

	started := time.Now()
	id, err := tc.Invoke(r.Context(), q.Agent, paths, q.Prompt)
	s.auditInvoke(r.Context(), q, paths, err, time.Since(started))
	if err != nil {
		switch {
		case errors.Is(err, trigger.ErrAlreadyQueued):
			httpErrorT(w, r, http.StatusConflict, "err.agent_queued", err)
		case errors.Is(err, trigger.ErrUnknownAgent):
			httpErrorT(w, r, http.StatusNotFound, "err.agent_not_configured")
		default:
			triggerError(w, r, err)
		}
		return
	}
	writeJSON(w, TriggerActionResponse{ID: id})
}

// agentConfigured answers before the confirmation prompt, like
// ruleConfigured does for /triggers/test.
func agentConfigured(tc TriggerControl, name string) bool {
	if name == "" {
		return false
	}
	for _, a := range tc.Agents() {
		if a.Name == name {
			return true
		}
	}
	return false
}

// auditInvoke records one attempt that reached the engine. The args carry
// the agent and the prompt's length; the prompt itself is never stored —
// it is the person's own words, not a tool argument, and the audit is
// read by everyone who can open the console. A store that cannot be
// written counts the failure the way the MCP middleware does and the run
// still goes ahead.
func (s *Server) auditInvoke(ctx context.Context, q AgentInvokeRequest, paths []string, runErr error, took time.Duration) {
	a, ok := s.collector.Agent.(auditAppender)
	if !ok {
		return
	}
	args, _ := json.Marshal(struct {
		Agent     string `json:"agent"`
		PromptLen int    `json:"prompt_len"`
	}{q.Agent, len(q.Prompt)})
	row := agent.AuditRow{
		PrincipalID: auditPrincipalConsole, SessionID: "", Transport: auditTransportConsole,
		Tool: auditToolInvoke, Paths: paths, Args: args, BytesIn: int64(len(q.Prompt)),
		Result: "ok", DurationMS: took.Milliseconds(),
	}
	if runErr != nil {
		row.Result = "error"
		row.Error = runErr.Error()
	}
	// The request context may be gone by the time the row is written; the
	// audit must not depend on the client still listening.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = a.AppendAudit(ctx, row)
}
