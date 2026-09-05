package box

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
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

type item struct {
	id, kind, name string
	parent         string
	data           []byte
	versionID      string
	etag           int
	status         string
	modified       time.Time
}

type upSession struct {
	id, fileID, parent, name string
	size, partSize           int64
	parts                    map[int64][]byte
}

// fakeBox is a stateful stand-in for the Box Content API. Files and folders
// live in separate maps precisely because Box's id namespaces are separate:
// a driver that dropped the type prefix would start hitting the wrong map.
type fakeBox struct {
	mu       sync.Mutex
	files    map[string]*item
	folders  map[string]*item
	sessions map[string]*upSession

	nextID, nextVersion, nextSession int
	refreshCalls                     int
	tokenRotates                     bool
	accessValid                      map[string]bool
	pageLimit                        int
	commitPending                    int
	ignoreRange                      bool
	contentReads                     int
}

func newFakeBox() *fakeBox {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	return &fakeBox{
		folders: map[string]*item{
			"0": {id: "0", kind: "folder", name: "All Files", etag: 1, modified: now},
			// Deliberately shares its number with a file: Box allows it and a
			// driver that ignored the type prefix would confuse the two.
			"1": {id: "1", kind: "folder", name: "Docs", parent: "0", etag: 1, modified: now},
		},
		files: map[string]*item{
			"1": {id: "1", kind: "file", name: "Movie.mkv", parent: "0", data: []byte("0123456789abcdef"), versionID: "900", etag: 1, modified: now},
			"2": {id: "2", kind: "file", name: "Notes.txt", parent: "0", data: []byte("hello"), versionID: "901", etag: 1, modified: now},
			"3": {id: "3", kind: "file", name: "Gone.txt", parent: "0", data: []byte("x"), versionID: "902", etag: 1, status: "trashed", modified: now},
		},
		sessions:    map[string]*upSession{},
		nextID:      100,
		nextVersion: 950,
		accessValid: map[string]bool{"good-token": true},
	}
}

func (f *fakeBox) newID() string {
	f.nextID++
	return strconv.Itoa(f.nextID)
}

func (f *fakeBox) newVersion() string {
	f.nextVersion++
	return strconv.Itoa(f.nextVersion)
}

func (f *fakeBox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/oauth2/token" {
		f.oauth(w, r)
		return
	}
	if !f.authorized(r) {
		boxError(w, http.StatusUnauthorized, "unauthorized", nil)
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/upload/2.0/"):
		f.upload(w, r, strings.TrimPrefix(r.URL.Path, "/upload/2.0"))
	case strings.HasPrefix(r.URL.Path, "/2.0/"):
		f.api(w, r, strings.TrimPrefix(r.URL.Path, "/2.0"))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeBox) authorized(r *http.Request) bool {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accessValid[token]
}

