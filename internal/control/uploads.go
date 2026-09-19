package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/upload"
	"cloudfs/internal/vfs"
)

// UploadItem deliberately omits blob paths, content hashes and provider
// sessions: persisted sessions can contain signed URLs and temporary keys.
type UploadItem struct {
	ID string `json:"id"`
	// Kind is "file" for a queued write and "mkdir" for a queued directory
	// creation.
	Kind         journal.Kind  `json:"kind"`
	Remote       string        `json:"remote"`
	Name         string        `json:"name"`
	State        journal.State `json:"state"`
	Size         int64         `json:"size"`
	Attempt      int           `json:"attempt"`
	NextRetryAt  time.Time     `json:"next_retry_at"`
	LastError    string        `json:"last_error,omitempty"`
	NeedsPublish bool          `json:"needs_publish,omitempty"`
}

type UploadRequest struct {
	Action  string `json:"-"`
	ID      string `json:"id,omitempty"`
	All     bool   `json:"all,omitempty"`
	Cursor  string `json:"cursor,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

type UploadResponse struct {
	Uploads    []UploadItem   `json:"uploads,omitempty"`
	NextCursor string         `json:"next_cursor,omitempty"`
	Requeued   int            `json:"requeued,omitempty"`
	Stats      *journal.Stats `json:"stats,omitempty"`
	State      journal.State  `json:"state,omitempty"`
	Warning    string         `json:"warning,omitempty"`
	Discarded  string         `json:"discarded,omitempty"`
}

func (q UploadRequest) Validate() error {
	if q.Confirm && q.Action != "resume" && q.Action != "drop" {
		return errors.New("uploads: confirm is only valid for resume or drop")
	}
	switch q.Action {
	case "list":
		if q.ID != "" || q.All || q.Limit < 0 || q.Limit > 1000 || len(q.Cursor) > 256 {
			return errors.New("uploads: invalid list parameters (limit 1..1000)")
		}
	case "retry":
		if (q.ID == "") == !q.All || q.Cursor != "" || q.Limit != 0 || len(q.ID) > 256 {
			return errors.New("uploads: retry requires exactly one upload id or all=true")
		}
	case "flush":
		if q.ID != "" || q.All || q.Cursor != "" || q.Limit != 0 {
			return errors.New("uploads: flush takes no upload selector")
		}
	case "cancel":
		if q.ID == "" || len(q.ID) > 256 || q.All || q.Cursor != "" || q.Limit != 0 {
			return errors.New("uploads: cancel requires exactly one upload id")
		}
	case "resume":
		if q.ID == "" || len(q.ID) > 256 || q.All || q.Cursor != "" || q.Limit != 0 || !q.Confirm {
			return errors.New("uploads: resume requires one upload id and confirm=true acknowledging remote replay risk")
		}
	case "drop":
		if q.ID == "" || len(q.ID) > 256 || strings.ContainsAny(q.ID, "\x00\r\n") || q.All || q.Cursor != "" || q.Limit != 0 || !q.Confirm {
			return errors.New("uploads: drop requires one upload id and confirm=true acknowledging permanent local data loss; cancel first and wait for cancelled")
		}
	default:
		return fmt.Errorf("uploads: unknown action %q", q.Action)
	}
	return nil
}

// ManageUploads is shared by the HTTP surface and offline CLI. Only the
// journal owner can mutate it. Viewing a queue never requires an uploader.
func ManageUploads(ctx context.Context, j *journal.Journal, flush func(context.Context) (journal.Stats, error), cancel func(context.Context, string) (journal.State, error), q UploadRequest) (UploadResponse, error) {
	var out UploadResponse
	if err := q.Validate(); err != nil {
		return out, err
	}
	if j == nil {
		return out, errors.New("uploads: no write journal")
	}
	if q.Action != "list" && !j.Owner() {
		return out, errors.New("uploads: queue belongs to another process; connect to its control socket")
	}
	switch q.Action {
	case "list":
		limit := q.Limit
		if limit == 0 {
			limit = 200
		}
		rows, next, err := j.ListActive(ctx, q.Cursor, limit)
		if err != nil {
			return out, err
		}
		out.NextCursor = next
		for _, u := range rows {
			out.Uploads = append(out.Uploads, UploadItem{ID: u.ID, Kind: u.Kind, Remote: u.Remote, Name: u.Name, State: u.State, Size: u.Size, Attempt: u.Attempt, NextRetryAt: u.NextRetryAt, LastError: u.LastError, NeedsPublish: u.NeedsPublish})
		}
	case "retry":
		if q.ID != "" {
			if err := j.Requeue(ctx, q.ID); err != nil {
				return out, err
			}
			out.Requeued = 1
		} else {
			rows, err := j.Dead(ctx)
			if err != nil {
				return out, err
			}
			for _, u := range rows {
				if err := j.Requeue(ctx, u.ID); err != nil {
					// Another administrator may already have retried or removed
					// this snapshot row. Never reset an active upload's state.
					if errors.Is(err, journal.ErrNotFound) || errors.Is(err, journal.ErrNotDead) || errors.Is(err, journal.ErrInFlight) {
						continue
					}
					return out, err
				}
				out.Requeued++
			}
		}
	case "flush":
		if flush == nil {
			return out, errors.New("uploads: no uploader")
		}
		st, err := flush(ctx)
		out.Stats = &st
		return out, err
	case "cancel":
		if cancel == nil {
			return out, errors.New("uploads: cancellation coordinator unavailable")
		}
		state, err := cancel(ctx, q.ID)
		out.State = state
		out.Warning = "local content retained; cancellation does not undo or reconcile remote changes; retry and discard require further explicit management"
		return out, err
	case "resume":
		return out, errors.New("uploads: resume requires the VFS resume coordinator")
	case "drop":
		return out, errors.New("uploads: drop requires the VFS cleanup coordinator")
	}
	return out, nil
}

// privateRequest excludes cross-site and DNS-rebound requests. The embedded UI
// may call the API from the exact same origin; a custom mutation header still
// prevents cross-site form submission and no CORS permission is granted.
// Native local clients use Host: cloudfs.
func privateRequest(w http.ResponseWriter, r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	origin := r.Header.Get("Origin")
	if (host != "cloudfs" && host != "localhost" && (ip == nil || !ip.IsLoopback())) || (origin != "" && !sameControlOrigin(origin, r.Host)) || (r.Header.Get("Sec-Fetch-Site") != "" && r.Header.Get("Sec-Fetch-Site") != "none" && r.Header.Get("Sec-Fetch-Site") != "same-origin") {
		httpErrorT(w, r, http.StatusForbidden, "err.local_clients_only")
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-CloudFS-Control") != "1" {
		httpErrorT(w, r, http.StatusForbidden, "err.control_header")
		return false
	}
	return true
}

func sameControlOrigin(raw, host string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.Host != "" && strings.EqualFold(u.Host, host) && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}

func (s *Server) uploads(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	q := UploadRequest{Action: strings.TrimPrefix(r.URL.Path, "/uploads/")}
	method := http.MethodPost
	if r.URL.Path == "/uploads" {
		q.Action, method = "list", http.MethodGet
	}
	if q.Action != "list" && q.Action != "retry" && q.Action != "flush" && q.Action != "cancel" && q.Action != "resume" && q.Action != "drop" {
		http.NotFound(w, r)
		return
	}
	if !allowMethod(w, r, method) {
		return
	}
	if q.Action == "list" {
		q.Cursor = r.URL.Query().Get("cursor")
		if l := r.URL.Query().Get("limit"); l != "" {
			n, err := strconv.Atoi(l)
			if err != nil || n < 1 {
				httpErrorT(w, r, http.StatusBadRequest, "err.invalid_limit")
				return
			}
			q.Limit = n
		}
	} else {
		if q.Action == "drop" && r.URL.RawQuery != "" {
			httpErrorT(w, r, http.StatusBadRequest, "err.drop_params")
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			httpErrorT(w, r, http.StatusUnsupportedMediaType, "err.use_json")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var decodeErr error
		if q.Action == "drop" {
			decodeErr = decodeUploadDiscard(dec, &q)
		} else {
			decodeErr = dec.Decode(&q)
		}
		if decodeErr != nil {
			httpErrorT(w, r, http.StatusBadRequest, "err.invalid_json")
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			httpErrorT(w, r, http.StatusBadRequest, "err.one_json_object")
			return
		}
	}
	if err := q.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var out UploadResponse
	var err error
	if q.Action == "resume" {
		out, err = ManageUploadResume(r.Context(), s.collector.ResumeUpload, q)
	} else if q.Action == "drop" {
		out, err = ManageUploadDiscard(r.Context(), s.collector.DiscardUpload, q)
	} else {
		out, err = ManageUploads(r.Context(), s.collector.Journal, s.collector.FlushUploads, s.collector.CancelUpload, q)
	}
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, journal.ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, vfs.ErrReadOnly):
			status = http.StatusForbidden
		case errors.Is(err, vfs.ErrUploadResumeTarget), errors.Is(err, vfs.ErrCopyBindingChanged):
			status = http.StatusConflict
		case errors.Is(err, vfs.ErrUploadCleanupTarget), errors.Is(err, vfs.ErrUploadCleanupBusy), errors.Is(err, meta.ErrLocalVersionChanged), errors.Is(err, journal.ErrUploadCleanupState), errors.Is(err, journal.ErrUploadCleanupIdentity), errors.Is(err, journal.ErrUploadCleanupContent):
			status = http.StatusConflict
		case errors.Is(err, journal.ErrInFlight), errors.Is(err, journal.ErrNotDead), errors.Is(err, upload.ErrDeadLetters), errors.Is(err, journal.ErrCannotCancel), errors.Is(err, journal.ErrCancelled), errors.Is(err, upload.ErrCancelledUploads), errors.Is(err, journal.ErrResumeChanged), errors.Is(err, journal.ErrResumeContent), errors.Is(err, journal.ErrUploadPurging), errors.Is(err, upload.ErrCleanupPending):
			status = http.StatusConflict
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			status = http.StatusRequestTimeout
		}
		message := err.Error()
		if (q.Action == "cancel" || q.Action == "resume" || q.Action == "drop") && status == http.StatusInternalServerError {
			message = "upload operation could not be confirmed; inspect uploads list before retrying"
		}
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

// Destructive confirmation has one canonical interpretation: reject duplicate
// or differently-cased selectors rather than using encoding/json's last value.
func decodeUploadDiscard(dec *json.Decoder, q *UploadRequest) error {
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return errors.New("uploads: expected object")
	}
	seen := map[string]bool{}
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return errors.New("uploads: duplicate field")
		}
		seen[name] = true
		switch name {
		case "id":
			err = dec.Decode(&q.ID)
		case "confirm":
			err = dec.Decode(&q.Confirm)
		default:
			return errors.New("uploads: unknown discard field")
		}
		if err != nil {
			return err
		}
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim('}') {
		return errors.New("uploads: malformed object")
	}
	return nil
}

func ManageUploadDiscard(ctx context.Context, discard func(context.Context, string, bool) error, q UploadRequest) (UploadResponse, error) {
	var out UploadResponse
	if err := q.Validate(); err != nil {
		return out, err
	}
	if q.Action != "drop" || discard == nil {
		return out, errors.New("uploads: cleanup coordinator unavailable")
	}
	if err := discard(ctx, q.ID, q.Confirm); err != nil {
		return out, err
	}
	out.Discarded = q.ID
	out.Warning = "local version and private upload records discarded; remote changes were not undone or reconciled; shared data or open cache leases may retain disk space"
	return out, nil
}

func ManageUploadResume(ctx context.Context, resume func(context.Context, string, bool) error, q UploadRequest) (UploadResponse, error) {
	var out UploadResponse
	if err := q.Validate(); err != nil {
		return out, err
	}
	if q.Action != "resume" || resume == nil {
		return out, errors.New("uploads: resume coordinator unavailable")
	}
	if err := resume(ctx, q.ID, q.Confirm); err != nil {
		return out, err
	}
	out.State = journal.StatePending
	out.Warning = "accepted a fresh upload attempt; remote data may be duplicated or overwritten, completion is not confirmed"
	return out, nil
}
