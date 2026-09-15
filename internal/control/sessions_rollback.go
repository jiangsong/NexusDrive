package control

import (
	"errors"
	"net/http"
	"strings"

	"cloudfs/internal/agent"
)

// Session rollback over the control plane (docs/agent-roadmap.md §4.8):
// POST /sessions/{id}/rollback with dry_run previews the plan, with
// confirm executes it. The console previews first and only then asks for
// the confirmation; the CLI does the same with --dry-run and --confirm.

// SessionRollbackRequest is POST /sessions/{id}/rollback. Exactly one of
// DryRun and Confirm is needed: a dry run needs no confirmation, an
// execution does.
type SessionRollbackRequest struct {
	DryRun  bool `json:"dry_run,omitempty"`
	Confirm bool `json:"confirm,omitempty"`
}

// RollbackDeps is what an AgentView needs to roll a session back: the VFS
// the writes go through and the preimage store the content comes from.
// The zero value leaves rollback unavailable, which the route reports as
// 503, the way a CLI reading agent.db offline has no VFS to write to.
type RollbackDeps struct {
	FS        agent.FSOps
	Preimages *agent.Preimages
}

// maxRollbackBody bounds the request body; it holds two booleans.
const maxRollbackBody = 1 << 10

func (s *Server) rollbackSession(w http.ResponseWriter, r *http.Request, id string) {
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
	var q SessionRollbackRequest
	if !decodeMutationLimit(w, r, &q, maxRollbackBody) {
		return
	}
	if !q.DryRun && !confirmed(w, r, q.Confirm, "confirm.rollback", id) {
		return
	}
	plan, _, err := v.Rollback(r.Context(), id, q.DryRun)
	switch {
	case errors.Is(err, agent.ErrRollbackUnavailable):
		httpErrorT(w, r, http.StatusServiceUnavailable, "err.rollback_unavailable")
		return
	case errors.Is(err, agent.ErrSessionNotFound):
		httpErrorT(w, r, http.StatusNotFound, "err.session_not_found")
		return
	case err != nil:
		httpErrorT(w, r, http.StatusInternalServerError, "err.rollback_failed", err)
		return
	}
	writeJSON(w, plan)
}