func (f *fakeBox) oauth(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" {
		boxError(w, http.StatusBadRequest, "invalid_grant", nil)
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

func (f *fakeBox) itemJSON(it *item) map[string]any {
	out := map[string]any{
		"id": it.id, "type": it.kind, "name": it.name,
		"etag":        strconv.Itoa(it.etag),
		"modified_at": it.modified.Format(time.RFC3339),
		"item_status": it.status,
	}
	if it.parent != "" {
		out["parent"] = map[string]any{"id": it.parent, "type": "folder"}
	}
	if it.kind == "file" {
		sum := sha1.Sum(it.data)
		out["size"] = len(it.data)
		out["sha1"] = hex.EncodeToString(sum[:])
		out["file_version"] = map[string]any{"id": it.versionID, "type": "file_version", "sha1": hex.EncodeToString(sum[:])}
	}
	return out
}

func (f *fakeBox) api(w http.ResponseWriter, r *http.Request, rest string) {
	switch {
	case rest == "/folders" && r.Method == http.MethodPost:
		f.createFolder(w, r)
	case strings.HasPrefix(rest, "/folders/") && strings.HasSuffix(rest, "/items") && r.Method == http.MethodGet:
		f.listItems(w, r, strings.TrimSuffix(strings.TrimPrefix(rest, "/folders/"), "/items"))
	case strings.HasPrefix(rest, "/folders/") && strings.HasSuffix(rest, "/copy") && r.Method == http.MethodPost:
		f.copyItem(w, r, "folder", strings.TrimSuffix(strings.TrimPrefix(rest, "/folders/"), "/copy"))
	case strings.HasPrefix(rest, "/files/") && strings.HasSuffix(rest, "/copy") && r.Method == http.MethodPost:
		f.copyItem(w, r, "file", strings.TrimSuffix(strings.TrimPrefix(rest, "/files/"), "/copy"))
	case strings.HasPrefix(rest, "/files/") && strings.HasSuffix(rest, "/content") && r.Method == http.MethodGet:
		f.content(w, r, strings.TrimSuffix(strings.TrimPrefix(rest, "/files/"), "/content"))
	case strings.HasPrefix(rest, "/folders/"):
		f.itemResource(w, r, "folder", strings.TrimPrefix(rest, "/folders/"))
	case strings.HasPrefix(rest, "/files/"):
		f.itemResource(w, r, "file", strings.TrimPrefix(rest, "/files/"))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeBox) collection(kind string) map[string]*item {
	if kind == "folder" {
		return f.folders
	}
	return f.files
}

func (f *fakeBox) itemResource(w http.ResponseWriter, r *http.Request, kind, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	store := f.collection(kind)
	it, ok := store[id]
	if !ok {
		boxError(w, http.StatusNotFound, "not_found", nil)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, f.itemJSON(it))
	case http.MethodPut:
		var body struct {
			Name   string `json:"name"`
			Parent *struct {
				ID string `json:"id"`
			} `json:"parent"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			boxError(w, http.StatusBadRequest, "bad_request", nil)
			return
		}
		if body.Name != "" {
			it.name = body.Name
		}
		if body.Parent != nil {
			it.parent = body.Parent.ID
		}
		it.etag++
		writeJSON(w, http.StatusOK, f.itemJSON(it))
	case http.MethodDelete:
		if kind == "folder" && r.URL.Query().Get("recursive") != "true" {
			boxError(w, http.StatusBadRequest, "folder_not_empty", nil)
			return
		}
		delete(store, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeBox) listItems(w http.ResponseWriter, r *http.Request, folderID string) {
	if r.URL.Query().Get("usemarker") != "true" {
		boxError(w, http.StatusBadRequest, "marker_required", nil)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.folders[folderID]; !ok {
		boxError(w, http.StatusNotFound, "not_found", nil)
		return
	}
	var matched []*item
	for _, store := range []map[string]*item{f.folders, f.files} {
		for _, it := range store {
			if it.parent == folderID && it.status != "trashed" {
				matched = append(matched, it)
			}
		}
	}
	// A web link has no bytes; the driver must skip it rather than mount it.
	matched = append(matched, &item{id: "77", kind: "web_link", name: "Bookmark", parent: folderID})
	sortItems(matched)
	limit := 1000
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if f.pageLimit > 0 && limit > f.pageLimit {
		limit = f.pageLimit
	}
	offset := 0
	if marker := r.URL.Query().Get("marker"); marker != "" {
		v, err := strconv.Atoi(marker)
		if err != nil || v < 0 || v > len(matched) {
			boxError(w, http.StatusBadRequest, "invalid_marker", nil)
			return
		}
		offset = v
	}
	end := offset + limit
	if end > len(matched) {
		end = len(matched)
	}
	entries := make([]map[string]any, 0, end-offset)
	for _, it := range matched[offset:end] {
		entries = append(entries, f.itemJSON(it))
	}
	out := map[string]any{"entries": entries, "total_count": len(matched)}
	if end < len(matched) {
		out["next_marker"] = strconv.Itoa(end)
	}
	writeJSON(w, http.StatusOK, out)
}

func sortItems(items []*item) {
	key := func(it *item) string { return it.kind + "/" + it.id }
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && key(items[j]) < key(items[j-1]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func (f *fakeBox) createFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Parent struct {
			ID string `json:"id"`
		} `json:"parent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		boxError(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range f.folders {
		if it.parent == body.Parent.ID && it.name == body.Name {
			boxError(w, http.StatusConflict, "item_name_in_use", []map[string]any{{"id": it.id, "type": "folder"}})
			return
		}
	}
	it := &item{id: f.newID(), kind: "folder", name: body.Name, parent: body.Parent.ID, etag: 1,
		modified: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)}
	f.folders[it.id] = it
	writeJSON(w, http.StatusCreated, f.itemJSON(it))
}

func (f *fakeBox) copyItem(w http.ResponseWriter, r *http.Request, kind, id string) {
	var body struct {
		Name   string `json:"name"`
		Parent struct {
			ID string `json:"id"`
		} `json:"parent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		boxError(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	src, ok := f.collection(kind)[id]
	if !ok {
		boxError(w, http.StatusNotFound, "not_found", nil)
		return
	}
	dup := &item{id: f.newID(), kind: kind, name: body.Name, parent: body.Parent.ID,
		data: append([]byte(nil), src.data...), versionID: f.newVersion(), etag: 1, modified: src.modified}
	f.collection(kind)[dup.id] = dup
	writeJSON(w, http.StatusCreated, f.itemJSON(dup))
}

func (f *fakeBox) content(w http.ResponseWriter, r *http.Request, id string) {
	f.mu.Lock()
	it, ok := f.files[id]
	var data []byte
	current := ""
	if ok {
		data = append([]byte(nil), it.data...)
		current = it.versionID
	}
	f.contentReads++
	ignore := f.ignoreRange
	f.mu.Unlock()
	if !ok {
		boxError(w, http.StatusNotFound, "not_found", nil)
		return
	}
	if want := r.URL.Query().Get("version"); want != "" && want != current {
		boxError(w, http.StatusNotFound, "version_not_found", nil)
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

func (f *fakeBox) upload(w http.ResponseWriter, r *http.Request, rest string) {
	switch {
	case rest == "/files/content" && r.Method == http.MethodPost:
		f.simpleUpload(w, r, "")
	case rest == "/files/upload_sessions" && r.Method == http.MethodPost:
		f.startSession(w, r, "")
	case strings.HasPrefix(rest, "/files/upload_sessions/") && strings.HasSuffix(rest, "/commit") && r.Method == http.MethodPost:
		f.commitSession(w, r, strings.TrimSuffix(strings.TrimPrefix(rest, "/files/upload_sessions/"), "/commit"))
	case strings.HasPrefix(rest, "/files/upload_sessions/") && r.Method == http.MethodPut:
		f.uploadChunk(w, r, strings.TrimPrefix(rest, "/files/upload_sessions/"))
	case strings.HasPrefix(rest, "/files/") && strings.HasSuffix(rest, "/upload_sessions") && r.Method == http.MethodPost:
		f.startSession(w, r, strings.TrimSuffix(strings.TrimPrefix(rest, "/files/"), "/upload_sessions"))
	case strings.HasPrefix(rest, "/files/") && strings.HasSuffix(rest, "/content") && r.Method == http.MethodPost:
		f.simpleUpload(w, r, strings.TrimSuffix(strings.TrimPrefix(rest, "/files/"), "/content"))
	default:
		http.NotFound(w, r)
	}
}

type attributes struct {
	Name   string `json:"name"`
	Parent *struct {
		ID string `json:"id"`
	} `json:"parent"`
}

func readUploadForm(r *http.Request) (attributes, []byte, error) {
	var attrs attributes
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return attrs, nil, err
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	form, err := reader.ReadForm(64 << 20)
	if err != nil {
		return attrs, nil, err
	}
	defer form.RemoveAll()
	values := form.Value["attributes"]
	if len(values) != 1 {
		return attrs, nil, errors.New("missing attributes")
	}
	if err := json.Unmarshal([]byte(values[0]), &attrs); err != nil {
		return attrs, nil, err
	}
	files := form.File["file"]
	if len(files) != 1 {
		return attrs, nil, errors.New("missing file part")
	}
	fh, err := files[0].Open()
	if err != nil {
		return attrs, nil, err
	}
	defer fh.Close()
	content, err := io.ReadAll(fh)
	return attrs, content, err
}

func (f *fakeBox) simpleUpload(w http.ResponseWriter, r *http.Request, targetID string) {
	attrs, content, err := readUploadForm(r)
	if err != nil {
		boxError(w, http.StatusBadRequest, "bad_multipart", nil)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if targetID != "" {
		it, ok := f.files[targetID]
		if !ok {
			boxError(w, http.StatusNotFound, "not_found", nil)
			return
		}
		if attrs.Parent != nil {
			boxError(w, http.StatusBadRequest, "parent_on_new_version", nil)
			return
		}
		it.data, it.versionID, it.etag = content, f.newVersion(), it.etag+1
		writeJSON(w, http.StatusCreated, map[string]any{"entries": []any{f.itemJSON(it)}})
		return
	}
	if attrs.Parent == nil {
		boxError(w, http.StatusBadRequest, "parent_required", nil)
		return
	}
	for _, it := range f.files {
		if it.parent == attrs.Parent.ID && it.name == attrs.Name && it.status != "trashed" {
			boxError(w, http.StatusConflict, "item_name_in_use", map[string]any{"id": it.id, "type": "file"})
			return
		}
	}
	it := &item{id: f.newID(), kind: "file", name: attrs.Name, parent: attrs.Parent.ID,
		data: content, versionID: f.newVersion(), etag: 1,
		modified: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)}
	f.files[it.id] = it
	writeJSON(w, http.StatusCreated, map[string]any{"entries": []any{f.itemJSON(it)}})
}

func (f *fakeBox) startSession(w http.ResponseWriter, r *http.Request, fileID string) {
	var body struct {
		FolderID string `json:"folder_id"`
		FileSize int64  `json:"file_size"`
		FileName string `json:"file_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.FileSize < sessionMinSize {
		boxError(w, http.StatusBadRequest, "invalid_file_size", nil)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextSession++
	partSize := int64(8 << 20)
	s := &upSession{
		id: fmt.Sprintf("sess-%d", f.nextSession), fileID: fileID, parent: body.FolderID,
		name: body.FileName, size: body.FileSize, partSize: partSize, parts: map[int64][]byte{},
	}
	f.sessions[s.id] = s
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": s.id, "part_size": partSize,
		"total_parts": (body.FileSize + partSize - 1) / partSize,
	})
}

