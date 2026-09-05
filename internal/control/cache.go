package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"

	"cloudfs/internal/cache"
	"cloudfs/internal/vfs"
)

type CacheRequest struct {
	Action string `json:"-"`
	Path   string `json:"path,omitempty"`
	Depth  int    `json:"depth,omitempty"`
}

type CacheResponse struct {
	Stats       cache.Stats     `json:"stats"`
	Pins        []vfs.PinPolicy `json:"pins"`
	Directories int             `json:"directories,omitempty"`
	FreedBytes  int64           `json:"freed_bytes,omitempty"`
	Warning     string          `json:"warning,omitempty"`
}

func (q CacheRequest) Validate() error {
	switch q.Action {
	case "stats", "gc", "pins":
		if q.Path != "" || q.Depth != 0 {
			return errors.New("control: this cache action does not take a path or depth")
		}
	case "pin", "unpin", "warm":
		if !strings.HasPrefix(q.Path, "/") || path.Clean(q.Path) != q.Path || len(q.Path) > 4096 || strings.ContainsAny(q.Path, "\x00\\") {
			return errors.New("control: expected a canonical absolute virtual path")
		}
		if q.Action != "warm" && q.Depth != 0 || q.Depth < -1 || q.Depth > 1024 {
			return errors.New("control: invalid warm depth")
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
		out.Directories, err = c.FS.Warm(ctx, q.Path, q.Depth)
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
	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, "method not allowed", 405)
		return
	}
	if method == http.MethodPost {
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "use application/json", 415)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&q); err != nil {
			http.Error(w, "invalid JSON request", 400)
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			http.Error(w, "expected one JSON object", 400)
			return
		}
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
