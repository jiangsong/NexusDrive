package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/vfs"
)

type CopyMutationRequest struct {
	Action  string `json:"-"`
	ID      string `json:"id"`
	Confirm bool   `json:"confirm,omitempty"`
}

func (q CopyMutationRequest) Validate() error {
	if q.ID == "" || q.Action != "retry" && q.Action != "cancel" && q.Action != "forget" {
		return errors.New("copies: retry/cancel/forget requires exactly one copy ID")
	}
	if (q.Action == "forget") != q.Confirm {
		return errors.New("copies: forget requires confirm=true; other actions do not accept confirmation")
	}
	return (CopiesRequest{ID: q.ID}).Validate()
}

func CallCopyMutation(ctx context.Context, socket, tcp string, q CopyMutationRequest) (CopiesResponse, bool, error) {
	var out CopiesResponse
	if err := q.Validate(); err != nil {
		return out, false, err
	}
	body, err := json.Marshal(q)
	if err != nil {
		return out, false, err
	}
	online, err := callControl(ctx, socket, tcp, http.MethodPost, "/copies/"+q.Action, body, &out)
	return out, online, err
}

func (s *Server) mutateCopy(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/copies/")
	if action != "retry" && action != "cancel" && action != "forget" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", 405)
		return
	}
	if r.URL.RawQuery != "" {
		http.Error(w, "query parameters are not accepted", 400)
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "use application/json", 415)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	q := CopyMutationRequest{Action: action}
	if err := dec.Decode(&q); err != nil {
		http.Error(w, "invalid JSON request", 400)
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		http.Error(w, "expected one JSON object", 400)
		return
	}
	if err := q.Validate(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if s.collector.FS == nil || s.collector.Journal == nil {
		http.Error(w, "copy management unavailable", 503)
		return
	}
	var err error
	if q.Action == "cancel" {
		err = s.collector.FS.CancelCopy(r.Context(), q.ID)
	} else if q.Action == "forget" {
		err = s.collector.FS.ForgetCopy(r.Context(), q.ID)
	} else {
		err = s.collector.FS.RetryCopy(r.Context(), q.ID)
	}
	if err != nil {
		status := 500
		switch {
		case errors.Is(err, journal.ErrNotFound), errors.Is(err, vfs.ErrNotFound):
			status = 404
		case errors.Is(err, journal.ErrCopyBusy), errors.Is(err, journal.ErrCopyState), errors.Is(err, journal.ErrCopyCorrupt), errors.Is(err, journal.ErrCopyReferenced), errors.Is(err, meta.ErrCopyReferenced), errors.Is(err, vfs.ErrCopyBindingChanged), errors.Is(err, meta.ErrCopyTargetChanged), errors.Is(err, meta.ErrExists):
			status = 409
		case errors.Is(err, vfs.ErrReadOnly):
			status = 403
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			status = 408
		}
		http.Error(w, "copy "+q.Action+" failed; inspect current state before retrying", status)
		return
	}
	out := CopiesResponse{Copies: []CopyItem{}}
	if q.Action == "forget" {
		out.Forgotten = q.ID
	} else {
		out, err = InspectCopies(r.Context(), s.collector.Journal, CopiesRequest{ID: q.ID})
	}
	if err != nil {
		http.Error(w, "copy state changed but inspection failed; do not replay automatically", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}
