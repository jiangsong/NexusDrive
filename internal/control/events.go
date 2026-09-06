package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"cloudfs/internal/vfs"
)

// statusTick is how often /events repeats the status snapshot. The gauges it
// carries — cache fill, queue depth, uptime — are cosmetic at this cadence;
// the change events are what a client cannot get by polling without a storm.
const statusTick = 2 * time.Second

// EventChange is one VFS change as the browser receives it: virtual paths,
// and two hints. Subtree means descendants may have moved or gone; Rescan
// means the queue overflowed and everything shown should be re-read.
type EventChange struct {
	Paths   []string `json:"paths"`
	Subtree bool     `json:"subtree,omitempty"`
	Rescan  bool     `json:"rescan,omitempty"`
}

// GET /events is a server-sent event stream: "change" events from the VFS's
// own change feed, and a "status" event every statusTick carrying the same
// document /status serves. One subscription per open page; the VFS never
// blocks on a slow one — a full queue collapses into a rescan hint instead.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported here", http.StatusInternalServerError)
		return
	}
	var changes <-chan vfs.Change
	if s.collector.FS != nil {
		ch, stop := s.collector.FS.WatchChanges()
		defer stop()
		changes = ch
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(event string, body any) bool {
		data, err := json.Marshal(body)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send("status", s.collector.Collect(r.Context())) {
		return
	}
	ticker := time.NewTicker(statusTick)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case c, ok := <-changes:
			if !ok {
				changes = nil
				continue
			}
			if !send("change", EventChange{Paths: c.Paths, Subtree: c.Subtree, Rescan: c.Rescan}) {
				return
			}
		case <-ticker.C:
			if !send("status", s.collector.Collect(r.Context())) {
				return
			}
		}
	}
}
