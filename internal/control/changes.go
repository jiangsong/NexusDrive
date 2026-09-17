package control

import (
	"context"
	"net/http"
	"strconv"

	"cloudfs/internal/agent"
)

// GET /changes?path=&cursor=&limit= is the change record as the console
// reads it (docs/agent-first-design.md §6.1, ui-plan G5): who last wrote
// what, newest first, for one path (a directory covers what is under it)
// or for everything. The rows are the changes table agent.db keeps from
// the VFS change feed — kernel, MCP, console and WebDAV writes alike —
// so the inspector's "last modified" line, its history overlay and the
// Agent screen's changes tab all read one source. Zero provider calls.
//
// The cursor is the previous page's oldest row id; the response carries
// it back as next_cursor while more remain. A row with reliable false
// follows a feed overflow: rows before it may be missing, and the console
// says so.

// ChangeStore is what the route reads; agent.Store satisfies it.
type ChangeStore interface {
	HistoryPage(ctx context.Context, q agent.HistoryQuery) ([]agent.Change, bool, error)
}

// ChangesResponse is GET /changes.
type ChangesResponse struct {
	Enabled    bool           `json:"enabled"`
	Path       string         `json:"path"`
	Changes    []agent.Change `json:"changes"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

const (
	defaultChangesLimit = 50
	maxChangesLimit     = 500
)

func (s *Server) changes(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) || !allowMethod(w, r, http.MethodGet) {
		return
	}
	resp := ChangesResponse{Path: "/", Changes: []agent.Change{}}
	if s.collector.Changes == nil {
		writeJSON(w, resp)
		return
	}
	resp.Enabled = true
	q := r.URL.Query()
	if p := q.Get("path"); p != "" {
		clean, ok := s.fsPath(w, p)
		if !ok {
			return
		}
		resp.Path = clean
	}
	limit, ok := queryLimit(w, r, defaultChangesLimit, maxChangesLimit)
	if !ok {
		return
	}
	var before int64
	if raw := q.Get("cursor"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			httpErrorT(w, r, http.StatusBadRequest, "err.invalid_query_param")
			return
		}
		before = n
	}
	rows, more, err := s.collector.Changes.HistoryPage(r.Context(), agent.HistoryQuery{Path: resp.Path, Before: before, Limit: limit})
	if err != nil {
		httpErrorT(w, r, http.StatusInternalServerError, "err.audit_list_failed")
		return
	}
	resp.Changes = rows
	if more && len(rows) > 0 {
		resp.NextCursor = strconv.FormatInt(rows[len(rows)-1].ID, 10)
	}
	writeJSON(w, resp)
}
