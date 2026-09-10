package control

import (
	"net/http"
	"strconv"
	"strings"
)

// SearchHit is one match. The path is the virtual path, never a provider id.
type SearchHit struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// SearchResponse says whether the answer is the whole answer: the index has
// a work budget, and a client that is not told the budget was hit would take
// a partial list for the full one.
type SearchResponse struct {
	Results  []SearchHit `json:"results"`
	Complete bool        `json:"complete"`
}

const searchMax = 500

// GET /search?q=name&path=/scope&limit=50 searches known metadata — what has
// been listed — not the remote. That is what makes it instant, and what the
// UI must say when a directory has never been opened.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	if !s.fsReady(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	if query == "" {
		httpErrorT(w, r, http.StatusBadRequest, "err.q_required")
		return
	}
	root := "/"
	if raw := q.Get("path"); raw != "" {
		p, ok := s.fsPath(w, raw)
		if !ok {
			return
		}
		root = p
	}
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > searchMax {
			httpErrorT(w, r, http.StatusBadRequest, "err.limit_range", searchMax)
			return
		}
		limit = n
	}
	report, err := s.collector.FS.Meta().SearchReport(r.Context(), query, []string{root}, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out := SearchResponse{Results: make([]SearchHit, 0, len(report.Results)), Complete: report.Complete}
	for _, hit := range report.Results {
		out.Results = append(out.Results, SearchHit{Name: hit.Name, Path: hit.Path})
	}
	writeJSON(w, out)
}
