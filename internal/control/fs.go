package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"cloudfs/internal/vfs"
)

// The /fs routes are the file browser's view of the mounted tree: what an
// adapter over the VFS looks like when the adapter is a web page. They do
// nothing the FUSE mount and the MCP tools cannot already do, through the same
// *vfs.FS, so they add no new capability — only a new caller. The path→inode
// translation mirrors the MCP tools (internal/mcpsrv) on purpose: two adapters
// that disagreed on what a path means would be a bug in one of them.

// FSEntry is one file or directory as the browser sees it. It carries the
// three local facts the tree cannot show through FUSE — how much of the file
// is cached, whether a pin covers it, and whether it exists only in the upload
// queue so far — and nothing that names a provider object or a cache file.
type FSEntry struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	IsDir     bool      `json:"is_dir"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mtime"`
	Cached    float64   `json:"cached"`
	Pinned    bool      `json:"pinned"`
	LocalOnly bool      `json:"local_only"`
	// Availability is set for paths under a pool: full, degraded (fewer
	// replicas than the target, or a replica on a member that is down) or
	// unavailable (no replica on a member that can be reached now).
	Availability   string `json:"availability,omitempty"`
	ReplicasLive   int    `json:"replicas_live,omitempty"`
	ReplicasTarget int    `json:"replicas_target,omitempty"`
	DegradedReason string `json:"degraded_reason,omitempty"`
}

// FSListResponse is a page of a directory.
type FSListResponse struct {
	Path       string    `json:"path"`
	Entries    []FSEntry `json:"entries"`
	NextCursor string    `json:"next_cursor,omitempty"`
	Total      int       `json:"total"`
	// LinkShareable says whether files under this directory can have a
	// download link at all (the mount's provider serves bytes to third
	// parties). Drive, Box and every pool holding one answer no; the page
	// hides the link button rather than offering a refusal.
	LinkShareable bool `json:"link_shareable"`
}

