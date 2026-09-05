package gdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

const rootActualID = "root-actual"

type record struct {
	id, name, mime string
	parents        []string
	data           []byte
	trashed        bool
	headRev        string
	version        int
	modified       time.Time
}

func (r *record) isFolder() bool { return r.mime == folderMime }

type session struct {
	fileID, parent, name string
	size                 int64
	data                 []byte
}

// fakeDrive is a stateful stand-in for the Drive API. It is deliberately
// strict about the request shape (fields, supportsAllDrives, Content-Range)
// so the driver cannot pass by sending something Drive would reject.
type fakeDrive struct {
	mu       sync.Mutex
	baseURL  string
	files    map[string]*record
	revs     map[string]map[string][]byte
	sessions map[string]*session
	changes  []map[string]any

	nextID, nextSession, nextRev int
	refreshCalls                 int
	tokenRotates                 bool
	accessValid                  map[string]bool
	purgeRevisions               bool
	ignoreRange                  bool
	pageLimit                    int
	mediaReads                   int
	revisionReads                int
}

func newFakeDrive() *fakeDrive {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	f := &fakeDrive{
		files: map[string]*record{
			rootActualID: {id: rootActualID, name: "My Drive", mime: folderMime, version: 1, modified: now},
			"folder-1":   {id: "folder-1", name: "Docs", mime: folderMime, parents: []string{rootActualID}, version: 1, modified: now},
			"file-1":     {id: "file-1", name: "Movie.mkv", mime: "video/x-matroska", parents: []string{rootActualID}, data: []byte("0123456789abcdef"), headRev: "rev-1", version: 2, modified: now},
			"file-2":     {id: "file-2", name: "Notes.txt", mime: "text/plain", parents: []string{rootActualID}, data: []byte("hello"), headRev: "rev-2", version: 2, modified: now},
			"doc-1":      {id: "doc-1", name: "Design", mime: "application/vnd.google-apps.document", parents: []string{rootActualID}, version: 1, modified: now},
			"short-1":    {id: "short-1", name: "Link", mime: "application/vnd.google-apps.shortcut", parents: []string{rootActualID}, version: 1, modified: now},
			"trash-1":    {id: "trash-1", name: "Gone.txt", mime: "text/plain", parents: []string{rootActualID}, data: []byte("x"), headRev: "rev-3", version: 1, trashed: true, modified: now},
		},
		revs: map[string]map[string][]byte{
			"file-1": {"rev-1": []byte("0123456789abcdef")},
			"file-2": {"rev-2": []byte("hello")},
		},
		sessions:    map[string]*session{},
		nextID:      100,
		accessValid: map[string]bool{"good-token": true},
	}
	return f
}

func (f *fakeDrive) newID(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

func (f *fakeDrive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/oauth2/token":
		f.oauth(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/upload/session/"):
		f.uploadChunk(w, r)
		return
	}
	if !f.authorized(r) {
		driveError(w, http.StatusUnauthorized, "authError")
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/upload/drive/v3/files"):
		f.uploadStart(w, r)
	case r.URL.Path == "/drive/v3/changes/startPageToken":
		f.startPageToken(w, r)
	case r.URL.Path == "/drive/v3/changes":
		f.changeFeed(w, r)
	case r.URL.Path == "/drive/v3/files":
		f.filesCollection(w, r)
	case strings.HasPrefix(r.URL.Path, "/drive/v3/files/"):
		f.fileResource(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDrive) authorized(r *http.Request) bool {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accessValid[token]
}

func (f *fakeDrive) oauth(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		driveError(w, http.StatusBadRequest, "invalid")
		return
	}
	f.mu.Lock()
	f.refreshCalls++
	f.accessValid["fresh-token"] = true
	rotate := f.tokenRotates
	f.mu.Unlock()
	out := map[string]any{"access_token": "fresh-token", "expires_in": 3600}
	if rotate {
		out["refresh_token"] = "rotated-refresh"
	}
	writeJSON(w, http.StatusOK, out)
}

// normalize maps Drive's "root" alias onto the concrete root id.
func normalize(id string) string {
	if id == "root" {
		return rootActualID
	}
	return id
}

func (f *fakeDrive) fileJSON(rec *record) map[string]any {
	out := map[string]any{
		"id": rec.id, "name": rec.name, "mimeType": rec.mime,
		"modifiedTime": rec.modified.Format(time.RFC3339Nano),
		"version":      strconv.Itoa(rec.version),
		"parents":      rec.parents, "trashed": rec.trashed,
	}
	if !rec.isFolder() && rec.headRev != "" {
		out["size"] = strconv.Itoa(len(rec.data))
		out["headRevisionId"] = rec.headRev
	}
	return out
}

func (f *fakeDrive) requireFields(w http.ResponseWriter, r *http.Request) bool {
	if !strings.Contains(r.URL.Query().Get("fields"), "headRevisionId") {
		driveError(w, http.StatusBadRequest, "fieldsMissing")
		return false
	}
	return true
}

func (f *fakeDrive) filesCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		f.list(w, r)
	case http.MethodPost:
		f.create(w, r)
	default:
		http.NotFound(w, r)
	}
}

