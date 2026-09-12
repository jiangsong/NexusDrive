package onedrive

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
	"cloudfs/internal/vfs"
)

type odRecord struct {
	id, parent, name string
	dir              bool
	data             []byte
	rev              int
}

type odSession struct {
	parent, name string
	data         []byte
}

type fakeGraph struct {
	mu                  sync.Mutex
	serverURL           string
	items               map[string]*odRecord
	sessions            map[string]*odSession
	nextID, nextSession int
	delta               []map[string]any
	deltaGen            int
	resetDelta          bool
	expireDownloadOnce  bool
	badCDNAuth          bool
	refreshCalls        int
	itemGets            map[string]int
}

func newFakeGraph() *fakeGraph {
	return &fakeGraph{
		items: map[string]*odRecord{
			"root-actual": {id: "root-actual", name: "root", dir: true, rev: 1},
			"folder-1":    {id: "folder-1", parent: "root-actual", name: "Docs", dir: true, rev: 1},
			"file-1":      {id: "file-1", parent: "root-actual", name: "Movie.mkv", data: []byte("0123456789abcdef"), rev: 1},
			"file-2":      {id: "file-2", parent: "root-actual", name: "Other.txt", data: []byte("other"), rev: 1},
		},
		sessions: map[string]*odSession{}, nextID: 10, itemGets: map[string]int{},
	}
}

