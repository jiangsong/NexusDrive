package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"cloudfs/internal/journal"
	"github.com/google/uuid"
)

// CopyItem deliberately excludes object IDs/versions, hashes, database identity,
// payload paths and arbitrary stored errors. Paths are virtual user paths.
type CopyItem struct {
	ID         string            `json:"id"`
	Source     string            `json:"source"`
	Target     string            `json:"target"`
	State      journal.CopyState `json:"state"`
	Size       int64             `json:"size"`
	Checkpoint int64             `json:"checkpoint"`
	UploadID   string            `json:"upload_id,omitempty"`
	Warning    string            `json:"warning,omitempty"`
}

type CopiesRequest struct {
	ID     string
	Cursor string
	Limit  int
}

type CopiesResponse struct {
	Copies     []CopyItem `json:"copies"`
	NextCursor string     `json:"next_cursor,omitempty"`
	Forgotten  string     `json:"forgotten,omitempty"`
}

func (q CopiesRequest) Validate() error {
	if q.Limit < 0 || q.Limit > 1000 || q.ID != "" && (q.Cursor != "" || q.Limit != 0) {
		return errors.New("copies: use a copy ID or list cursor/limit (1..1000)")
	}
	for _, id := range []string{q.ID, q.Cursor} {
		if id == "" {
			continue
		}
		u, err := uuid.Parse(id)
		if err != nil || u.String() != id {
			return errors.New("copies: invalid copy ID or cursor")
		}
	}
	return nil
}

// InspectCopies performs no recovery, provider initialization or writes.
func InspectCopies(ctx context.Context, j *journal.Journal, q CopiesRequest) (CopiesResponse, error) {
	out := CopiesResponse{Copies: []CopyItem{}}
	if err := q.Validate(); err != nil {
		return out, err
	}
	if j == nil {
		return out, errors.New("copies: no write journal")
	}
	var jobs []journal.CopyJob
	if q.ID != "" {
		job, err := j.GetCopy(ctx, q.ID)
		if err != nil {
			return out, err
		}
		jobs = []journal.CopyJob{job}
	} else {
		limit := q.Limit
		if limit == 0 {
			limit = 200
		}
		var err error
		jobs, out.NextCursor, err = j.ListCopyJobs(ctx, q.Cursor, limit)
		if err != nil {
			return out, err
		}
	}
	for _, job := range jobs {
		item := CopyItem{ID: job.ID, Source: job.Spec.SourcePath, Target: job.Spec.TargetPath, State: job.State, Size: job.Spec.Size, Checkpoint: job.Checkpoint}
		if job.State == journal.CopySubmitted {
			item.UploadID = job.ID
		}
		if job.State == journal.CopyFailed {
			item.Warning = "preparation failed; retained content requires inspection"
		}
		if job.State == journal.CopyCancelled {
			item.Warning = "preparation cancelled; content retained until explicit retry or cleanup"
		}
		if job.State == journal.CopyPurging {
			item.Warning = "cleanup pending; reference or storage checks must finish before history is removed"
		}
		out.Copies = append(out.Copies, item)
	}
	return out, nil
}

func CallCopies(ctx context.Context, socket, tcp string, q CopiesRequest) (CopiesResponse, bool, error) {
	var out CopiesResponse
	if err := q.Validate(); err != nil {
		return out, false, err
	}
	params := url.Values{}
	if q.ID != "" {
		params.Set("id", q.ID)
	}
	if q.Cursor != "" {
		params.Set("cursor", q.Cursor)
	}
	if q.Limit != 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	online, err := callControl(ctx, socket, tcp, http.MethodGet, "/copies?"+params.Encode(), nil, &out)
	return out, online, err
}

func (s *Server) copies(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	params, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		httpErrorT(w, r, 400, "err.invalid_query")
		return
	}
	for key, values := range params {
		if (key != "id" && key != "cursor" && key != "limit") || len(values) != 1 || values[0] == "" {
			httpErrorT(w, r, 400, "err.invalid_query_param")
			return
		}
	}
	q := CopiesRequest{ID: params.Get("id"), Cursor: params.Get("cursor")}
	if raw := params.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httpErrorT(w, r, 400, "err.invalid_limit")
			return
		}
		q.Limit = n
	}
	if err := q.Validate(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if s.collector.Journal == nil {
		httpErrorT(w, r, 503, "err.copy_journal_unavailable")
		return
	}
	out, err := InspectCopies(r.Context(), s.collector.Journal, q)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, journal.ErrNotFound) {
			status = 404
		}
		// Database errors can contain filesystem paths; do not expose them.
		httpErrorT(w, r, status, "err.copy_inspect_failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}