// parseQuery understands only the two expressions the driver builds.
func parseQuery(q string) (parent, name string, ok bool) {
	if !strings.Contains(q, "in parents") || !strings.Contains(q, "trashed = false") {
		return "", "", false
	}
	start := strings.Index(q, "'")
	end := strings.Index(q[start+1:], "'")
	if start < 0 || end < 0 {
		return "", "", false
	}
	parent = normalize(q[start+1 : start+1+end])
	if idx := strings.Index(q, "name = '"); idx >= 0 {
		rest := q[idx+len("name = '"):]
		var b strings.Builder
		for i := 0; i < len(rest); i++ {
			if rest[i] == '\\' && i+1 < len(rest) {
				i++
				b.WriteByte(rest[i])
				continue
			}
			if rest[i] == '\'' {
				break
			}
			b.WriteByte(rest[i])
		}
		name = b.String()
	}
	return parent, name, true
}

func (f *fakeDrive) list(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("supportsAllDrives") != "true" {
		driveError(w, http.StatusBadRequest, "sharedDriveFlagMissing")
		return
	}
	parent, name, ok := parseQuery(query.Get("q"))
	if !ok {
		driveError(w, http.StatusBadRequest, "badQuery")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var matched []*record
	for _, rec := range f.files {
		if rec.trashed || len(rec.parents) == 0 || rec.parents[0] != parent {
			continue
		}
		if name != "" && rec.name != name {
			continue
		}
		matched = append(matched, rec)
	}
	sortRecords(matched)
	pageSize := 1000
	if v, err := strconv.Atoi(query.Get("pageSize")); err == nil && v > 0 {
		pageSize = v
	}
	if f.pageLimit > 0 && pageSize > f.pageLimit {
		pageSize = f.pageLimit
	}
	offset := 0
	if token := query.Get("pageToken"); token != "" {
		v, err := strconv.Atoi(token)
		if err != nil || v < 0 || v > len(matched) {
			driveError(w, http.StatusBadRequest, "pageTokenInvalid")
			return
		}
		offset = v
	}
	end := offset + pageSize
	if end > len(matched) {
		end = len(matched)
	}
	out := map[string]any{}
	files := make([]map[string]any, 0, end-offset)
	for _, rec := range matched[offset:end] {
		files = append(files, f.fileJSON(rec))
	}
	out["files"] = files
	if end < len(matched) {
		out["nextPageToken"] = strconv.Itoa(end)
	}
	writeJSON(w, http.StatusOK, out)
}

func sortRecords(recs []*record) {
	for i := 1; i < len(recs); i++ {
		for j := i; j > 0 && recs[j].id < recs[j-1].id; j-- {
			recs[j], recs[j-1] = recs[j-1], recs[j]
		}
	}
}

func (f *fakeDrive) create(w http.ResponseWriter, r *http.Request) {
	if !f.requireFields(w, r) {
		return
	}
	var body struct {
		Name     string   `json:"name"`
		MimeType string   `json:"mimeType"`
		Parents  []string `json:"parents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		driveError(w, http.StatusBadRequest, "badBody")
		return
	}
	if len(body.Parents) != 1 || body.Name == "" {
		driveError(w, http.StatusBadRequest, "badBody")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := &record{
		id: f.newID("dir"), name: body.Name, mime: body.MimeType,
		parents: []string{normalize(body.Parents[0])}, version: 1,
		modified: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
	}
	f.files[rec.id] = rec
	writeJSON(w, http.StatusOK, f.fileJSON(rec))
}

func (f *fakeDrive) fileResource(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
	switch {
	case strings.HasSuffix(rest, "/copy") && r.Method == http.MethodPost:
		f.copy(w, r, normalize(strings.TrimSuffix(rest, "/copy")))
		return
	case strings.Contains(rest, "/revisions/") && r.Method == http.MethodGet:
		id, rev, _ := strings.Cut(rest, "/revisions/")
		f.readRevision(w, r, normalize(id), rev)
		return
	}
	id := normalize(rest)
	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Get("alt") == "media" {
			f.readMedia(w, r, id)
			return
		}
		f.mu.Lock()
		rec, ok := f.files[id]
		var payload map[string]any
		if ok {
			payload = f.fileJSON(rec)
		}
		f.mu.Unlock()
		if !ok {
			driveError(w, http.StatusNotFound, "notFound")
			return
		}
		writeJSON(w, http.StatusOK, payload)
	case http.MethodPatch:
		f.patch(w, r, id)
	case http.MethodDelete:
		f.mu.Lock()
		_, ok := f.files[id]
		delete(f.files, id)
		f.mu.Unlock()
		if !ok {
			driveError(w, http.StatusNotFound, "notFound")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDrive) patch(w http.ResponseWriter, r *http.Request, id string) {
	if !f.requireFields(w, r) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.files[id]
	if !ok {
		driveError(w, http.StatusNotFound, "notFound")
		return
	}
	if body.Name != "" {
		rec.name = body.Name
	}
	if add := r.URL.Query().Get("addParents"); add != "" {
		remove := map[string]bool{}
		for _, p := range strings.Split(r.URL.Query().Get("removeParents"), ",") {
			if p != "" {
				remove[normalize(p)] = true
			}
		}
		kept := rec.parents[:0]
		for _, p := range rec.parents {
			if !remove[p] {
				kept = append(kept, p)
			}
		}
		rec.parents = append(kept, normalize(add))
	}
	rec.version++
	writeJSON(w, http.StatusOK, f.fileJSON(rec))
}

func (f *fakeDrive) copy(w http.ResponseWriter, r *http.Request, id string) {
	if !f.requireFields(w, r) {
		return
	}
	var body struct {
		Name    string   `json:"name"`
		Parents []string `json:"parents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Parents) != 1 {
		driveError(w, http.StatusBadRequest, "badBody")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	src, ok := f.files[id]
	if !ok {
		driveError(w, http.StatusNotFound, "notFound")
		return
	}
	f.nextRev++
	dup := &record{
		id: f.newID("copy"), name: body.Name, mime: src.mime,
		parents: []string{normalize(body.Parents[0])}, data: append([]byte(nil), src.data...),
		headRev: fmt.Sprintf("copy-rev-%d", f.nextRev), version: 1, modified: src.modified,
	}
	f.files[dup.id] = dup
	f.revs[dup.id] = map[string][]byte{dup.headRev: dup.data}
	writeJSON(w, http.StatusOK, f.fileJSON(dup))
}

func (f *fakeDrive) readMedia(w http.ResponseWriter, r *http.Request, id string) {
	f.mu.Lock()
	rec, ok := f.files[id]
	var data []byte
	if ok {
		data = append([]byte(nil), rec.data...)
	}
	f.mediaReads++
	ignore := f.ignoreRange
	f.mu.Unlock()
	if !ok {
		driveError(w, http.StatusNotFound, "notFound")
		return
	}
	serveRange(w, r, data, ignore)
}

func (f *fakeDrive) readRevision(w http.ResponseWriter, r *http.Request, id, rev string) {
	f.mu.Lock()
	f.revisionReads++
	purge := f.purgeRevisions
	var data []byte
	ok := false
	if !purge {
		if byRev, exists := f.revs[id]; exists {
			data, ok = byRev[rev]
		}
	}
	ignore := f.ignoreRange
	f.mu.Unlock()
	if !ok {
		driveError(w, http.StatusNotFound, "notFound")
		return
	}
	serveRange(w, r, data, ignore)
}

func serveRange(w http.ResponseWriter, r *http.Request, data []byte, ignoreRange bool) {
	spec := r.Header.Get("Range")
	if spec == "" || ignoreRange {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		return
	}
	value := strings.TrimPrefix(spec, "bytes=")
	startText, endText, _ := strings.Cut(value, "-")
	start, err := strconv.ParseInt(startText, 10, 64)
	if err != nil || start > int64(len(data)) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	end := int64(len(data)) - 1
	if endText != "" {
		if v, err := strconv.ParseInt(endText, 10, 64); err == nil && v < end {
			end = v
		}
	}
	chunk := data[start : end+1]
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
	w.WriteHeader(http.StatusPartialContent)
	w.Write(chunk)
}

func (f *fakeDrive) uploadStart(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	rest := strings.TrimPrefix(r.URL.Path, "/upload/drive/v3/files")
	targetID := normalize(strings.TrimPrefix(rest, "/"))
	switch query.Get("uploadType") {
	case "multipart":
		f.multipartUpload(w, r, targetID)
	case "resumable":
		f.resumableStart(w, r, targetID)
	default:
		driveError(w, http.StatusBadRequest, "badUploadType")
	}
}

func (f *fakeDrive) multipartUpload(w http.ResponseWriter, r *http.Request, targetID string) {
	if !f.requireFields(w, r) {
		return
	}
	contentType := r.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "multipart/related") {
		driveError(w, http.StatusBadRequest, "badContentType")
		return
	}
	metadata, content, err := readRelated(r)
	if err != nil {
		driveError(w, http.StatusBadRequest, "badMultipart")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextRev++
	rev := fmt.Sprintf("put-rev-%d", f.nextRev)
	if targetID != "" {
		rec, ok := f.files[targetID]
		if !ok {
			driveError(w, http.StatusNotFound, "notFound")
			return
		}
		if len(metadata.Parents) != 0 {
			driveError(w, http.StatusBadRequest, "parentsOnUpdate")
			return
		}
		rec.data, rec.headRev, rec.version = content, rev, rec.version+1
		f.revs[rec.id] = map[string][]byte{rev: append([]byte(nil), content...)}
		writeJSON(w, http.StatusOK, f.fileJSON(rec))
		return
	}
	if len(metadata.Parents) != 1 {
		driveError(w, http.StatusBadRequest, "badParents")
		return
	}
	rec := &record{
		id: f.newID("put"), name: metadata.Name, mime: "application/octet-stream",
		parents: []string{normalize(metadata.Parents[0])}, data: content,
		headRev: rev, version: 1, modified: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
	}
	f.files[rec.id] = rec
	f.revs[rec.id] = map[string][]byte{rev: append([]byte(nil), content...)}
	writeJSON(w, http.StatusOK, f.fileJSON(rec))
}

type relatedMetadata struct {
	Name    string   `json:"name"`
	Parents []string `json:"parents"`
}

// readRelated parses multipart/related by hand: net/http's MultipartReader
// only accepts form-data and mixed, and Drive's metadata+media upload is
// related.
func readRelated(r *http.Request) (relatedMetadata, []byte, error) {
	var meta relatedMetadata
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return meta, nil, err
	}
	boundary := params["boundary"]
	if boundary == "" {
		return meta, nil, errors.New("no boundary")
	}
	reader := multipart.NewReader(r.Body, boundary)
	part, err := reader.NextPart()
	if err != nil {
		return meta, nil, err
	}
	if err := json.NewDecoder(part).Decode(&meta); err != nil {
		return meta, nil, err
	}
	part, err = reader.NextPart()
	if err != nil {
		return meta, nil, err
	}
	content, err := io.ReadAll(part)
	return meta, content, err
}

func (f *fakeDrive) resumableStart(w http.ResponseWriter, r *http.Request, targetID string) {
	if !f.requireFields(w, r) {
		return
	}
	size, err := strconv.ParseInt(r.Header.Get("X-Upload-Content-Length"), 10, 64)
	if err != nil || size <= 0 {
		driveError(w, http.StatusBadRequest, "missingUploadLength")
		return
	}
	var body relatedMetadata
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		driveError(w, http.StatusBadRequest, "badBody")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextSession++
	sid := fmt.Sprintf("sess-%d", f.nextSession)
	s := &session{fileID: targetID, name: body.Name, size: size}
	if targetID == "" {
		if len(body.Parents) != 1 {
			driveError(w, http.StatusBadRequest, "badParents")
			return
		}
		s.parent = normalize(body.Parents[0])
	}
	f.sessions[sid] = s
	w.Header().Set("Location", f.baseURL+"/upload/session/"+sid)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (f *fakeDrive) uploadChunk(w http.ResponseWriter, r *http.Request) {
	sid := strings.TrimPrefix(r.URL.Path, "/upload/session/")
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sid]
	if !ok {
		driveError(w, http.StatusNotFound, "sessionGone")
		return
	}
	start, end, total, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil || total != s.size || start != int64(len(s.data)) {
		driveError(w, http.StatusBadRequest, "badContentRange")
		return
	}
	chunk, err := io.ReadAll(r.Body)
	if err != nil || int64(len(chunk)) != end-start+1 {
		driveError(w, http.StatusBadRequest, "shortChunk")
		return
	}
	s.data = append(s.data, chunk...)
	if int64(len(s.data)) < s.size {
		// 308 without a Location: Go's client returns it rather than following.
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(s.data)-1))
		w.WriteHeader(resumeIncomplete)
		return
	}
	f.nextRev++
	rev := fmt.Sprintf("sess-rev-%d", f.nextRev)
	rec := f.files[s.fileID]
	if rec == nil {
		rec = &record{
			id: f.newID("up"), name: s.name, mime: "application/octet-stream",
			parents: []string{s.parent}, modified: time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
		}
		f.files[rec.id] = rec
	}
	rec.data, rec.headRev, rec.version = s.data, rev, rec.version+1
	f.revs[rec.id] = map[string][]byte{rev: append([]byte(nil), s.data...)}
	delete(f.sessions, sid)
	writeJSON(w, http.StatusOK, f.fileJSON(rec))
}

