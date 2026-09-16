package control

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"cloudfs/internal/agent"
)

// GET /agent/heat?path=&days=&limit= is the "read heat × staleness"
// view (docs/agent-first-design.md §6.3, ui-plan G6): the most-read paths
// over a window, by kind of reader, each with its modification time from
// meta and a quadrant — hot-fresh, hot-stale (read often, changed long
// ago: the worklist), and the rest. Zero provider calls: heat comes from
// agent.db, mtimes from the metadata cache.

// HeatStore is what the route reads; agent.Store satisfies it.
type HeatStore interface {
	HotPaths(ctx context.Context, prefix string, days, limit int) ([]agent.HotPath, error)
}

// HeatEntry is one row of the response.
type HeatEntry struct {
	Path     string         `json:"path"`
	Reads    int64          `json:"reads"`
	ByKind   map[string]int `json:"by_kind"`
	LastRead time.Time      `json:"last_read"`
	MTime    *time.Time     `json:"mtime,omitempty"`
	// Quadrant is hot_stale, hot_fresh, warm_stale or warm_fresh: hot is
	// the top half of the list by reads, stale a modification time before
	// the window began.
	Quadrant string `json:"quadrant"`
}

// HeatResponse is GET /agent/heat.
type HeatResponse struct {
	Enabled bool        `json:"enabled"`
	Path    string      `json:"path"`
	Days    int         `json:"days"`
	Entries []HeatEntry `json:"entries"`
}

func (s *Server) agentHeat(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) || !allowMethod(w, r, http.MethodGet) {
		return
	}
	resp := HeatResponse{Path: "/", Days: 7, Entries: []HeatEntry{}}
	if s.collector.HeatStore == nil {
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
	limit := 100
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 && l <= 1000 {
		limit = l
	}
	hot, err := s.collector.HeatStore.HotPaths(r.Context(), resp.Path, resp.Days, limit)
	if err != nil {
		httpErrorT(w, r, http.StatusInternalServerError, "err.audit_list_failed")
		return
	}
	since := time.Now().Add(-time.Duration(resp.Days) * 24 * time.Hour)
	for i, h := range hot {
		e := HeatEntry{Path: h.Path, Reads: h.Reads, ByKind: h.ByKind, LastRead: h.LastRead}
		stale := false
		if s.collector.FS != nil {
			if n, err := s.collector.FS.Meta().Resolve(r.Context(), h.Path); err == nil && !n.MTime.IsZero() && n.MTime.Unix() > 0 {
				mt := n.MTime.UTC()
				e.MTime = &mt
				stale = mt.Before(since)
			}
		}
		heat := "warm"
		if i < (len(hot)+1)/2 {
			heat = "hot"
		}
		fresh := "fresh"
		if stale {
			fresh = "stale"
		}
		e.Quadrant = heat + "_" + fresh
		resp.Entries = append(resp.Entries, e)
	}
	writeJSON(w, resp)
}
