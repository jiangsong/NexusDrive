package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/vfs"
)

type CacheRequest struct {
	Action string `json:"-"`
	Path   string `json:"path,omitempty"`
	Depth  int    `json:"depth,omitempty"`
	// All makes warm run the crawler over every mount instead of Warm:
	// one pass over the directories the index does not hold yet.
	All bool `json:"all,omitempty"`
	// Confirm is required for a warm with Depth < 0: listing a whole
	// subtree is a provider call per directory, and nobody should get
	// that from a mistyped depth.
	Confirm bool `json:"confirm,omitempty"`
}

type CacheResponse struct {
	Stats       cache.Stats     `json:"stats"`
	Pins        []vfs.PinPolicy `json:"pins"`
	Directories int             `json:"directories,omitempty"`
	FreedBytes  int64           `json:"freed_bytes,omitempty"`
	Warning     string          `json:"warning,omitempty"`
	// Crawl is the crawler's progress after a warm with All: the pass's
	// end state when it ran here, or the running state when a daemon's
	// manager took it over.
	Crawl *vfs.CrawlProgress `json:"crawl,omitempty"`
	// Queued says a daemon's manager took the pass and it runs on after
	// this response; the CLI reads it to say where to follow the crawl.
	Queued bool `json:"queued,omitempty"`
}

func (q CacheRequest) Validate() error {
	switch q.Action {
	case "stats", "gc", "pins":
		if q.Path != "" || q.Depth != 0 || q.All {
			return errors.New("control: this cache action does not take a path, depth or all")
		}
	case "pin", "unpin", "warm":
		if !strings.HasPrefix(q.Path, "/") || path.Clean(q.Path) != q.Path || len(q.Path) > 4096 || strings.ContainsAny(q.Path, "\x00\\") {
			return errors.New("control: expected a canonical absolute virtual path")
		}
		if q.Action != "warm" && q.Depth != 0 || q.Depth < -1 || q.Depth > 1024 {
			return errors.New("control: invalid warm depth")
		}
		if q.All && (q.Action != "warm" || q.Path != "/" || q.Depth >= 0) {
			return errors.New("control: all=true is warm over every mount: path must be / and depth -1")
		}
		if q.Action == "warm" && q.Depth < 0 && !q.Confirm {
			return errors.New("control: confirm=true is required to list a whole subtree")
		}
	default:
		return errors.New("control: unknown cache action")
	}
	return nil
}

func ManageCache(ctx context.Context, c *Collector, q CacheRequest) (CacheResponse, error) {
	var out CacheResponse
	if err := q.Validate(); err != nil {
		return out, err
	}
	if c.Cache == nil || c.FS == nil {
		return out, errors.New("control: cache management is not wired")
	}
	var err error
	switch q.Action {
	case "gc":
		before := c.Cache.Stats().Bytes
		err = c.Cache.GC()
		out.FreedBytes = max(0, before-c.Cache.Stats().Bytes)
	case "pin":
		err = c.FS.Pin(ctx, q.Path)
	case "unpin":
		err = c.FS.Unpin(ctx, q.Path)
	case "warm":
		if !q.All {
			out.Directories, err = c.FS.Warm(ctx, q.Path, q.Depth)
			break
		}
		// A running daemon has a crawl manager: hand the pass to it and
		// answer with where it stands, so the request does not hold the
		// connection for as long as the tree is deep. A one-shot command
		// has no manager and runs the pass here.
		if c.FS.KickCrawl() {
			prog := c.FS.CrawlProgress()
			out.Crawl, out.Queued = &prog, true
			break
		}
		var crawl config.SearchCrawl
		if cfg := c.ConfigView(); cfg != nil {
			crawl = cfg.Search.Crawl
		}
		before := c.FS.CrawlProgress().Listed
		var prog vfs.CrawlProgress
		prog, err = c.FS.CrawlOnce(ctx, vfs.CrawlOptionsFrom(crawl))
		out.Crawl = &prog
		out.Directories = int(prog.Listed - before)
	}
	out.Stats, out.Pins, out.Warning = c.Cache.Stats(), c.FS.PinPolicies(), c.FS.PinWarning()
	return out, err
}

func CallCache(ctx context.Context, socket, tcp string, q CacheRequest) (CacheResponse, bool, error) {
	var out CacheResponse
	if err := q.Validate(); err != nil {
		return out, false, err
	}
	method := http.MethodPost
	body, err := json.Marshal(q)
	if err != nil {
		return out, false, err
	}
	if q.Action == "stats" || q.Action == "pins" {
		method, body = http.MethodGet, nil
	}
	online, err := callControl(ctx, socket, tcp, method, "/cache/"+q.Action, body, &out)
	return out, online, err
}

func (s *Server) manageCache(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	q := CacheRequest{Action: strings.TrimPrefix(r.URL.Path, "/cache/")}
	method := http.MethodPost
	if q.Action == "stats" || q.Action == "pins" {
		method = http.MethodGet
	}
	if !allowMethod(w, r, method) {
		return
	}
	if method == http.MethodPost && !decodeMutationLimit(w, r, &q, 8192) {
		return
	}
	if q.Action == "warm" && q.Depth < 0 && !confirmed(w, r, q.Confirm, "confirm.warm_all", q.Path) {
		return
	}
	if err := q.Validate(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	out, err := ManageCache(r.Context(), s.collector, q)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, vfs.ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, vfs.ErrNoSpace), errors.Is(err, cache.ErrNoSpace):
			status = http.StatusInsufficientStorage
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			status = http.StatusRequestTimeout
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}