func parseContentRange(value string) (start, end, total int64, err error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "bytes ")
	span, totalText, ok := strings.Cut(value, "/")
	if !ok {
		return 0, 0, 0, errors.New("no total")
	}
	startText, endText, ok := strings.Cut(span, "-")
	if !ok {
		return 0, 0, 0, errors.New("no span")
	}
	if start, err = strconv.ParseInt(startText, 10, 64); err != nil {
		return
	}
	if end, err = strconv.ParseInt(endText, 10, 64); err != nil {
		return
	}
	total, err = strconv.ParseInt(totalText, 10, 64)
	return
}

func (f *fakeDrive) startPageToken(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"startPageToken": "100"})
}

func (f *fakeDrive) changeFeed(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("pageToken")
	if token == "stale" {
		driveError(w, http.StatusGone, "pageTokenInvalid")
		return
	}
	if token != "100" {
		driveError(w, http.StatusBadRequest, "pageTokenInvalid")
		return
	}
	f.mu.Lock()
	changes := f.changes
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"changes": changes, "newStartPageToken": "101"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func driveError(w http.ResponseWriter, status int, reason string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": status, "message": reason,
		"errors": []map[string]any{{"reason": reason}},
	}})
}

func newTestProvider(t *testing.T, f *fakeDrive) (*Provider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.mu.Lock()
	f.baseURL = srv.URL
	f.mu.Unlock()
	p, err := New(Options{
		Name: "gd", APIBase: srv.URL, TokenURL: srv.URL + "/oauth2/token",
		AccessToken: "good-token", RefreshToken: "refresh-token", ClientID: "client",
		PartSize: uploadChunkUnit,
		Client:   httpx.New(httpx.Options{HTTP: srv.Client(), Policy: retry.Policy{MaxAttempts: 1}}),
		Now:      func() time.Time { return time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return p, srv
}

func TestListSkipsItemsWithoutAByteStream(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	entries, next, err := p.List(context.Background(), RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "" {
		t.Fatalf("unexpected page token %q", next)
	}
	got := map[string]provider.Kind{}
	for _, e := range entries {
		got[e.Name] = e.Kind
		if e.ParentID != RootID {
			t.Fatalf("entry %q has parent %q, want the root alias", e.Name, e.ParentID)
		}
	}
	want := map[string]provider.Kind{"Docs": provider.KindDir, "Movie.mkv": provider.KindFile, "Notes.txt": provider.KindFile}
	if len(got) != len(want) {
		t.Fatalf("listing returned %v, want exactly %v", got, want)
	}
	for name, kind := range want {
		if got[name] != kind {
			t.Fatalf("entry %q is %v, want %v", name, got[name], kind)
		}
	}
}

func TestListReportsAVersionThatNamesTheContent(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	entries, _, err := p.List(context.Background(), RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Kind != provider.KindFile {
			continue
		}
		if e.Name == "Movie.mkv" {
			if e.Version != "rev-1" {
				t.Fatalf("Movie.mkv version is %q, want the head revision id", e.Version)
			}
			if e.Size != 16 {
				t.Fatalf("Movie.mkv size is %d, want 16", e.Size)
			}
		}
		if e.Version == "" {
			t.Fatalf("entry %q has no version; the block cache would key two contents alike", e.Name)
		}
	}
}

func TestDuplicateNamesInOnePageFailTheDirectory(t *testing.T) {
	f := newFakeDrive()
	f.files["file-3"] = &record{id: "file-3", name: "Notes.txt", mime: "text/plain",
		parents: []string{rootActualID}, data: []byte("second"), headRev: "rev-9", version: 1}
	p, _ := newTestProvider(t, f)
	_, _, err := p.List(context.Background(), RootID, "")
	if err == nil || !strings.Contains(err.Error(), "more than one item named") {
		t.Fatalf("listing a folder with duplicate names returned %v, want an actionable error", err)
	}
}

func TestDuplicateNamesAcrossPagesFailTheDirectory(t *testing.T) {
	f := newFakeDrive()
	f.files["file-3"] = &record{id: "file-3", name: "Notes.txt", mime: "text/plain",
		parents: []string{rootActualID}, data: []byte("second"), headRev: "rev-9", version: 1}
	// One entry per page puts the two collisions in different responses, which
	// per-page detection alone would miss.
	f.pageLimit = 1
	p, _ := newTestProvider(t, f)
	err := p.ListStream(context.Background(), RootID, func(provider.Entry) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "more than one item named") {
		t.Fatalf("streaming a folder with duplicate names returned %v, want an actionable error", err)
	}
}

func TestListStreamVisitsEveryEntryOnce(t *testing.T) {
	f := newFakeDrive()
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("extra-%d", i)
		f.files[id] = &record{id: id, name: fmt.Sprintf("extra-%d.bin", i), mime: "application/octet-stream",
			parents: []string{rootActualID}, data: []byte("x"), headRev: "r" + id, version: 1}
	}
	f.pageLimit = 2
	p, _ := newTestProvider(t, f)
	seen := map[string]int{}
	if err := p.ListStream(context.Background(), RootID, func(e provider.Entry) error {
		seen[e.Name]++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 10 {
		t.Fatalf("stream visited %d names, want 10", len(seen))
	}
	for name, count := range seen {
		if count != 1 {
			t.Fatalf("entry %q was visited %d times", name, count)
		}
	}
}

func TestListStreamStopsOnVisitorErrorWithoutReplay(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	sentinel := errors.New("stop")
	count := 0
	err := p.ListStream(context.Background(), RootID, func(provider.Entry) error {
		count++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ListStream returned %v, want the visitor error unwrapped", err)
	}
	if count != 1 {
		t.Fatalf("visitor ran %d times after returning an error", count)
	}
}

func TestStatRejectsItemsWithoutAByteStream(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	if _, err := p.Stat(context.Background(), "doc-1"); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("Stat on a Google Doc returned %v, want ErrUnsupported", err)
	}
	if _, err := p.Stat(context.Background(), "short-1"); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("Stat on a shortcut returned %v, want ErrUnsupported", err)
	}
}

func TestStatTreatsTrashedItemsAsMissing(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	if _, err := p.Stat(context.Background(), "trash-1"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("Stat on a trashed file returned %v, want ErrNotFound", err)
	}
}

func TestStatRootReportsADirectory(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	e, err := p.Stat(context.Background(), RootID)
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != provider.KindDir || e.ID != RootID || e.Version == "" {
		t.Fatalf("root stat is %+v, want a versioned directory under the root alias", e)
	}
}

func TestReadRangeIsPinnedToTheRequestedRevision(t *testing.T) {
	f := newFakeDrive()
	p, _ := newTestProvider(t, f)
	body, err := p.ReadRange(context.Background(), "file-1", "rev-1", 4, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "456789" {
		t.Fatalf("read %q, want %q", got, "456789")
	}
	f.mu.Lock()
	revisionReads, mediaReads := f.revisionReads, f.mediaReads
	f.mu.Unlock()
	if revisionReads != 1 || mediaReads != 0 {
		t.Fatalf("read used %d revision and %d media requests; a versioned read must pin the revision", revisionReads, mediaReads)
	}
}

func TestReadRangeRejectsAChangedFileWhenTheRevisionIsGone(t *testing.T) {
	f := newFakeDrive()
	f.purgeRevisions = true
	p, _ := newTestProvider(t, f)
	// The head has moved on: serving its bytes under the old version would put
	// the wrong content in the block cache.
	f.mu.Lock()
	f.files["file-1"].headRev = "rev-later"
	f.files["file-1"].data = []byte("completely different")
	f.mu.Unlock()
	_, err := p.ReadRange(context.Background(), "file-1", "rev-1", 0, 4)
	if !errors.Is(err, provider.ErrConflict) {
		t.Fatalf("read of a purged revision on a changed file returned %v, want ErrConflict", err)
	}
}

func TestReadRangeFallsBackToTheHeadWhenTheVersionStillMatches(t *testing.T) {
	f := newFakeDrive()
	f.purgeRevisions = true
	p, _ := newTestProvider(t, f)
	body, err := p.ReadRange(context.Background(), "file-1", "rev-1", 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if string(got) != "0123" {
		t.Fatalf("read %q, want %q", got, "0123")
	}
	f.mu.Lock()
	mediaReads := f.mediaReads
	f.mu.Unlock()
	if mediaReads != 1 {
		t.Fatalf("expected exactly one media fallback, got %d", mediaReads)
	}
}

func TestReadRangeSurvivesAServerThatIgnoresRange(t *testing.T) {
	f := newFakeDrive()
	f.ignoreRange = true
	p, _ := newTestProvider(t, f)
	body, err := p.ReadRange(context.Background(), "file-1", "rev-1", 10, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if string(got) != "abcd" {
		t.Fatalf("read %q, want %q; a 200 must be trimmed to the requested window", got, "abcd")
	}
}

func TestDownloadURLIsUnsupported(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	if _, err := p.DownloadURL(context.Background(), "file-1"); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("DownloadURL returned %v, want ErrUnsupported", err)
	}
	if p.Capabilities().LinkShareable {
		t.Fatal("capabilities advertise a shareable link the backend cannot produce")
	}
}

func TestPutFileCreatesThenReplacesTheSameName(t *testing.T) {
	f := newFakeDrive()
	p, _ := newTestProvider(t, f)
	ctx := context.Background()
	first, err := p.PutFile(ctx, RootID, "report.bin", strings.NewReader("first"), 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.PutFile(ctx, RootID, "report.bin", strings.NewReader("second!"), 7, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("second put created a new file %q instead of replacing %q; Drive would now hold two children with one name", second.ID, first.ID)
	}
	if second.Size != 7 {
		t.Fatalf("replacement size is %d, want 7", second.Size)
	}
	if second.Version == first.Version {
		t.Fatal("replacement kept the old version; the block cache would serve stale bytes")
	}
}

func TestPutFileRejectsAShortBody(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	if _, err := p.PutFile(context.Background(), RootID, "short.bin", strings.NewReader("ab"), 5, nil); err == nil {
		t.Fatal("PutFile accepted a body shorter than the declared size")
	}
}

func TestResumableUploadCommitsEveryChunk(t *testing.T) {
	f := newFakeDrive()
	p, _ := newTestProvider(t, f)
	ctx := context.Background()
	payload := make([]byte, uploadChunkUnit+7)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	s, err := p.BeginUpload(ctx, RootID, "big.bin", int64(len(payload)), nil)
	if err != nil {
		t.Fatal(err)
	}
	var parts []provider.PartToken
	for idx, off := 0, 0; off < len(payload); idx, off = idx+1, off+uploadChunkUnit {
		end := off + uploadChunkUnit
		if end > len(payload) {
			end = len(payload)
		}
		token, err := p.UploadPart(ctx, s, idx, strings.NewReader(string(payload[off:end])), int64(end-off))
		if err != nil {
			t.Fatalf("chunk %d: %v", idx, err)
		}
		parts = append(parts, token)
	}
	e, err := p.CompleteUpload(ctx, s, parts)
	if err != nil {
		t.Fatal(err)
	}
	if e.Size != int64(len(payload)) {
		t.Fatalf("committed size is %d, want %d", e.Size, len(payload))
	}
	body, err := p.ReadRange(ctx, e.ID, e.Version, 0, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if string(got) != string(payload) {
		t.Fatal("bytes read back do not match the bytes uploaded")
	}
}

func TestUploadPartRejectsAWrongSizedChunk(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	ctx := context.Background()
	s, err := p.BeginUpload(ctx, RootID, "big.bin", 3*uploadChunkUnit, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UploadPart(ctx, s, 0, strings.NewReader("short"), 5); err == nil {
		t.Fatal("UploadPart accepted a chunk smaller than the part size")
	}
}

func TestCompleteUploadRejectsAMissingPart(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	s := provider.UploadSession{ID: "https://example.com/session", PartSize: uploadChunkUnit, Opaque: map[string]string{
		"session_uri": "https://example.com/session", "parent_id": RootID, "name": "big.bin",
		"size": strconv.Itoa(2 * uploadChunkUnit), "part_size": strconv.Itoa(uploadChunkUnit),
	}}
	_, err := p.CompleteUpload(context.Background(), s, []provider.PartToken{{Index: 0, ETag: strconv.Itoa(uploadChunkUnit)}})
	if err == nil || !strings.Contains(err.Error(), "want 2") {
		t.Fatalf("CompleteUpload accepted an incomplete part set: %v", err)
	}
}

func TestResumableUploadReplacesAnExistingName(t *testing.T) {
	f := newFakeDrive()
	p, _ := newTestProvider(t, f)
	ctx := context.Background()
	payload := strings.Repeat("z", uploadChunkUnit)
	s, err := p.BeginUpload(ctx, RootID, "Notes.txt", int64(len(payload)), nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := p.UploadPart(ctx, s, 0, strings.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	e, err := p.CompleteUpload(ctx, s, []provider.PartToken{token})
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "file-2" {
		t.Fatalf("upload created %q instead of replacing the existing Notes.txt", e.ID)
	}
}

func TestMkdirRefusesAnExistingName(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	if _, err := p.Mkdir(context.Background(), RootID, "Docs"); !errors.Is(err, provider.ErrExists) {
		t.Fatalf("Mkdir over an existing folder returned %v, want ErrExists", err)
	}
}

func TestMkdirCreatesUnderTheResolvedRoot(t *testing.T) {
	f := newFakeDrive()
	p, _ := newTestProvider(t, f)
	e, err := p.Mkdir(context.Background(), RootID, "New")
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != provider.KindDir || e.ParentID != RootID {
		t.Fatalf("Mkdir returned %+v, want a directory under the root alias", e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.files[e.ID]
	if rec == nil || len(rec.parents) != 1 || rec.parents[0] != rootActualID {
		t.Fatalf("new folder was stored with parents %v, want the concrete root id", rec.parents)
	}
}

func TestRenameAndMoveKeepASingleParent(t *testing.T) {
	f := newFakeDrive()
	p, _ := newTestProvider(t, f)
	ctx := context.Background()
	if _, err := p.Rename(ctx, "file-2", "Renamed.txt"); err != nil {
		t.Fatal(err)
	}
	e, err := p.Move(ctx, "file-2", "folder-1")
	if err != nil {
		t.Fatal(err)
	}
	if e.ParentID != "folder-1" || e.Name != "Renamed.txt" {
		t.Fatalf("moved entry is %+v, want Renamed.txt under folder-1", e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := f.files["file-2"]
	if len(rec.parents) != 1 || rec.parents[0] != "folder-1" {
		t.Fatalf("file kept parents %v; a move must drop the old parent, not add a second link", rec.parents)
	}
}

func TestCopyIsServerSide(t *testing.T) {
	f := newFakeDrive()
	p, _ := newTestProvider(t, f)
	e, err := p.Copy(context.Background(), "file-1", "folder-1", "Movie copy.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if e.ParentID != "folder-1" || e.Name != "Movie copy.mkv" || e.Size != 16 {
		t.Fatalf("copy returned %+v", e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mediaReads != 0 {
		t.Fatalf("server-side copy downloaded %d times; no bytes should pass through this process", f.mediaReads)
	}
}

func TestDeleteRefusesTheRoot(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	if err := p.Delete(context.Background(), RootID); err == nil {
		t.Fatal("Delete accepted the drive root")
	}
}

func TestDeleteRemovesTheFile(t *testing.T) {
	f := newFakeDrive()
	p, _ := newTestProvider(t, f)
	if err := p.Delete(context.Background(), "file-2"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files["file-2"]; ok {
		t.Fatal("file survived Delete")
	}
}

func TestChangesEstablishesABaselineWithoutReplayingTheDrive(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	events, next, err := p.Changes(context.Background(), "")
	var reset *provider.CursorResetError
	if !errors.As(err, &reset) {
		t.Fatalf("first Changes returned %v, want a cursor reset carrying the baseline", err)
	}
	if reset.Cursor != "100" || len(events) != 0 || next != "" {
		t.Fatalf("baseline returned cursor %q with %d events", reset.Cursor, len(events))
	}
}

func TestChangesReportsUpsertsAndDeletes(t *testing.T) {
	f := newFakeDrive()
	f.changes = []map[string]any{
		{"fileId": "file-2", "removed": false, "file": map[string]any{
			"id": "file-2", "name": "Notes.txt", "mimeType": "text/plain", "size": "5",
			"headRevisionId": "rev-2", "version": "3", "parents": []string{rootActualID},
			"modifiedTime": "2026-09-01T12:00:00Z", "trashed": false,
		}},
		{"fileId": "file-1", "removed": true},
		{"fileId": "doc-1", "removed": false, "file": map[string]any{
			"id": "doc-1", "name": "Design", "mimeType": "application/vnd.google-apps.document",
			"version": "2", "parents": []string{rootActualID}, "modifiedTime": "2026-09-01T12:00:00Z",
		}},
		{"fileId": "trash-1", "removed": false, "file": map[string]any{
			"id": "trash-1", "name": "Gone.txt", "mimeType": "text/plain", "size": "1",
			"headRevisionId": "rev-3", "version": "2", "parents": []string{rootActualID},
			"modifiedTime": "2026-09-01T12:00:00Z", "trashed": true,
		}},
	}
	p, _ := newTestProvider(t, f)
	events, next, err := p.Changes(context.Background(), "100")
	if err != nil {
		t.Fatal(err)
	}
	if next != "101" {
		t.Fatalf("continuation token is %q, want 101", next)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3 (the Google Doc has no byte stream and is skipped)", len(events))
	}
	if events[0].Op != provider.ChangeUpsert || events[0].Entry == nil || events[0].Entry.ParentID != RootID {
		t.Fatalf("first event is %+v, want an upsert under the root alias", events[0])
	}
	if events[1].Op != provider.ChangeDelete || events[1].ID != "file-1" {
		t.Fatalf("second event is %+v, want a delete of file-1", events[1])
	}
	if events[2].Op != provider.ChangeDelete || events[2].ID != "trash-1" {
		t.Fatalf("a trashed file must reach the tree as a delete, got %+v", events[2])
	}
}

func TestChangesRebaselinesOnAnExpiredToken(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	_, _, err := p.Changes(context.Background(), "stale")
	var reset *provider.CursorResetError
	if !errors.As(err, &reset) || reset.Cursor != "100" {
		t.Fatalf("expired page token returned %v, want a fresh baseline", err)
	}
}

func TestExpiredAccessTokenIsRefreshedOnce(t *testing.T) {
	f := newFakeDrive()
	f.accessValid = map[string]bool{}
	p, _ := newTestProvider(t, f)
	if _, _, err := p.List(context.Background(), RootID, ""); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refreshCalls != 1 {
		t.Fatalf("token was refreshed %d times, want exactly 1", f.refreshCalls)
	}
}

func TestRotatedRefreshTokenIsPersistedBeforeTheNextRequest(t *testing.T) {
	f := newFakeDrive()
	f.accessValid = map[string]bool{}
	f.tokenRotates = true
	p, _ := newTestProvider(t, f)
	var saved map[string]string
	p.SetTokenPersister(func(fields map[string]string) error {
		saved = map[string]string{}
		for k, v := range fields {
			saved[k] = v
		}
		return nil
	})
	if _, _, err := p.List(context.Background(), RootID, ""); err != nil {
		t.Fatal(err)
	}
	if saved["refresh_token"] != "rotated-refresh" {
		t.Fatalf("rotated refresh token was not persisted, saved=%v", saved)
	}
}

func TestAFailedTokenSaveStopsTheNextRequest(t *testing.T) {
	f := newFakeDrive()
	f.accessValid = map[string]bool{}
	f.tokenRotates = true
	p, _ := newTestProvider(t, f)
	p.SetTokenPersister(func(map[string]string) error { return errors.New("disk full") })
	_, _, err := p.List(context.Background(), RootID, "")
	if err == nil || !strings.Contains(err.Error(), "save rotated refresh token") {
		t.Fatalf("List returned %v; a credential that cannot be saved must not be used silently", err)
	}
}

func TestQuotaAndForbiddenAreClassifiedApart(t *testing.T) {
	cases := []struct {
		status int
		reason string
		want   error
	}{
		{403, "userRateLimitExceeded", provider.ErrRateLimited},
		{403, "insufficientFilePermissions", provider.ErrAuth},
		{404, "notFound", provider.ErrNotFound},
		{429, "rateLimitExceeded", provider.ErrRateLimited},
		{410, "pageTokenInvalid", provider.ErrCursorReset},
		{503, "backendError", provider.ErrTransient},
	}
	for _, tc := range cases {
		body := fmt.Sprintf(`{"error":{"code":%d,"errors":[{"reason":%q}]}}`, tc.status, tc.reason)
		err := mapError(&httpx.StatusError{Code: tc.status, Body: body})
		if !errors.Is(err, tc.want) {
			t.Fatalf("HTTP %d/%s mapped to %v, want %v", tc.status, tc.reason, err, tc.want)
		}
	}
}

func TestResourceIDsCannotEscapeTheSearchExpression(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	// A quote in the id would otherwise close the literal and let the rest of
	// the value become query syntax.
	if _, _, err := p.List(context.Background(), "abc' or name != '", ""); err == nil {
		t.Fatal("List accepted a directory id containing a quote")
	}
	if _, err := p.Stat(context.Background(), "../../etc/passwd"); err == nil {
		t.Fatal("Stat accepted a traversal-shaped id")
	}
}

func TestNamesAreCheckedBeforeTheyReachTheAPI(t *testing.T) {
	p, _ := newTestProvider(t, newFakeDrive())
	ctx := context.Background()
	for _, name := range []string{"", ".", "..", "a/b", "nul\x00", "line\nbreak"} {
		if _, err := p.Mkdir(ctx, RootID, name); err == nil {
			t.Fatalf("Mkdir accepted unsafe name %q", name)
		}
	}
}

func TestQuoteLiteralEscapesDriveSyntax(t *testing.T) {
	if got := quoteLiteral(`it's a \ test`); got != `'it\'s a \\ test'` {
		t.Fatalf("quoteLiteral produced %s", got)
	}
}

func TestRangeAcknowledgementIsInclusive(t *testing.T) {
	if !rangeEndsAt("bytes=0-262143", 262144) {
		t.Fatal("an inclusive acknowledgement of the first chunk was rejected")
	}
	if rangeEndsAt("bytes=0-262143", 262143) {
		t.Fatal("an off-by-one acknowledgement was accepted")
	}
	if rangeEndsAt("", 1) || rangeEndsAt("bytes=0-abc", 1) {
		t.Fatal("a malformed acknowledgement was accepted")
	}
}

func TestNewRejectsUnusableConfiguration(t *testing.T) {
	client := httpx.New(httpx.Options{})
	cases := []struct {
		name string
		opt  Options
	}{
		{"no client", Options{AccessToken: "x"}},
		{"no credentials", Options{Client: client}},
		{"refresh without client id", Options{Client: client, RefreshToken: "r"}},
		{"part size not a chunk multiple", Options{Client: client, AccessToken: "x", PartSize: uploadChunkUnit + 1}},
		{"api base with query", Options{Client: client, AccessToken: "x", APIBase: "https://example.com/?a=1"}},
		{"api base with credentials", Options{Client: client, AccessToken: "x", APIBase: "https://u:p@example.com"}},
		{"token url with query", Options{Client: client, AccessToken: "x", TokenURL: "https://example.com/t?x=1"}},
		{"newline in token", Options{Client: client, AccessToken: "x\ny"}},
	}
	for _, tc := range cases {
		if _, err := New(tc.opt); err == nil {
			t.Fatalf("New accepted %s", tc.name)
		}
	}
}

func TestFactoryUsesTheSharedHTTPClient(t *testing.T) {
	f := newFakeDrive()
	srv := httptest.NewServer(f)
	defer srv.Close()
	f.mu.Lock()
	f.baseURL = srv.URL
	f.mu.Unlock()
	shared := httpx.New(httpx.Options{HTTP: srv.Client(), Policy: retry.Policy{MaxAttempts: 1}})
	built, err := Factory("gd", map[string]any{
		"api_base": srv.URL, "token_url": srv.URL + "/oauth2/token",
		"access_token": "good-token", "part_size": "256KiB",
		provider.ConfigHTTPClient: shared,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := built.(*Provider)
	if p.client != shared {
		t.Fatal("factory built its own client instead of using the injected one; proxy rules and rate limits would not apply")
	}
	if _, _, err := p.List(context.Background(), RootID, ""); err != nil {
		t.Fatal(err)
	}
}

func TestFactoryRejectsABadPartSize(t *testing.T) {
	if _, err := Factory("gd", map[string]any{"access_token": "x", "part_size": "banana"}); err == nil {
		t.Fatal("factory accepted a non-numeric part_size")
	}
	if _, err := Factory("gd", map[string]any{"access_token": "x", "part_size": "3KiB"}); err == nil {
		t.Fatal("factory accepted a part_size that is not a chunk multiple")
	}
}

func TestRegisteredUnderItsOwnType(t *testing.T) {
	found := false
	for _, typ := range provider.Types() {
		if typ == "gdrive" {
			found = true
		}
	}
	if !found {
		t.Fatal("gdrive is not registered; main.go's blank import would link nothing")
	}
}