func (f *fakeBox) uploadChunk(w http.ResponseWriter, r *http.Request, sid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sid]
	if !ok {
		boxError(w, http.StatusNotFound, "session_not_found", nil)
		return
	}
	start, end, total, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil || total != s.size {
		boxError(w, http.StatusBadRequest, "bad_content_range", nil)
		return
	}
	chunk, err := io.ReadAll(r.Body)
	if err != nil || int64(len(chunk)) != end-start+1 {
		boxError(w, http.StatusBadRequest, "short_chunk", nil)
		return
	}
	sum := sha1.Sum(chunk)
	want := "sha=" + base64.StdEncoding.EncodeToString(sum[:])
	if r.Header.Get("Digest") != want {
		boxError(w, http.StatusBadRequest, "digest_mismatch", nil)
		return
	}
	s.parts[start] = chunk
	writeJSON(w, http.StatusOK, map[string]any{"part": map[string]any{
		"part_id": fmt.Sprintf("P%d", start), "offset": start, "size": len(chunk),
		"sha1": base64.StdEncoding.EncodeToString(sum[:]),
	}})
}

func (f *fakeBox) commitSession(w http.ResponseWriter, r *http.Request, sid string) {
	var body struct {
		Parts []uploadedPart `json:"parts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		boxError(w, http.StatusBadRequest, "bad_request", nil)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sid]
	if !ok {
		boxError(w, http.StatusNotFound, "session_not_found", nil)
		return
	}
	if f.commitPending > 0 {
		f.commitPending--
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var assembled []byte
	for _, part := range body.Parts {
		chunk, ok := s.parts[part.Offset]
		if !ok || int64(len(chunk)) != part.Size {
			boxError(w, http.StatusBadRequest, "part_missing", nil)
			return
		}
		assembled = append(assembled, chunk...)
	}
	if int64(len(assembled)) != s.size {
		boxError(w, http.StatusBadRequest, "size_mismatch", nil)
		return
	}
	sum := sha1.Sum(assembled)
	if r.Header.Get("Digest") != "sha="+base64.StdEncoding.EncodeToString(sum[:]) {
		boxError(w, http.StatusPreconditionFailed, "digest_mismatch", nil)
		return
	}
	it := f.files[s.fileID]
	if it == nil {
		it = &item{id: f.newID(), kind: "file", name: s.name, parent: s.parent,
			modified: time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)}
		f.files[it.id] = it
	}
	it.data, it.versionID, it.etag = assembled, f.newVersion(), it.etag+1
	delete(f.sessions, sid)
	writeJSON(w, http.StatusCreated, map[string]any{"entries": []any{f.itemJSON(it)}})
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func boxError(w http.ResponseWriter, status int, code string, conflicts any) {
	body := map[string]any{"type": "error", "status": status, "code": code}
	if conflicts != nil {
		body["context_info"] = map[string]any{"conflicts": conflicts}
	}
	writeJSON(w, status, body)
}

func newTestProvider(t *testing.T, f *fakeBox) *Provider {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p, err := New(Options{
		Name: "bx", APIBase: srv.URL + "/2.0", UploadBase: srv.URL + "/upload/2.0",
		TokenURL:    srv.URL + "/oauth2/token",
		AccessToken: "good-token", RefreshToken: "refresh-token",
		ClientID: "client", ClientSecret: "secret",
		Client: httpx.New(httpx.Options{HTTP: srv.Client(), Policy: retry.Policy{MaxAttempts: 1}}),
		Now:    func() time.Time { return time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIDsCarryTheBoxNamespace(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	entries, _, err := p.List(context.Background(), RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]provider.Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	folder, file := byName["Docs"], byName["Movie.mkv"]
	if folder.ID != "d:1" || file.ID != "f:1" {
		t.Fatalf("folder id %q and file id %q must not collide; Box numbers them separately", folder.ID, file.ID)
	}
	// Reading the folder id as a file would silently hit the wrong endpoint.
	if _, err := p.ReadRange(context.Background(), folder.ID, "", 0, 1); err == nil {
		t.Fatal("ReadRange accepted a folder id")
	}
}

func TestListSkipsWebLinksAndTrashedItems(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	entries, next, err := p.List(context.Background(), RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "" {
		t.Fatalf("unexpected marker %q", next)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
		if e.ParentID != RootID {
			t.Fatalf("entry %q reports parent %q", e.Name, e.ParentID)
		}
	}
	if names["Bookmark"] {
		t.Fatal("a web link has no byte stream and must not be listed")
	}
	if names["Gone.txt"] {
		t.Fatal("a trashed file must not be listed")
	}
	if len(names) != 3 {
		t.Fatalf("listing returned %v, want Docs, Movie.mkv and Notes.txt", names)
	}
}

func TestListStreamPagesThroughMarkers(t *testing.T) {
	f := newFakeBox()
	f.pageLimit = 1
	p := newTestProvider(t, f)
	seen := map[string]int{}
	if err := p.ListStream(context.Background(), RootID, func(e provider.Entry) error {
		seen[e.Name]++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Fatalf("stream visited %v, want three entries", seen)
	}
	for name, count := range seen {
		if count != 1 {
			t.Fatalf("entry %q visited %d times", name, count)
		}
	}
}

func TestListStreamStopsOnVisitorError(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	sentinel := errors.New("stop")
	calls := 0
	err := p.ListStream(context.Background(), RootID, func(provider.Entry) error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) || calls != 1 {
		t.Fatalf("ListStream returned %v after %d visits", err, calls)
	}
}

func TestStatDistinguishesFilesFromFoldersWithTheSameNumber(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	ctx := context.Background()
	folder, err := p.Stat(ctx, "d:1")
	if err != nil {
		t.Fatal(err)
	}
	file, err := p.Stat(ctx, "f:1")
	if err != nil {
		t.Fatal(err)
	}
	if folder.Kind != provider.KindDir || folder.Name != "Docs" {
		t.Fatalf("d:1 resolved to %+v", folder)
	}
	if file.Kind != provider.KindFile || file.Name != "Movie.mkv" || file.Size != 16 {
		t.Fatalf("f:1 resolved to %+v", file)
	}
}

func TestStatTreatsTrashedItemsAsMissing(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	if _, err := p.Stat(context.Background(), "f:3"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("Stat on a trashed file returned %v, want ErrNotFound", err)
	}
}

func TestStatRejectsAnIDWithoutAType(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	for _, id := range []string{"1", "x:1", "d:abc", "d:1/../2", ""} {
		if _, err := p.Stat(context.Background(), id); err == nil {
			t.Fatalf("Stat accepted malformed id %q", id)
		}
	}
}

func TestReadRangePinsTheFileVersion(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	body, err := p.ReadRange(context.Background(), "f:1", "900", 4, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if string(got) != "456789" {
		t.Fatalf("read %q, want %q", got, "456789")
	}
}

func TestReadRangeRefusesToServeNewerBytesUnderAnOldVersion(t *testing.T) {
	f := newFakeBox()
	p := newTestProvider(t, f)
	f.mu.Lock()
	f.files["1"].data = []byte("completely different content")
	f.files["1"].versionID = "999"
	f.mu.Unlock()
	_, err := p.ReadRange(context.Background(), "f:1", "900", 0, 4)
	if !errors.Is(err, provider.ErrConflict) {
		t.Fatalf("reading a superseded version returned %v, want ErrConflict", err)
	}
}

func TestReadRangeTrimsAResponseThatIgnoredRange(t *testing.T) {
	f := newFakeBox()
	f.ignoreRange = true
	p := newTestProvider(t, f)
	body, err := p.ReadRange(context.Background(), "f:1", "900", 10, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if string(got) != "abcd" {
		t.Fatalf("read %q, want %q", got, "abcd")
	}
}

func TestDownloadURLIsUnsupported(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	if _, err := p.DownloadURL(context.Background(), "f:1"); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("DownloadURL returned %v, want ErrUnsupported", err)
	}
	if p.Capabilities().LinkShareable {
		t.Fatal("capabilities advertise a shareable link the backend cannot produce")
	}
}

func TestPutFileTurnsANameClashIntoANewVersion(t *testing.T) {
	f := newFakeBox()
	p := newTestProvider(t, f)
	ctx := context.Background()
	first, err := p.PutFile(ctx, RootID, "Notes.txt", strings.NewReader("replacement"), 11, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "f:2" {
		t.Fatalf("upload created %q instead of versioning the existing Notes.txt", first.ID)
	}
	if first.Size != 11 {
		t.Fatalf("new version size is %d, want 11", first.Size)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.files) != 3 {
		t.Fatalf("account now holds %d files; the clash must not create a second Notes.txt", len(f.files))
	}
}

func TestPutFileCreatesANewName(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	e, err := p.PutFile(context.Background(), RootID, "fresh.bin", strings.NewReader("abc"), 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != provider.KindFile || e.Size != 3 || e.ParentID != RootID {
		t.Fatalf("created entry is %+v", e)
	}
	if !strings.HasPrefix(e.ID, "f:") {
		t.Fatalf("created entry id %q lacks the file namespace", e.ID)
	}
}

func TestPutFileRejectsAShortBody(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	if _, err := p.PutFile(context.Background(), RootID, "short.bin", strings.NewReader("ab"), 9, nil); err == nil {
		t.Fatal("PutFile accepted a body shorter than the declared size")
	}
}

func TestPutFileQuotesTheFilenameInThePartHeader(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	// A quote in the name would otherwise terminate the filename parameter and
	// let the rest become header syntax.
	e, err := p.PutFile(context.Background(), RootID, `we"ird.bin`, strings.NewReader("z"), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != `we"ird.bin` {
		t.Fatalf("stored name is %q", e.Name)
	}
}

func chunkedPayload(size int) []byte {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	return payload
}

func runChunkedUpload(t *testing.T, p *Provider, parent, name string, payload []byte) (provider.Entry, error) {
	t.Helper()
	ctx := context.Background()
	sum := sha1.Sum(payload)
	s, err := p.BeginUpload(ctx, parent, name, int64(len(payload)), provider.Hashes{
		provider.HashSHA1: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		return provider.Entry{}, err
	}
	var parts []provider.PartToken
	for idx, off := 0, int64(0); off < int64(len(payload)); idx, off = idx+1, off+s.PartSize {
		end := off + s.PartSize
		if end > int64(len(payload)) {
			end = int64(len(payload))
		}
		token, err := p.UploadPart(ctx, s, idx, strings.NewReader(string(payload[off:end])), end-off)
		if err != nil {
			return provider.Entry{}, fmt.Errorf("chunk %d: %w", idx, err)
		}
		parts = append(parts, token)
	}
	return p.CompleteUpload(ctx, s, parts)
}

func TestChunkedUploadCommitsAgainstTheWholeFileDigest(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	payload := chunkedPayload(sessionMinSize + 1024)
	e, err := runChunkedUpload(t, p, RootID, "large.bin", payload)
	if err != nil {
		t.Fatal(err)
	}
	if e.Size != int64(len(payload)) {
		t.Fatalf("committed size is %d, want %d", e.Size, len(payload))
	}
	body, err := p.ReadRange(context.Background(), e.ID, e.Version, 0, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if string(got) != string(payload) {
		t.Fatal("bytes read back do not match the bytes uploaded")
	}
}

func TestChunkedUploadReplacesAnExistingName(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	payload := chunkedPayload(sessionMinSize)
	e, err := runChunkedUpload(t, p, RootID, "Notes.txt", payload)
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "f:2" {
		t.Fatalf("chunked upload created %q instead of versioning the existing Notes.txt", e.ID)
	}
}

func TestBeginUploadNeedsTheWholeFileHash(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	_, err := p.BeginUpload(context.Background(), RootID, "large.bin", sessionMinSize, nil)
	if err == nil || !strings.Contains(err.Error(), "SHA-1 of the whole file") {
		t.Fatalf("BeginUpload without a hash returned %v; Box cannot commit such a session", err)
	}
}

func TestBeginUploadRejectsFilesBelowTheSessionMinimum(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	sum := sha1.Sum([]byte("x"))
	_, err := p.BeginUpload(context.Background(), RootID, "small.bin", 1024, provider.Hashes{
		provider.HashSHA1: hex.EncodeToString(sum[:]),
	})
	if err == nil || !strings.Contains(err.Error(), "must use PutFile") {
		t.Fatalf("BeginUpload accepted a file Box would refuse a session for: %v", err)
	}
	if p.Capabilities().SinglePutMax != sessionMinSize {
		t.Fatal("SinglePutMax must reach the session minimum or files in between have no upload path")
	}
}

func TestCommitRetriesWhileBoxIsStillAssembling(t *testing.T) {
	f := newFakeBox()
	f.commitPending = 1
	p := newTestProvider(t, f)
	payload := chunkedPayload(sessionMinSize)
	_, err := runChunkedUpload(t, p, RootID, "pending.bin", payload)
	if !errors.Is(err, provider.ErrTransient) {
		t.Fatalf("a 202 commit returned %v, want a transient error the uploader will retry", err)
	}
}

func TestCompleteUploadRejectsATamperedPartToken(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	ctx := context.Background()
	payload := chunkedPayload(sessionMinSize)
	sum := sha1.Sum(payload)
	s, err := p.BeginUpload(ctx, RootID, "tampered.bin", int64(len(payload)), provider.Hashes{
		provider.HashSHA1: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	var parts []provider.PartToken
	for idx, off := 0, int64(0); off < int64(len(payload)); idx, off = idx+1, off+s.PartSize {
		end := off + s.PartSize
		if end > int64(len(payload)) {
			end = int64(len(payload))
		}
		token, err := p.UploadPart(ctx, s, idx, strings.NewReader(string(payload[off:end])), end-off)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, token)
	}
	parts[0].ETag = base64.RawURLEncoding.EncodeToString([]byte(`{"part_id":"P0","offset":99,"size":1}`))
	if _, err := p.CompleteUpload(ctx, s, parts); err == nil {
		t.Fatal("CompleteUpload accepted a part token describing a different window")
	}
}

func TestUploadPartRejectsAWrongSizedChunk(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	sum := sha1.Sum([]byte("x"))
	s, err := p.BeginUpload(context.Background(), RootID, "large.bin", sessionMinSize, provider.Hashes{
		provider.HashSHA1: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UploadPart(context.Background(), s, 0, strings.NewReader("short"), 5); err == nil {
		t.Fatal("UploadPart accepted a chunk that is not the session part size")
	}
}

func TestMkdirReportsAnExistingName(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	if _, err := p.Mkdir(context.Background(), RootID, "Docs"); !errors.Is(err, provider.ErrExists) {
		t.Fatalf("Mkdir over an existing folder returned %v, want ErrExists", err)
	}
}

func TestMkdirCreatesAFolderInTheFolderNamespace(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	e, err := p.Mkdir(context.Background(), RootID, "New")
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != provider.KindDir || !strings.HasPrefix(e.ID, "d:") || e.ParentID != RootID {
		t.Fatalf("Mkdir returned %+v", e)
	}
}

func TestRenameAndMoveUseTheRightEndpoint(t *testing.T) {
	f := newFakeBox()
	p := newTestProvider(t, f)
	ctx := context.Background()
	if _, err := p.Rename(ctx, "f:2", "Renamed.txt"); err != nil {
		t.Fatal(err)
	}
	e, err := p.Move(ctx, "f:2", "d:1")
	if err != nil {
		t.Fatal(err)
	}
	if e.ParentID != "d:1" || e.Name != "Renamed.txt" {
		t.Fatalf("moved entry is %+v", e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.folders["1"].name != "Docs" {
		t.Fatal("the rename hit the folder with the same number instead of the file")
	}
}

func TestMoveRejectsAFileAsDestination(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	if _, err := p.Move(context.Background(), "f:2", "f:1"); err == nil {
		t.Fatal("Move accepted a file as the destination folder")
	}
}

func TestCopyIsServerSide(t *testing.T) {
	f := newFakeBox()
	p := newTestProvider(t, f)
	e, err := p.Copy(context.Background(), "f:1", "d:1", "Movie copy.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if e.ParentID != "d:1" || e.Size != 16 {
		t.Fatalf("copy returned %+v", e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.contentReads != 0 {
		t.Fatalf("server-side copy read content %d times", f.contentReads)
	}
}

func TestDeleteRefusesTheRootAndRecursesForFolders(t *testing.T) {
	f := newFakeBox()
	p := newTestProvider(t, f)
	ctx := context.Background()
	if err := p.Delete(ctx, RootID); err == nil {
		t.Fatal("Delete accepted the root folder")
	}
	// The fake answers 400 unless recursive=true, which is what Box does for a
	// folder that still has children.
	if err := p.Delete(ctx, "d:1"); err != nil {
		t.Fatalf("deleting a folder failed: %v", err)
	}
	if err := p.Delete(ctx, "f:2"); err != nil {
		t.Fatalf("deleting a file failed: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.folders["1"]; ok {
		t.Fatal("folder survived Delete")
	}
	if _, ok := f.files["2"]; ok {
		t.Fatal("file survived Delete")
	}
	if _, ok := f.files["1"]; !ok {
		t.Fatal("deleting folder 1 also removed file 1; the namespaces were confused")
	}
}

func TestExpiredAccessTokenIsRefreshedOnce(t *testing.T) {
	f := newFakeBox()
	f.accessValid = map[string]bool{}
	p := newTestProvider(t, f)
	if _, _, err := p.List(context.Background(), RootID, ""); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refreshCalls != 1 {
		t.Fatalf("token was refreshed %d times, want 1", f.refreshCalls)
	}
}

func TestRotatedRefreshTokenIsPersisted(t *testing.T) {
	f := newFakeBox()
	f.accessValid = map[string]bool{}
	f.tokenRotates = true
	p := newTestProvider(t, f)
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
	// Box invalidates the previous refresh token on every exchange, so losing
	// the new one locks the account out.
	if saved["refresh_token"] != "rotated-refresh" {
		t.Fatalf("rotated refresh token was not persisted, saved=%v", saved)
	}
}

func TestAFailedTokenSaveStopsTheRequest(t *testing.T) {
	f := newFakeBox()
	f.accessValid = map[string]bool{}
	f.tokenRotates = true
	p := newTestProvider(t, f)
	p.SetTokenPersister(func(map[string]string) error { return errors.New("disk full") })
	_, _, err := p.List(context.Background(), RootID, "")
	if err == nil || !strings.Contains(err.Error(), "save rotated refresh token") {
		t.Fatalf("List returned %v; an unsaved rotated credential must not be used", err)
	}
}

func TestErrorsAreClassifiedForRetryAndBreaker(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{401, `{"code":"unauthorized"}`, provider.ErrAuth},
		{403, `{"code":"forbidden"}`, provider.ErrAuth},
		{404, `{"code":"not_found"}`, provider.ErrNotFound},
		{409, `{"code":"item_name_in_use"}`, provider.ErrExists},
		{412, `{"code":"precondition_failed"}`, provider.ErrConflict},
		{429, `{"code":"rate_limit_exceeded"}`, provider.ErrRateLimited},
		{503, `{"code":"unavailable"}`, provider.ErrTransient},
	}
	for _, tc := range cases {
		err := mapError(&httpx.StatusError{Code: tc.status, Body: tc.body})
		if !errors.Is(err, tc.want) {
			t.Fatalf("HTTP %d mapped to %v, want %v", tc.status, err, tc.want)
		}
	}
}

func TestConflictErrorCarriesTheExistingID(t *testing.T) {
	single := mapError(&httpx.StatusError{Code: 409, Body: `{"code":"item_name_in_use","context_info":{"conflicts":{"id":"42","type":"file"}}}`})
	var conflict *conflictError
	if !errors.As(single, &conflict) || conflict.id != "42" {
		t.Fatalf("single conflict parsed as %v", single)
	}
	array := mapError(&httpx.StatusError{Code: 409, Body: `{"code":"item_name_in_use","context_info":{"conflicts":[{"id":"7","type":"folder"}]}}`})
	if !errors.As(array, &conflict) || conflict.id != "7" {
		t.Fatalf("array conflict parsed as %v", array)
	}
	if !errors.Is(single, provider.ErrExists) {
		t.Fatal("a conflict must still read as ErrExists to callers that only check the sentinel")
	}
}

func TestDigestComparisonAcceptsBothEncodings(t *testing.T) {
	sum := sha1.Sum([]byte("payload"))
	if !sameDigest(hex.EncodeToString(sum[:]), sum[:]) {
		t.Fatal("hex digest rejected")
	}
	if !sameDigest(base64.StdEncoding.EncodeToString(sum[:]), sum[:]) {
		t.Fatal("base64 digest rejected")
	}
	if sameDigest("deadbeef", sum[:]) {
		t.Fatal("a mismatched digest was accepted")
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
		{"refresh without client secret", Options{Client: client, RefreshToken: "r", ClientID: "c"}},
		{"api base with query", Options{Client: client, AccessToken: "x", APIBase: "https://example.com/2.0?a=1"}},
		{"upload base with credentials", Options{Client: client, AccessToken: "x", UploadBase: "https://u:p@example.com"}},
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
	f := newFakeBox()
	srv := httptest.NewServer(f)
	defer srv.Close()
	shared := httpx.New(httpx.Options{HTTP: srv.Client(), Policy: retry.Policy{MaxAttempts: 1}})
	built, err := Factory("bx", map[string]any{
		"api_base": srv.URL + "/2.0", "upload_base": srv.URL + "/upload/2.0",
		"token_url": srv.URL + "/oauth2/token", "access_token": "good-token",
		provider.ConfigHTTPClient: shared,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := built.(*Provider)
	if p.client != shared {
		t.Fatal("factory built its own client; proxy rules and rate limits would not apply")
	}
	if _, _, err := p.List(context.Background(), RootID, ""); err != nil {
		t.Fatal(err)
	}
}

func TestRegisteredUnderItsOwnType(t *testing.T) {
	found := false
	for _, typ := range provider.Types() {
		if typ == "box" {
			found = true
		}
	}
	if !found {
		t.Fatal("box is not registered; main.go's blank import would link nothing")
	}
}
