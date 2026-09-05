package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"
	"syscall"

	"cloudfs/internal/vfs"
)

type CopyRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type CopyResponse struct {
	File vfs.Attr `json:"file"`
}

func (q CopyRequest) Validate() error {
	for _, p := range []string{q.From, q.To} {
		if !strings.HasPrefix(p, "/") || path.Clean(p) != p || len(p) > 4096 || strings.ContainsAny(p, "\x00\\") {
			return errors.New("copy: expected canonical absolute virtual paths")
		}
	}
	return nil
}

func CallCopy(ctx context.Context, socket, tcp string, q CopyRequest) (CopyResponse, bool, error) {
	var out CopyResponse
	if err := q.Validate(); err != nil {
		return out, false, err
	}
	body, err := json.Marshal(q)
	if err != nil {
		return out, false, err
	}
	online, err := callControl(ctx, socket, tcp, http.MethodPost, "/copy", body, &out)
	return out, online, err
}

func (s *Server) copyFile(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", 405)
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "use application/json", 415)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var q CopyRequest
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
	if s.collector.FS == nil {
		http.Error(w, "copy is not wired", 503)
		return
	}
	a, err := s.collector.FS.Copy(r.Context(), q.From, q.To)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, vfs.ErrNotFound):
			status = 404
		case errors.Is(err, vfs.ErrExists), errors.Is(err, syscall.EBUSY):
			status = 409
		case errors.Is(err, vfs.ErrReadOnly):
			status = 403
		case errors.Is(err, vfs.ErrIsDir), errors.Is(err, vfs.ErrNotDir):
			status = 400
		case errors.Is(err, syscall.ENOSPC):
			status = 507
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			status = 408
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(CopyResponse{File: a})
}