// FSMutation is the body of mkdir, rename and delete.
type FSMutation struct {
	Path      string `json:"path,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
	Confirm   bool   `json:"confirm,omitempty"`
}

// FSLinkResponse is a download link for one file. It is the one reply on this
// server that deliberately carries a signed URL — that is the point of the
// endpoint, the browser opens it so the bytes need not cross the daemon — and
// it is sent no-store and never logged for that reason.
type FSLinkResponse struct {
	URL       string            `json:"url"`
	ExpiresAt time.Time         `json:"expires_at,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// previewMax caps what /fs/preview will hand back in one response. It is a
// preview; a client that wants the file goes through the download link, where
// the provider's bandwidth applies rather than the daemon's.
const previewMax = 1 << 20

// fsPageMax bounds a listing page; the VFS enforces the same bound.
const fsPageMax = vfs.MaxDirectoryPageSize

// canonicalPath is the one rule for what a path in a request may look like:
// absolute, cleaned, no NUL or control characters. Cleaning also defeats
// ".." traversal. It is the same shape mcpsrv applies before its allowlist.
func canonicalPath(raw string) (string, error) {
	if raw == "" || !strings.HasPrefix(raw, "/") {
		return "", errors.New("path must be absolute")
	}
	if len(raw) > 4096 {
		return "", errors.New("path is too long")
	}
	for _, r := range raw {
		if r == 0 || unicode.IsControl(r) {
			return "", errors.New("path contains a control character")
		}
	}
	return path.Clean(raw), nil
}

// decorateAvailability adds the pool's view of a file: nothing for a
// directory or a path outside a pool, and nothing that names a member's
// object.
func (s *Server) decorateAvailability(ctx context.Context, e *FSEntry) {
	if e.IsDir || e.LocalOnly {
		return
	}
	a, ok := s.poolAvailability(ctx, e.Path)
	if !ok {
		return
	}
	e.Availability, e.ReplicasLive, e.ReplicasTarget, e.DegradedReason = a.State, a.Live, a.Target, a.Reason
}

func toEntry(dir string, a vfs.Attr) FSEntry {
	return FSEntry{
		Name: a.Name, Path: path.Join(dir, a.Name), IsDir: a.IsDir, Size: a.Size, ModTime: a.MTime,
		Cached: a.Cached, Pinned: a.Pinned, LocalOnly: a.LocalOnly,
	}
}

// fsStatus maps a VFS error to the status the browser should see. The text of
// the error is the VFS's own, which names paths but never provider objects.
func fsStatus(err error) int {
	switch {
	case errors.Is(err, vfs.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, vfs.ErrExists), errors.Is(err, vfs.ErrNotEmpty), errors.Is(err, syscall.EBUSY):
		return http.StatusConflict
	case errors.Is(err, vfs.ErrReadOnly):
		return http.StatusForbidden
	case errors.Is(err, vfs.ErrIsDir), errors.Is(err, vfs.ErrNotDir), errors.Is(err, vfs.ErrCrossMount), errors.Is(err, vfs.ErrInvalidCursor):
		return http.StatusBadRequest
	case errors.Is(err, vfs.ErrNoSpace), errors.Is(err, syscall.ENOSPC):
		return http.StatusInsufficientStorage
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusRequestTimeout
	}
	return http.StatusInternalServerError
}

// fsReady is the guard every /fs route starts with.
func (s *Server) fsReady(w http.ResponseWriter, r *http.Request, method string) bool {
	if !privateRequest(w, r) {
		return false
	}
	if !allowMethod(w, r, method) {
		return false
	}
	if s.collector.FS == nil {
		httpErrorT(w, r, http.StatusServiceUnavailable, "err.no_filesystem")
		return false
	}
	return true
}

func (s *Server) fsPath(w http.ResponseWriter, raw string) (string, bool) {
	p, err := canonicalPath(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", false
	}
	return p, true
}

// decodeMutation reads one JSON object and nothing else, the way the other
// mutating routes do.
func decodeMutation(w http.ResponseWriter, r *http.Request, into any) bool {
	return decodeMutationLimit(w, r, into, 16<<10)
}

// decodeMutationLimit is decodeMutation with the body cap named. Routes that
// take a small fixed request — an id and a confirmation — cap tighter; the
// 415/400/400 sequence is the same one, in one place.
func decodeMutationLimit(w http.ResponseWriter, r *http.Request, into any, max int64) bool {
	if r.Header.Get("Content-Type") != "application/json" {
		httpErrorT(w, r, http.StatusUnsupportedMediaType, "err.use_json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_json")
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		httpErrorT(w, r, http.StatusBadRequest, "err.one_json_object")
		return false
	}
	return true
}

// GET /fs/list?path=/dir&cursor=&limit=200
func (s *Server) fsList(w http.ResponseWriter, r *http.Request) {
	if !s.fsReady(w, r, http.MethodGet) {
		return
	}
	p, ok := s.fsPath(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > fsPageMax {
			httpErrorT(w, r, http.StatusBadRequest, "err.limit_range", fsPageMax)
			return
		}
		limit = n
	}
	opt, err := vfs.ParseDirectoryCursor(r.URL.Query().Get("cursor"), limit)
	if err != nil {
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_cursor")
		return
	}
	page, err := s.collector.FS.ReadDirPagePath(r.Context(), p, opt)
	if err != nil {
		http.Error(w, err.Error(), fsStatus(err))
		return
	}
	out := FSListResponse{Path: p, Entries: make([]FSEntry, 0, len(page.Entries)), Total: page.Total, NextCursor: vfs.NextDirectoryCursor(page)}
	// A directory that was just listed resolves; a failure here is not one
	// worth failing the listing for, and "no link" is the safe answer.
	out.LinkShareable, _ = s.collector.FS.HandsOutLinks(r.Context(), p)
	for _, a := range page.Entries {
		e := toEntry(p, a)
		s.decorateAvailability(r.Context(), &e)
		out.Entries = append(out.Entries, e)
	}
	writeJSON(w, out)
}

// GET /fs/stat?path=/file
func (s *Server) fsStat(w http.ResponseWriter, r *http.Request) {
	if !s.fsReady(w, r, http.MethodGet) {
		return
	}
	p, ok := s.fsPath(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	a, err := s.collector.FS.StatPath(r.Context(), p)
	if err != nil {
		http.Error(w, err.Error(), fsStatus(err))
		return
	}
	e := toEntry(path.Dir(p), a)
	s.decorateAvailability(r.Context(), &e)
	e.Path = p
	writeJSON(w, e)
}

// GET /fs/preview?path=/file&offset=0&length=65536 — raw bytes, capped.
func (s *Server) fsPreview(w http.ResponseWriter, r *http.Request) {
	if !s.fsReady(w, r, http.MethodGet) {
		return
	}
	p, ok := s.fsPath(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	var offset, length int64 = 0, 64 << 10
	q := r.URL.Query()
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			httpErrorT(w, r, http.StatusBadRequest, "err.offset_invalid")
			return
		}
		offset = n
	}
	if raw := q.Get("length"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 || n > previewMax {
			httpErrorT(w, r, http.StatusBadRequest, "err.length_range", previewMax)
			return
		}
		length = n
	}
	a, err := s.collector.FS.StatPath(r.Context(), p)
	if err != nil {
		http.Error(w, err.Error(), fsStatus(err))
		return
	}
	if a.IsDir {
		http.Error(w, vfs.ErrIsDir.Error(), http.StatusBadRequest)
		return
	}
	data, err := s.collector.FS.ReadFileRange(r.Context(), p, offset, length)
	if err != nil {
		http.Error(w, err.Error(), fsStatus(err))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if len(data) == 0 {
		// An empty read (zero-length file, or offset at/after EOF) has no byte
		// range to name; "bytes 0--1/0" is not something a client can parse.
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", a.Size))
	} else {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+int64(len(data))-1, a.Size))
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// GET /fs/download-url?path=/file
func (s *Server) fsDownloadURL(w http.ResponseWriter, r *http.Request) {
	if !s.fsReady(w, r, http.MethodGet) {
		return
	}
	p, ok := s.fsPath(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	link, err := s.collector.FS.DownloadURL(r.Context(), p)
	if err != nil {
		// Neither is a fault: a Drive or Box file is readable only with the
		// account's own credential, and a file still in the journal has no
		// remote yet. Both used to reach the page as the vfs sentence, in
		// English, naming the pool ("home does not hand out links...").
		switch {
		case errors.Is(err, vfs.ErrNoDownloadURL):
			httpErrorT(w, r, http.StatusUnprocessableEntity, "err.link_unshareable")
		case errors.Is(err, vfs.ErrNotUploaded):
			httpErrorT(w, r, http.StatusConflict, "err.link_not_uploaded")
		default:
			http.Error(w, err.Error(), fsStatus(err))
		}
		return
	}
	writeJSON(w, FSLinkResponse{URL: link.URL, ExpiresAt: link.ExpiresAt, Headers: link.Headers})
}

// POST /fs/mkdir {"path": "/dir/new"} — one level, like mkdir(2). A typo in a
// parent must not quietly produce a chain of directories on the remote.
func (s *Server) fsMkdir(w http.ResponseWriter, r *http.Request) {
	if !s.fsReady(w, r, http.MethodPost) {
		return
	}
	var q FSMutation
	if !decodeMutation(w, r, &q) {
		return
	}
	p, ok := s.fsPath(w, q.Path)
	if !ok {
		return
	}
	if p == "/" {
		httpErrorT(w, r, http.StatusConflict, "err.root_exists")
		return
	}
	parent, err := s.collector.FS.StatPath(r.Context(), path.Dir(p))
	if err != nil {
		http.Error(w, err.Error(), fsStatus(err))
		return
	}
	a, err := s.collector.FS.Mkdir(r.Context(), parent.Ino, path.Base(p))
	if err != nil {
		http.Error(w, err.Error(), fsStatus(err))
		return
	}
	writeJSON(w, toEntry(path.Dir(p), a))
}

// POST /fs/rename {"from": "/a", "to": "/b"}
func (s *Server) fsRename(w http.ResponseWriter, r *http.Request) {
	if !s.fsReady(w, r, http.MethodPost) {
		return
	}
	var q FSMutation
	if !decodeMutation(w, r, &q) {
		return
	}
	from, ok := s.fsPath(w, q.From)
	if !ok {
		return
	}
	to, ok := s.fsPath(w, q.To)
	if !ok {
		return
	}
	if from == "/" || to == "/" {
		httpErrorT(w, r, http.StatusBadRequest, "err.root_rename")
		return
	}
	if to == from || strings.HasPrefix(to, from+"/") {
		httpErrorT(w, r, http.StatusBadRequest, "err.move_into_self")
		return
	}
	src, err := s.collector.FS.StatPath(r.Context(), path.Dir(from))
	if err != nil {
		http.Error(w, err.Error(), fsStatus(err))
		return
	}
	dst, err := s.collector.FS.StatPath(r.Context(), path.Dir(to))
	if err != nil {
		http.Error(w, err.Error(), fsStatus(err))
		return
	}
	if err := s.collector.FS.Rename(r.Context(), src.Ino, path.Base(from), dst.Ino, path.Base(to)); err != nil {
		http.Error(w, err.Error(), fsStatus(err))
		return
	}
	a, err := s.collector.FS.StatPath(r.Context(), to)
	if err != nil {
		http.Error(w, err.Error(), fsStatus(err))
		return
	}
	writeJSON(w, toEntry(path.Dir(to), a))
}

// POST /fs/delete {"path": "/a", "recursive": false, "confirm": true}. It
// deletes on the remote too, so it wants the same typed confirmation the
// upload drop does; the UI only sets confirm after the person has seen the
// name of what goes.
func (s *Server) fsDelete(w http.ResponseWriter, r *http.Request) {
	if !s.fsReady(w, r, http.MethodPost) {
		return
	}
	var q FSMutation
	if !decodeMutation(w, r, &q) {
		return
	}
	p, ok := s.fsPath(w, q.Path)
	if !ok {
		return
	}
	if p == "/" {
		httpErrorT(w, r, http.StatusBadRequest, "err.delete_mount_root")
		return
	}
	if !confirmed(w, r, q.Confirm, "confirm.delete_path", p) {
		return
	}
	parent, err := s.collector.FS.StatPath(r.Context(), path.Dir(p))
	if err != nil {
		http.Error(w, err.Error(), fsStatus(err))
		return
	}
	if err := s.collector.FS.Remove(r.Context(), parent.Ino, path.Base(p), q.Recursive); err != nil {
		http.Error(w, err.Error(), fsStatus(err))
		return
	}
	writeJSON(w, map[string]any{"deleted": p})
}
