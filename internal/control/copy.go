package control

import (
	"context"
	"encoding/json"
	"errors"
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
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	var q CopyRequest
	if !decodeMutation(w, r, &q) {
		return
	}
	if err := q.Validate(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if s.collector.FS == nil {
		httpErrorT(w, r, 503, "err.copy_unwired")
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