func (f *fakeGraph) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/oauth2/token" {
		f.oauth(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/download/") {
		f.download(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/upload/") {
		f.uploadPart(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer good-token" && r.Header.Get("Authorization") != "Bearer fresh-token" {
		graphError(w, http.StatusUnauthorized, "InvalidAuthenticationToken")
		return
	}
	base := "/v1.0/me/drive"
	if !strings.HasPrefix(r.URL.Path, base) {
		http.NotFound(w, r)
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, base)
	switch {
	case r.Method == http.MethodGet && suffix == "/root":
		f.get(w, "root-actual")
	case r.Method == http.MethodGet && suffix == "/root/children":
		f.children(w, r, "root-actual")
	case r.Method == http.MethodGet && suffix == "/root/delta":
		f.deltaPage(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(suffix, "/items/") && strings.HasSuffix(suffix, "/children"):
		id := strings.TrimSuffix(strings.TrimPrefix(suffix, "/items/"), "/children")
		f.children(w, r, id)
	case r.Method == http.MethodGet && strings.HasPrefix(suffix, "/items/"):
		f.get(w, strings.TrimPrefix(suffix, "/items/"))
	case r.Method == http.MethodPost && (suffix == "/root/children" || strings.HasSuffix(suffix, "/children")):
		parent := "root-actual"
		if suffix != "/root/children" {
			parent = strings.TrimSuffix(strings.TrimPrefix(suffix, "/items/"), "/children")
		}
		f.mkdir(w, r, parent)
	case r.Method == http.MethodPut && strings.HasSuffix(suffix, ":/content"):
		f.putFile(w, r, suffix)
	case r.Method == http.MethodPost && strings.HasSuffix(suffix, ":/createUploadSession"):
		f.startUpload(w, r, suffix)
	case r.Method == http.MethodPatch && strings.HasPrefix(suffix, "/items/"):
		f.patch(w, r, strings.TrimPrefix(suffix, "/items/"))
	case r.Method == http.MethodDelete && strings.HasPrefix(suffix, "/items/"):
		f.delete(w, strings.TrimPrefix(suffix, "/items/"))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGraph) oauth(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.Form.Get("refresh_token") != "refresh" || r.Form.Get("client_id") != "app" {
		graphError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	f.mu.Lock()
	f.refreshCalls++
	f.mu.Unlock()
	writeODJSON(w, map[string]any{"access_token": "fresh-token", "refresh_token": "rotated", "expires_in": 3600})
}

func (f *fakeGraph) item(r *odRecord) map[string]any {
	parentPath := "/drive/root:"
	if r.parent != "root-actual" && r.parent != "" {
		parentPath += "/nested"
	}
	m := map[string]any{
		"id": r.id, "name": r.name, "size": len(r.data),
		"eTag": fmt.Sprintf("etag-%d", r.rev), "cTag": fmt.Sprintf("ctag-%d", r.rev),
		"lastModifiedDateTime": "2026-09-05T00:00:00Z",
		"parentReference":      map[string]any{"id": r.parent, "path": parentPath},
	}
	if r.dir {
		m["folder"] = map[string]any{"childCount": 0}
		m["size"] = 0
	} else {
		sum := sha1.Sum(r.data)
		m["file"] = map[string]any{"hashes": map[string]string{"sha1Hash": strings.ToUpper(hex.EncodeToString(sum[:]))}}
		m["@microsoft.graph.downloadUrl"] = f.serverURL + "/download/" + url.PathEscape(r.id) + "?sig=private-download"
	}
	return m
}

func (f *fakeGraph) get(w http.ResponseWriter, id string) {
	f.mu.Lock()
	f.itemGets[id]++
	r, ok := f.items[id]
	if ok {
		writeODJSON(w, f.item(r))
	}
	f.mu.Unlock()
	if !ok {
		graphError(w, http.StatusNotFound, "itemNotFound")
	}
}

func (f *fakeGraph) children(w http.ResponseWriter, req *http.Request, parent string) {
	f.mu.Lock()
	ids := make([]string, 0)
	for id, item := range f.items {
		if item.parent == parent {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	page := 1
	if req.URL.Query().Get("page") == "2" {
		page = 2
	}
	start, end := 0, len(ids)
	if page == 1 && len(ids) > 1 {
		end = 1
	} else if page == 2 {
		start = 1
	}
	values := make([]map[string]any, 0, end-start)
	for _, id := range ids[start:end] {
		values = append(values, f.item(f.items[id]))
	}
	out := map[string]any{"value": values}
	if end < len(ids) {
		out["@odata.nextLink"] = f.serverURL + req.URL.Path + "?page=2&token=private-cursor"
	}
	f.mu.Unlock()
	writeODJSON(w, out)
}

func decodeODBody(r *http.Request, out any) { _ = json.NewDecoder(r.Body).Decode(out) }

func (f *fakeGraph) mkdir(w http.ResponseWriter, req *http.Request, parent string) {
	var body struct {
		Name string `json:"name"`
	}
	decodeODBody(req, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, item := range f.items {
		if item.parent == parent && strings.EqualFold(item.name, body.Name) {
			graphError(w, http.StatusConflict, "nameAlreadyExists")
			return
		}
	}
	f.nextID++
	id := fmt.Sprintf("item-%d", f.nextID)
	rec := &odRecord{id: id, parent: parent, name: body.Name, dir: true, rev: 1}
	f.items[id] = rec
	writeODJSON(w, f.item(rec))
}

func parseParentName(suffix, terminal string) (string, string, bool) {
	s := strings.TrimSuffix(suffix, terminal)
	if strings.HasPrefix(s, "/root:/") {
		return "root-actual", strings.TrimPrefix(s, "/root:/"), true
	}
	if !strings.HasPrefix(s, "/items/") {
		return "", "", false
	}
	s = strings.TrimPrefix(s, "/items/")
	i := strings.Index(s, ":/")
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+2:], true
}

func (f *fakeGraph) putFile(w http.ResponseWriter, req *http.Request, suffix string) {
	parent, name, ok := parseParentName(suffix, ":/content")
	if !ok {
		graphError(w, http.StatusBadRequest, "invalidRequest")
		return
	}
	body, _ := io.ReadAll(req.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, item := range f.items {
		if item.parent == parent && strings.EqualFold(item.name, name) {
			item.data, item.rev = bytes.Clone(body), item.rev+1
			writeODJSON(w, f.item(item))
			return
		}
	}
	f.nextID++
	id := fmt.Sprintf("item-%d", f.nextID)
	rec := &odRecord{id: id, parent: parent, name: name, data: bytes.Clone(body), rev: 1}
	f.items[id] = rec
	writeODJSON(w, f.item(rec))
}

func (f *fakeGraph) startUpload(w http.ResponseWriter, req *http.Request, suffix string) {
	parent, name, ok := parseParentName(suffix, ":/createUploadSession")
	if !ok {
		graphError(w, http.StatusBadRequest, "invalidRequest")
		return
	}
	var ignored any
	decodeODBody(req, &ignored)
	f.mu.Lock()
	f.nextSession++
	sid := fmt.Sprintf("session-%d", f.nextSession)
	f.sessions[sid] = &odSession{parent: parent, name: name}
	f.mu.Unlock()
	writeODJSON(w, map[string]any{
		"uploadUrl":          f.serverURL + "/upload/" + sid + "?sig=private-upload",
		"expirationDateTime": "2026-09-06T00:00:00Z", "nextExpectedRanges": []string{"0-"},
	})
}

func (f *fakeGraph) uploadPart(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Authorization") != "" {
		f.mu.Lock()
		f.badCDNAuth = true
		f.mu.Unlock()
	}
	sid := strings.TrimPrefix(req.URL.Path, "/upload/")
	var start, end, total int64
	if _, err := fmt.Sscanf(req.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil {
		graphError(w, http.StatusBadRequest, "invalidRange")
		return
	}
	body, _ := io.ReadAll(req.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sid]
	if !ok {
		graphError(w, http.StatusNotFound, "uploadSessionNotFound")
		return
	}
	if int64(len(s.data)) != start || int64(len(body)) != end-start+1 {
		graphError(w, http.StatusRequestedRangeNotSatisfiable, "invalidRange")
		return
	}
	s.data = append(s.data, body...)
	if end+1 < total {
		w.WriteHeader(http.StatusAccepted)
		writeODJSON(w, map[string]any{"nextExpectedRanges": []string{fmt.Sprintf("%d-", end+1)}})
		return
	}
	f.nextID++
	id := fmt.Sprintf("item-%d", f.nextID)
	rec := &odRecord{id: id, parent: s.parent, name: s.name, data: bytes.Clone(s.data), rev: 1}
	f.items[id] = rec
	delete(f.sessions, sid)
	w.WriteHeader(http.StatusCreated)
	writeODJSON(w, f.item(rec))
}

func (f *fakeGraph) patch(w http.ResponseWriter, req *http.Request, id string) {
	var body struct {
		Name   string `json:"name"`
		Parent struct {
			ID string `json:"id"`
		} `json:"parentReference"`
	}
	decodeODBody(req, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.items[id]
	if !ok {
		graphError(w, http.StatusNotFound, "itemNotFound")
		return
	}
	if body.Name != "" {
		rec.name = body.Name
	}
	if body.Parent.ID != "" {
		rec.parent = body.Parent.ID
	}
	rec.rev++
	writeODJSON(w, f.item(rec))
}

func (f *fakeGraph) delete(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[id]; !ok {
		graphError(w, http.StatusNotFound, "itemNotFound")
		return
	}
	delete(f.items, id)
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeGraph) download(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Authorization") != "" {
		f.mu.Lock()
		f.badCDNAuth = true
		f.mu.Unlock()
	}
	id := strings.TrimPrefix(req.URL.Path, "/download/")
	f.mu.Lock()
	if f.expireDownloadOnce {
		f.expireDownloadOnce = false
		f.mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		return
	}
	rec, ok := f.items[id]
	if !ok {
		f.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	body := bytes.Clone(rec.data)
	f.mu.Unlock()
	start, end := 0, len(body)-1
	if raw := req.Header.Get("Range"); raw != "" {
		_, _ = fmt.Sscanf(raw, "bytes=%d-%d", &start, &end)
		if end >= len(body) {
			end = len(body) - 1
		}
		w.WriteHeader(http.StatusPartialContent)
	}
	_, _ = w.Write(body[start : end+1])
}

func (f *fakeGraph) deltaPage(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	if f.resetDelta {
		f.resetDelta = false
		f.deltaGen++
		f.mu.Unlock()
		graphError(w, http.StatusGone, "resyncRequired")
		return
	}
	if req.URL.Query().Get("token") == "latest" {
		link := fmt.Sprintf("%s%s?g=%d&d=%d&token=private-delta", f.serverURL, req.URL.Path, f.deltaGen, len(f.delta))
		f.mu.Unlock()
		writeODJSON(w, map[string]any{"value": []any{}, "@odata.deltaLink": link})
		return
	}
	generation, _ := strconv.Atoi(req.URL.Query().Get("g"))
	offset, _ := strconv.Atoi(req.URL.Query().Get("d"))
	if generation != f.deltaGen || offset < 0 || offset > len(f.delta) {
		f.mu.Unlock()
		graphError(w, http.StatusGone, "resyncRequired")
		return
	}
	values := append([]map[string]any(nil), f.delta[offset:]...)
	link := fmt.Sprintf("%s%s?g=%d&d=%d&token=private-delta", f.serverURL, req.URL.Path, f.deltaGen, len(f.delta))
	f.mu.Unlock()
	writeODJSON(w, map[string]any{"value": values, "@odata.deltaLink": link})
}

func graphError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": "fake error"}})
}

func writeODJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func newODProvider(t *testing.T, f *fakeGraph, srv *httptest.Server) *Provider {
	t.Helper()
	f.serverURL = srv.URL
	p, err := New(Options{
		Name: "od", GraphBase: srv.URL + "/v1.0", AccessToken: "good-token", PartSize: fragmentUnit,
		TokenURL: srv.URL + "/oauth2/token", Client: httpx.New(httpx.Options{HTTP: srv.Client(), Policy: retry.Policy{MaxAttempts: 1}}),
		Now: func() time.Time { return time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLifecycleRangeUploadAndDelta(t *testing.T) {
	fake := newFakeGraph()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	p := newODProvider(t, fake, srv)
	ctx := context.Background()
	if p.RootID() != RootID || !p.Capabilities().Delta || !p.Capabilities().StreamList || p.Capabilities().ServerCopy {
		t.Fatalf("unexpected capabilities: %+v", p.Capabilities())
	}
	page, cursor, err := p.List(ctx, RootID, "")
	if err != nil || len(page) != 1 || cursor == "" {
		t.Fatalf("page 1 = %+v, %q, %v", page, cursor, err)
	}
	page2, next, err := p.List(ctx, RootID, cursor)
	if err != nil || len(page2) != 2 || next != "" {
		t.Fatalf("page 2 = %+v, %q, %v", page2, next, err)
	}
	stop := errors.New("stop listing")
	if err := p.ListStream(ctx, RootID, func(provider.Entry) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("ListStream stop = %v", err)
	}
	file, err := p.Stat(ctx, "file-1")
	if err != nil || file.Version != "ctag-1" || file.Hashes[provider.HashSHA1] == "" {
		t.Fatalf("stat = %+v, %v", file, err)
	}
	fake.mu.Lock()
	fake.expireDownloadOnce = true
	fake.mu.Unlock()
	rc, err := p.ReadRange(ctx, file.ID, file.Version, 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "34567" {
		t.Fatalf("range = %q", body)
	}
	fake.mu.Lock()
	getsAfterRefresh := fake.itemGets[file.ID]
	fake.mu.Unlock()
	rc, err = p.ReadRange(ctx, file.ID, file.Version, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := io.ReadAll(rc)
	_ = rc.Close()
	fake.mu.Lock()
	getsAfterCachedRead := fake.itemGets[file.ID]
	fake.mu.Unlock()
	if string(second) != "01" || getsAfterCachedRead != getsAfterRefresh {
		t.Fatalf("cached range = %q, metadata GETs %d -> %d", second, getsAfterRefresh, getsAfterCachedRead)
	}
	if _, err := p.ReadRange(ctx, file.ID, "stale", 0, 1); !errors.Is(err, provider.ErrConflict) {
		t.Fatalf("stale range = %v", err)
	}
	link, err := p.DownloadURL(ctx, file.ID)
	if err != nil || !strings.Contains(link.URL, "private-download") || !link.ExpiresAt.Equal(time.Date(2026, 9, 5, 0, 15, 0, 0, time.UTC)) {
		t.Fatalf("download link = %+v, %v", link, err)
	}

	put, err := p.PutFile(ctx, RootID, "视频.mp4", strings.NewReader("small"), 5, nil)
	if err != nil || put.Size != 5 || put.Name != "视频.mp4" {
		t.Fatalf("put = %+v, %v", put, err)
	}
	large := bytes.Repeat([]byte("x"), fragmentUnit+7)
	session, err := p.BeginUpload(ctx, RootID, "large.bin", int64(len(large)), nil)
	if err != nil {
		t.Fatal(err)
	}
	t0, err := p.UploadPart(ctx, session, 0, bytes.NewReader(large[:fragmentUnit]), fragmentUnit)
	if err != nil {
		t.Fatal(err)
	}
	resumed := newODProvider(t, fake, srv)
	t1, err := resumed.UploadPart(ctx, session, 1, bytes.NewReader(large[fragmentUnit:]), 7)
	if err != nil {
		t.Fatal(err)
	}
	parts := []provider.PartToken{t1, t0}
	complete, err := resumed.CompleteUpload(ctx, session, parts)
	if err != nil || complete.Size != int64(len(large)) || parts[0].Index != 1 {
		t.Fatalf("complete = %+v, parts=%+v, %v", complete, parts, err)
	}

	dir, err := p.Mkdir(ctx, RootID, "Dest")
	if err != nil || dir.Kind != provider.KindDir {
		t.Fatalf("mkdir = %+v, %v", dir, err)
	}
	renamed, err := p.Rename(ctx, complete.ID, "renamed.bin")
	if err != nil || renamed.Name != "renamed.bin" {
		t.Fatalf("rename = %+v, %v", renamed, err)
	}
	moved, err := p.Move(ctx, renamed.ID, dir.ID)
	if err != nil || moved.ParentID != dir.ID {
		t.Fatalf("move = %+v, %v", moved, err)
	}
	if err := p.Delete(ctx, moved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stat(ctx, moved.ID); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("deleted stat = %v", err)
	}

	_, _, err = p.Changes(ctx, "")
	var baseline *provider.CursorResetError
	if !errors.As(err, &baseline) || baseline.Cursor == "" {
		t.Fatalf("delta baseline = %#v, %v", baseline, err)
	}
	fake.mu.Lock()
	rec := fake.items["file-2"]
	rec.data, rec.rev = []byte("changed"), rec.rev+1
	fake.delta = append(fake.delta, fake.item(rec), map[string]any{
		"id": "file-1", "deleted": map[string]any{}, "parentReference": map[string]any{"id": "root-actual"},
	})
	fake.mu.Unlock()
	changes, deltaNext, err := p.Changes(ctx, baseline.Cursor)
	if err != nil || len(changes) != 2 || changes[0].Entry.Version != "ctag-2" || changes[1].Op != provider.ChangeDelete {
		t.Fatalf("changes = %+v, %q, %v", changes, deltaNext, err)
	}
	fake.mu.Lock()
	fake.resetDelta = true
	fake.mu.Unlock()
	_, _, err = p.Changes(ctx, deltaNext)
	var reset *provider.CursorResetError
	if !errors.As(err, &reset) || reset.Cursor == deltaNext {
		t.Fatalf("delta reset = %#v, %v", reset, err)
	}
	fake.mu.Lock()
	badAuth := fake.badCDNAuth
	fake.mu.Unlock()
	if badAuth {
		t.Fatal("Bearer credential leaked to a preauthenticated download/upload URL")
	}
}

func TestRefreshRotationPersistenceAndFactory(t *testing.T) {
	fake := newFakeGraph()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fake.serverURL = srv.URL
	pAny, err := provider.New("onedrive", "od", map[string]any{
		"graph_base": srv.URL + "/v1.0", "token_url": srv.URL + "/oauth2/token",
		"access_token": "expired", "refresh_token": "refresh", "client_id": "app",
		"part_size": "10MiB", provider.ConfigHTTPClient: httpx.New(httpx.Options{HTTP: srv.Client(), Policy: retry.Policy{MaxAttempts: 1}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	p := pAny.(*Provider)
	var saved map[string]string
	p.SetTokenPersister(func(fields map[string]string) error {
		saved = map[string]string{}
		for k, v := range fields {
			saved[k] = v
		}
		return nil
	})
	if _, err := p.Stat(context.Background(), "file-1"); err != nil {
		t.Fatal(err)
	}
	// Stat rejected the expired token and refreshed it. A redundant caller with
	// the stale token must reuse that result rather than exchange the grant again.
	if _, err := p.refresh(context.Background(), "expired"); err != nil {
		t.Fatal(err)
	}
	if saved["refresh_token"] != "rotated" {
		t.Fatalf("saved credentials = %#v", saved)
	}
	if p.Capabilities().PartSize != 10<<20 {
		t.Fatalf("part size = %d", p.Capabilities().PartSize)
	}
	if _, err := Factory("bad", map[string]any{"access_token": "x", "part_size": "1MiB"}); err == nil {
		t.Fatal("non-320KiB part size should fail")
	}
	if _, err := Factory("bad", map[string]any{"refresh_token": "x"}); err == nil {
		t.Fatal("refresh token without client id should fail")
	}
}

func TestFactoryVFSReadAndDelta(t *testing.T) {
	fake := newFakeGraph()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fake.serverURL = srv.URL
	pAny, err := provider.New("onedrive", "od", map[string]any{
		"graph_base": srv.URL + "/v1.0", "token_url": srv.URL + "/oauth2/token", "access_token": "good-token",
		provider.ConfigHTTPClient: httpx.New(httpx.Options{HTTP: srv.Client(), Policy: retry.Policy{MaxAttempts: 1}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	blocks, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer blocks.Close()
	fs, err := vfs.New(vfs.Options{Meta: store, Cache: blocks, DefaultDirTTL: time.Hour, AttrTTL: time.Hour,
		Mounts: []vfs.Mount{{Prefix: "/", Remote: "od", RootID: RootID, Provider: pAny, Mode: config.ModeReadonly, DirTTL: time.Hour}}})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	if body, err := fs.ReadFileRange(context.Background(), "/Movie.mkv", 0, 0); err != nil || string(body) != "0123456789abcdef" {
		t.Fatalf("VFS read = %q, %v", body, err)
	}
	refresher := vfs.NewRefresher(fs, time.Minute)
	mount := fs.Mounts()[0]
	if _, err := refresher.PollOnce(context.Background(), mount); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadDirPath(context.Background(), "/"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	rec := fake.items["file-1"]
	rec.data, rec.rev = []byte("new movie"), rec.rev+1
	fake.delta = append(fake.delta, fake.item(rec))
	fake.mu.Unlock()
	if applied, err := refresher.PollOnce(context.Background(), mount); err != nil || applied != 1 {
		t.Fatalf("delta apply = %d, %v", applied, err)
	}
	if body, err := fs.ReadFileRange(context.Background(), "/Movie.mkv", 0, 0); err != nil || string(body) != "new movie" {
		t.Fatalf("VFS delta read = %q, %v", body, err)
	}
}

func TestGuardsAndErrorURLRedaction(t *testing.T) {
	if _, err := New(Options{Client: httpx.New(httpx.Options{}), AccessToken: "x", GraphBase: "https://example.com"}); err == nil {
		t.Fatal("graph base without version path should fail")
	}
	if _, err := New(Options{Client: httpx.New(httpx.Options{}), AccessToken: "x", PartSize: fragmentUnit + 1}); err == nil {
		t.Fatal("unaligned part size should fail")
	}
	if _, err := New(Options{Client: httpx.New(httpx.Options{}), AccessToken: "x", GraphBase: "https://example.com/v1.0/../beta"}); err == nil {
		t.Fatal("non-canonical Graph base should fail")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "failed", http.StatusBadRequest)
	}))
	defer server.Close()
	client := httpx.New(httpx.Options{HTTP: server.Client(), Policy: retry.Policy{MaxAttempts: 1}})
	p, err := New(Options{Client: client, AccessToken: "x", GraphBase: server.URL + "/v1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.safeCursor(server.URL + "/v1.0/root/delta?token=private"); err != nil {
		t.Fatalf("valid cursor = %v", err)
	}
	if err := p.safeCursor(server.URL + "/v1.0evil/root/delta"); err == nil {
		t.Fatal("cursor outside configured Graph path should fail")
	}
	if err := p.safeCursor("https://example.invalid/v1.0/root/delta"); err == nil {
		t.Fatal("cross-origin cursor should fail")
	}
	_, err = client.Do(context.Background(), httpx.Request{Method: http.MethodGet, URL: server.URL + "/upload?sig=private-value"})
	if err == nil || strings.Contains(err.Error(), "private-value") {
		t.Fatalf("signed query leaked in error: %v", err)
	}
	var se *httpx.StatusError
	if !errors.As(err, &se) || se.URL != server.URL+"/upload" {
		t.Fatalf("redacted status URL = %#v", se)
	}
}

func TestAFullDriveIsClassifiedAsQuota(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"insufficient storage", 507, `{"error":{"code":"quotaLimitReached","message":"drive is full"}}`},
		{"code without 507", 400, `{"error":{"code":"quotaLimitReached","message":"drive is full"}}`},
		{"507 without envelope", 507, `not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mapError(&httpx.StatusError{Code: tc.status, Body: tc.body})
			if !errors.Is(err, provider.ErrQuotaExceeded) {
				t.Fatalf("mapped to %v, want ErrQuotaExceeded", err)
			}
			if got := retry.Classify(err); got != retry.ClassQuota {
				t.Fatalf("Classify = %v, want quota: a full drive must not be retried", got)
			}
		})
	}
}
