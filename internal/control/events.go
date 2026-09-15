package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/index"
	"cloudfs/internal/vfs"
)

// statusTick is how often /events repeats the status snapshot. The gauges it
// carries — cache fill, queue depth, uptime — are cosmetic at this cadence;
// the change events are what a client cannot get by polling without a storm.
const (
	statusTick = 2 * time.Second
	exportTick = 1 * time.Second
	// indexTick is the least time between two "index" frames: the indexer
	// publishes a snapshot after every file, which is far more often than
	// a progress bar can show.
	indexTick = 1 * time.Second
)

// EventChange is one VFS change as the browser receives it: virtual paths,
// and two hints. Subtree means descendants may have moved or gone; Rescan
// means the queue overflowed and everything shown should be re-read.
type EventChange struct {
	Paths   []string `json:"paths"`
	Subtree bool     `json:"subtree,omitempty"`
	Rescan  bool     `json:"rescan,omitempty"`
}

// GET /events is a server-sent event stream: "change" events from the VFS's
// own change feed, a "status" event every statusTick, an "export" event
// every exportTick carrying the first page of live job progress, and an
// "audit" or "session" event for every row the agent store records, and
// an "index" event carrying the indexer's latest progress at most once per
// indexTick. One subscription per open page; the VFS never blocks on a slow
// one — a full queue collapses into a rescan hint instead — and the agent
// store and the indexer drop events rather than wait.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpErrorT(w, r, http.StatusInternalServerError, "err.no_streaming")
		return
	}
	var changes <-chan vfs.Change
	if s.collector.FS != nil {
		ch, stop := s.collector.FS.WatchChanges()
		defer stop()
		changes = ch
	}
	var agentEvents <-chan agent.Event
	var names *clientNames
	if s.collector.Agent != nil {
		ch, stop := s.collector.Agent.Watch()
		defer stop()
		agentEvents = ch
		names = newClientNames(s.collector.Agent)
	}
	var indexEvents <-chan index.Progress
	if s.collector.Index != nil {
		ch, stop := s.collector.Index.Watch()
		defer stop()
		indexEvents = ch
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	lang := LangFrom(r)
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
	if !send("status", s.collector.Collect(r.Context(), lang)) {
		return
	}
	if snapshot, ok := s.exportEvent(r.Context()); ok && !send("export", snapshot) {
		return
	}
	statusTicker := time.NewTicker(statusTick)
	exportTicker := time.NewTicker(exportTick)
	indexFlush := time.NewTicker(indexTick)
	defer statusTicker.Stop()
	defer exportTicker.Stop()
	defer indexFlush.Stop()
	// The newest progress snapshot not yet sent, and when the last one went
	// out: a burst of snapshots collapses into the latest one per tick.
	var lastIndex time.Time
	var pendingIndex *index.Progress
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
		case ev, ok := <-agentEvents:
			if !ok {
				agentEvents = nil
				continue
			}
			switch {
			case ev.Audit != nil:
				if !send("audit", names.auditView(r.Context(), *ev.Audit)) {
					return
				}
			case ev.Session != nil:
				// The session changed, so its cached client name may be stale.
				names.names[ev.Session.ID] = ev.Session.ClientName
				if !send("session", sessionView(*ev.Session)) {
					return
				}
			}
		case p, ok := <-indexEvents:
			if !ok {
				indexEvents = nil
				continue
			}
			pendingIndex = &p
			if time.Since(lastIndex) >= indexTick {
				if !send("index", *pendingIndex) {
					return
				}
				lastIndex, pendingIndex = time.Now(), nil
			}
		case <-indexFlush.C:
			if pendingIndex != nil {
				if !send("index", *pendingIndex) {
					return
				}
				lastIndex, pendingIndex = time.Now(), nil
			}
		case <-statusTicker.C:
			if !send("status", s.collector.Collect(r.Context(), lang)) {
				return
			}
		case <-exportTicker.C:
			if snapshot, ok := s.exportEvent(r.Context()); ok && !send("export", snapshot) {
				return
			}
		}
	}
}

func (s *Server) exportEvent(ctx context.Context) (ExportsResponse, bool) {
	if s.collector.Export == nil {
		return ExportsResponse{}, false
	}
	jobs, next, err := s.collector.Export.Jobs(ctx, defaultExportLimit, "")
	if err != nil {
		return ExportsResponse{}, false
	}
	out := ExportsResponse{Jobs: make([]ExportJobView, 0, len(jobs)), NextCursor: next}
	for _, job := range jobs {
		view := exportJobView(job)
		if !job.State.Terminal() {
			if progress, err := s.collector.Export.Progress(ctx, job.ID); err == nil {
				live := exportProgressView(progress)
				view.BytesDone, view.BytesTotal = live.BytesDone, live.BytesTotal
				view.Rate, view.ETASeconds = live.Rate, live.ETASeconds
			}
		}
		out.Jobs = append(out.Jobs, view)
	}
	return out, true
}
