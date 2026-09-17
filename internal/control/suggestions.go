package control

import (
	"net/http"
	"strconv"
	"time"
)

// GET /agent/suggestions?path=&days=&limit= turns read heat into drafts
// (docs/agent-first-design.md §6.2, ui-plan G6-3): the files agents keep
// reading but the cache does not hold (pin them), the hot files no index
// rule covers (index them), the hot files nobody has changed in a long
// while (look at them), and the pins nothing read in the window (let
// them go). Every entry is a draft: this route never writes a pin or a
// rule — adopting a draft goes through /cache/pin and /index/add with
// their own confirmation — because an automatic pin is a download, and
// "no download by default" outranks convenience. Zero provider calls:
// heat from agent.db, cache and pin state from the VFS, coverage from
// index.db.

// Suggestion is one draft.
type Suggestion struct {
	// Kind is pin | index | stale | unpin.
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	Reads int64  `json:"reads"`
	// MTime is the file's modification time when meta knows it.
	MTime *time.Time `json:"mtime,omitempty"`
	// Cached is the cached fraction of the file (pin and stale drafts).
	Cached float64 `json:"cached,omitempty"`
}

// SuggestionsResponse is GET /agent/suggestions.
type SuggestionsResponse struct {
	Enabled     bool         `json:"enabled"`
	Path        string       `json:"path"`
	Days        int          `json:"days"`
	Suggestions []Suggestion `json:"suggestions"`
}

// staleAfter is how long a hot file must have gone unchanged to be worth a
// look: BearDrive's read-heat argument, that the most dangerous document
// is the one everyone reads and nobody has touched.
const staleAfter = 90 * 24 * time.Hour

func (s *Server) agentSuggestions(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) || !allowMethod(w, r, http.MethodGet) {
		return
	}
	resp := SuggestionsResponse{Path: "/", Days: 30, Suggestions: []Suggestion{}}
	if s.collector.HeatStore == nil || s.collector.FS == nil {
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
	if d, err := strconv.Atoi(q.Get("days")); err == nil && d > 0 && d <= 365 {
		resp.Days = d
	}
	limit := 50
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 && l <= 500 {
		limit = l
	}
	hot, err := s.collector.HeatStore.HotPaths(r.Context(), resp.Path, resp.Days, limit)
	if err != nil {
		httpErrorT(w, r, http.StatusInternalServerError, "err.audit_list_failed")
		return
	}
	now := s.now()
	read := map[string]bool{}
	for _, h := range hot {
		read[h.Path] = true
		a, err := s.collector.FS.StatPath(r.Context(), h.Path)
		if err != nil || a.IsDir {
			continue
		}
		base := Suggestion{Path: h.Path, Reads: h.Reads, Cached: a.Cached}
		if !a.MTime.IsZero() && a.MTime.Unix() > 0 {
			mt := a.MTime.UTC()
			base.MTime = &mt
		}
		if a.Cached < 1 && !a.Pinned {
			resp.Suggestions = append(resp.Suggestions, with(base, "pin"))
		}
		if s.collector.Index != nil {
			if st, err := s.collector.Index.Status(r.Context(), h.Path); err == nil && st.Enabled && st.State == "uncovered" {
				resp.Suggestions = append(resp.Suggestions, with(base, "index"))
			}
		}
		if base.MTime != nil && now.Sub(*base.MTime) > staleAfter {
			resp.Suggestions = append(resp.Suggestions, with(base, "stale"))
		}
	}
	// Pins nothing read in the window are candidates to release; a
	// recursive pin covers a subtree, so a read anywhere under it keeps it.
	if pins, err := s.collector.FS.Meta().Pins(r.Context()); err == nil {
		for _, p := range pins {
			if !under(resp.Path, p.Path) || readUnder(read, p.Path) {
				continue
			}
			resp.Suggestions = append(resp.Suggestions, Suggestion{Kind: "unpin", Path: p.Path})
		}
	}
	writeJSON(w, resp)
}

func with(s Suggestion, kind string) Suggestion {
	s.Kind = kind
	return s
}

// under says p is prefix or lies beneath it.
func under(prefix, p string) bool {
	if prefix == "/" || prefix == p {
		return true
	}
	return len(p) > len(prefix) && p[:len(prefix)] == prefix && p[len(prefix)] == '/'
}

// readUnder says something in read is p or beneath it.
func readUnder(read map[string]bool, p string) bool {
	for r := range read {
		if under(p, r) {
			return true
		}
	}
	return false
}
