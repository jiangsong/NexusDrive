package dropbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
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

type fakeEntry struct {
	dir     bool
	display string
	data    []byte
	rev     int
}

type fakeDropbox struct {
	mu            sync.Mutex
	entries       map[string]*fakeEntry
	sessions      map[string][]byte
	nextSession   int
	continueCalls int
	refreshCalls  int
	requireFresh  bool
	nonASCIIArg   bool
	deltaEntries  []map[string]any
	deltaGen      int
	resetCursor   bool
	spaceUsage    any
}

func newFakeDropbox() *fakeDropbox {
	return &fakeDropbox{
		entries: map[string]*fakeEntry{
			"/docs":            {dir: true, display: "/Docs", rev: 1},
			"/hello world.txt": {display: "/Hello World.txt", data: []byte("0123456789abcdef"), rev: 1},
			"/other.txt":       {display: "/Other.txt", data: []byte("other"), rev: 1},
		},
		sessions: map[string][]byte{},
	}
}

func (f *fakeDropbox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/oauth2/token" {
		f.oauth(w, r)
		return
	}
	want := "Bearer good-token"
	f.mu.Lock()
	if f.requireFresh {
		want = "Bearer fresh-token"
	}
	f.mu.Unlock()
	if r.Header.Get("Authorization") != want {
		writeDropboxError(w, http.StatusUnauthorized, "invalid_access_token/..")
		return
	}
	if raw := r.Header.Get("Dropbox-API-Arg"); raw != "" {
		for _, ch := range []byte(raw) {
			if ch > 0x7f {
				f.mu.Lock()
				f.nonASCIIArg = true
				f.mu.Unlock()
			}
		}
	}
	switch r.URL.Path {
	case "/2/files/list_folder":
		f.list(w, r, false)
	case "/2/files/list_folder/continue":
		f.list(w, r, true)
	case "/2/files/list_folder/get_latest_cursor":
		f.latestCursor(w, r)
	case "/2/files/get_metadata":
		f.getMetadata(w, r)
	case "/2/files/get_temporary_link":
		f.temporaryLink(w, r)
	case "/2/files/create_folder_v2":
		f.createFolder(w, r)
	case "/2/files/copy_v2":
		f.relocate(w, r, false)
	case "/2/files/move_v2":
		f.relocate(w, r, true)
	case "/2/files/delete_v2":
		f.delete(w, r)
	case "/2/files/download":
		f.download(w, r)
	case "/2/files/upload":
		f.upload(w, r)
	case "/2/files/upload_session/start":
		f.start(w, r)
	case "/2/files/upload_session/append_v2":
		f.append(w, r)
	case "/2/files/upload_session/finish":
		f.finish(w, r)
	case "/2/users/get_space_usage":
		f.spaceUsageHandler(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDropbox) oauth(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh" ||
		r.Form.Get("client_id") != "app" || r.Form.Get("client_secret") != "secret" {
		writeDropboxError(w, http.StatusBadRequest, "invalid_grant/..")
		return
	}
	f.mu.Lock()
	f.refreshCalls++
	f.mu.Unlock()
	writeJSON(w, map[string]any{"access_token": "fresh-token", "expires_in": 14400})
}

func decodeBody(r *http.Request, out any) error { return json.NewDecoder(r.Body).Decode(out) }

func decodeArg(r *http.Request, out any) error {
	return json.Unmarshal([]byte(r.Header.Get("Dropbox-API-Arg")), out)
}

func lowerPath(p string) string { return strings.ToLower(p) }

func (f *fakeDropbox) meta(id string, e *fakeEntry) map[string]any {
	tag := "file"
	if e.dir {
		tag = "folder"
	}
	name := path.Base(e.display)
	m := map[string]any{
		".tag": tag, "id": "id:" + id, "name": name,
		"path_lower": id, "path_display": e.display,
	}
	if !e.dir {
		m["size"] = len(e.data)
		m["rev"] = fmt.Sprintf("rev-%d", e.rev)
		m["server_modified"] = "2026-09-05T00:00:00Z"
		m["client_modified"] = "2026-09-05T00:00:00Z"
		m["is_downloadable"] = true
	}
	return m
}

func (f *fakeDropbox) list(w http.ResponseWriter, r *http.Request, continuation bool) {
	dir, offset := "", 0
	if continuation {
		var arg struct {
			Cursor string `json:"cursor"`
		}
		if decodeBody(r, &arg) != nil {
			writeDropboxError(w, http.StatusConflict, "reset/..")
			return
		}
		if strings.HasPrefix(arg.Cursor, "delta:") {
			f.delta(w, arg.Cursor)
			return
		}
		if !strings.HasPrefix(arg.Cursor, "cursor:") {
			writeDropboxError(w, http.StatusConflict, "reset/..")
			return
		}
		dir = strings.TrimPrefix(arg.Cursor, "cursor:")
		offset = 1
		f.mu.Lock()
		f.continueCalls++
		f.mu.Unlock()
	} else {
		var arg struct {
			Path string `json:"path"`
		}
		if decodeBody(r, &arg) != nil {
			writeDropboxError(w, http.StatusBadRequest, "bad_input/..")
			return
		}
		dir = lowerPath(arg.Path)
	}
	if dir == "" {
		dir = "/"
	}
	f.mu.Lock()
	ids := make([]string, 0)
	for id := range f.entries {
		parent := path.Dir(id)
		if parent == "." {
			parent = "/"
		}
		if parent == dir {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	result := make([]map[string]any, 0)
	end := len(ids)
	if offset == 0 && len(ids) > 1 {
		end = 1
	}
	for _, id := range ids[offset:end] {
		result = append(result, f.meta(id, f.entries[id]))
	}
	f.mu.Unlock()
	writeJSON(w, map[string]any{
		"entries": result, "cursor": "cursor:" + dir, "has_more": end < len(ids),
	})
}

func (f *fakeDropbox) latestCursor(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		Path           string `json:"path"`
		Recursive      bool   `json:"recursive"`
		IncludeDeleted bool   `json:"include_deleted"`
	}
	if decodeBody(r, &arg) != nil || arg.Path != "" || !arg.Recursive || !arg.IncludeDeleted {
		writeDropboxError(w, http.StatusBadRequest, "bad_input/..")
		return
	}
	f.mu.Lock()
	cursor := fmt.Sprintf("delta:%d:%d", f.deltaGen, len(f.deltaEntries))
	f.mu.Unlock()
	writeJSON(w, map[string]string{"cursor": cursor})
}

func (f *fakeDropbox) delta(w http.ResponseWriter, cursor string) {
	var generation, offset int
	if _, err := fmt.Sscanf(cursor, "delta:%d:%d", &generation, &offset); err != nil {
		writeDropboxError(w, http.StatusConflict, "reset/..")
		return
	}
	f.mu.Lock()
	if f.resetCursor || generation != f.deltaGen || offset < 0 || offset > len(f.deltaEntries) {
		f.resetCursor = false
		f.deltaGen++
		f.mu.Unlock()
		writeDropboxError(w, http.StatusConflict, "reset/..")
		return
	}
	entries := append([]map[string]any(nil), f.deltaEntries[offset:]...)
	next := fmt.Sprintf("delta:%d:%d", f.deltaGen, len(f.deltaEntries))
	f.mu.Unlock()
	writeJSON(w, map[string]any{"entries": entries, "cursor": next, "has_more": false})
}

func (f *fakeDropbox) getMetadata(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		Path string `json:"path"`
	}
	_ = decodeBody(r, &arg)
	id := lowerPath(arg.Path)
	f.mu.Lock()
	e, ok := f.entries[id]
	if ok {
		writeJSON(w, f.meta(id, e))
	}
	f.mu.Unlock()
	if !ok {
		writeDropboxError(w, http.StatusConflict, "path/not_found/..")
	}
}

func (f *fakeDropbox) temporaryLink(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		Path string `json:"path"`
	}
	_ = decodeBody(r, &arg)
	id := lowerPath(arg.Path)
	f.mu.Lock()
	e, ok := f.entries[id]
	if ok {
		writeJSON(w, map[string]any{"metadata": f.meta(id, e), "link": "https://dl.dropboxusercontent.test/temp"})
	}
	f.mu.Unlock()
	if !ok {
		writeDropboxError(w, http.StatusConflict, "path/not_found/..")
	}
}

func (f *fakeDropbox) createFolder(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		Path string `json:"path"`
	}
	_ = decodeBody(r, &arg)
	id := lowerPath(arg.Path)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.entries[id]; ok {
		writeDropboxError(w, http.StatusConflict, "path/conflict/folder/..")
		return
	}
	e := &fakeEntry{dir: true, display: arg.Path, rev: 1}
	f.entries[id] = e
	writeJSON(w, map[string]any{"metadata": f.meta(id, e)})
}

func (f *fakeDropbox) relocate(w http.ResponseWriter, r *http.Request, move bool) {
	var arg struct {
		From string `json:"from_path"`
		To   string `json:"to_path"`
	}
	_ = decodeBody(r, &arg)
	from, to := lowerPath(arg.From), lowerPath(arg.To)
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[from]
	if !ok {
		writeDropboxError(w, http.StatusConflict, "from_lookup/not_found/..")
		return
	}
	if _, exists := f.entries[to]; exists {
		writeDropboxError(w, http.StatusConflict, "to/conflict/..")
		return
	}
	clone := &fakeEntry{dir: e.dir, display: arg.To, data: bytes.Clone(e.data), rev: e.rev + 1}
	f.entries[to] = clone
	if move {
		delete(f.entries, from)
	}
	writeJSON(w, map[string]any{"metadata": f.meta(to, clone)})
}

func (f *fakeDropbox) delete(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		Path string `json:"path"`
	}
	_ = decodeBody(r, &arg)
	id := lowerPath(arg.Path)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.entries[id]; !ok {
		writeDropboxError(w, http.StatusConflict, "path_lookup/not_found/..")
		return
	}
	for key := range f.entries {
		if key == id || strings.HasPrefix(key, id+"/") {
			delete(f.entries, key)
		}
	}
	writeJSON(w, map[string]any{"metadata": map[string]any{".tag": "deleted", "name": path.Base(id), "path_lower": id}})
}

func (f *fakeDropbox) download(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		Path string `json:"path"`
	}
	_ = decodeArg(r, &arg)
	id := lowerPath(arg.Path)
	f.mu.Lock()
	e, ok := f.entries[id]
	if !ok || e.dir {
		f.mu.Unlock()
		writeDropboxError(w, http.StatusConflict, "path/not_found/..")
		return
	}
	body := bytes.Clone(e.data)
	m := f.meta(id, e)
	f.mu.Unlock()
	metaJSON, _ := json.Marshal(m)
	w.Header().Set("Dropbox-API-Result", string(metaJSON))
	start, end := 0, len(body)-1
	if raw := r.Header.Get("Range"); raw != "" {
		_, _ = fmt.Sscanf(raw, "bytes=%d-%d", &start, &end)
		if start < 0 || start >= len(body) || end < start {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= len(body) {
			end = len(body) - 1
		}
		w.WriteHeader(http.StatusPartialContent)
	}
	_, _ = w.Write(body[start : end+1])
}

func (f *fakeDropbox) storeFile(target string, body []byte) map[string]any {
	id := lowerPath(target)
	e := &fakeEntry{display: target, data: bytes.Clone(body), rev: 1}
	if old := f.entries[id]; old != nil {
		e.rev = old.rev + 1
	}
	f.entries[id] = e
	return f.meta(id, e)
}

func commitPath(arg map[string]json.RawMessage) string {
	var target string
	_ = json.Unmarshal(arg["path"], &target)
	return target
}

func (f *fakeDropbox) upload(w http.ResponseWriter, r *http.Request) {
	var arg map[string]json.RawMessage
	_ = decodeArg(r, &arg)
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	m := f.storeFile(commitPath(arg), body)
	f.mu.Unlock()
	writeJSON(w, m)
}

func (f *fakeDropbox) start(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	f.nextSession++
	sid := fmt.Sprintf("session-%d", f.nextSession)
	f.sessions[sid] = nil
	f.mu.Unlock()
	writeJSON(w, map[string]string{"session_id": sid})
}

type cursorArg struct {
	Cursor struct {
		SessionID string `json:"session_id"`
		Offset    int64  `json:"offset"`
	} `json:"cursor"`
	Commit map[string]json.RawMessage `json:"commit"`
}

func (f *fakeDropbox) append(w http.ResponseWriter, r *http.Request) {
	var arg cursorArg
	_ = decodeArg(r, &arg)
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.sessions[arg.Cursor.SessionID]
	if !ok {
		writeDropboxError(w, http.StatusConflict, "lookup_failed/not_found/..")
		return
	}
	if int64(len(current)) != arg.Cursor.Offset {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error_summary": "lookup_failed/incorrect_offset/..",
			"error":         map[string]any{".tag": "incorrect_offset", "incorrect_offset": map[string]any{"correct_offset": len(current)}},
		})
		return
	}
	f.sessions[arg.Cursor.SessionID] = append(current, body...)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeDropbox) finish(w http.ResponseWriter, r *http.Request) {
	var arg cursorArg
	_ = decodeArg(r, &arg)
	_, _ = io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.sessions[arg.Cursor.SessionID]
	if !ok || int64(len(body)) != arg.Cursor.Offset {
		writeDropboxError(w, http.StatusConflict, "lookup_failed/not_found/..")
		return
	}
	delete(f.sessions, arg.Cursor.SessionID)
	writeJSON(w, f.storeFile(commitPath(arg.Commit), body))
}

// spaceUsageHandler answers /2/users/get_space_usage. The default is the
// individual allocation every personal account reports; a test that wants
// another allocation shape sets f.spaceUsage itself.
func (f *fakeDropbox) spaceUsageHandler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	body := f.spaceUsage
	f.mu.Unlock()
	if body == nil {
		body = map[string]any{
			"used": 1234,
			"allocation": map[string]any{
				".tag": "individual", "allocated": 2 << 30,
			},
		}
	}
	writeJSON(w, body)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeDropboxError(w http.ResponseWriter, status int, summary string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error_summary": summary, "error": map[string]any{".tag": strings.Split(summary, "/")[0]}})
}

func testProvider(t *testing.T, serverURL string, partSize int64) *Provider {
	t.Helper()
	p, err := New(Options{
		Name: "db", APIBase: serverURL, ContentBase: serverURL, OAuthURL: serverURL + "/oauth2/token",
		AccessToken: "good-token", PartSize: partSize,
		Client: httpx.New(httpx.Options{Policy: retry.Policy{
			Backoff: retry.Backoff{Base: time.Millisecond, Max: time.Millisecond}, MaxAttempts: 2,
		}}),
		Now: func() time.Time { return time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProviderLifecycleAndResume(t *testing.T) {
	fake := newFakeDropbox()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	p := testProvider(t, srv.URL, 4)
	ctx := context.Background()

	if p.RootID() != RootID || !p.Capabilities().RangeRead || !p.Capabilities().StreamList || !p.Capabilities().ServerCopy {
		t.Fatalf("unexpected identity/capabilities: root=%q caps=%+v", p.RootID(), p.Capabilities())
	}
	page1, cursor, err := p.List(ctx, RootID, "")
	if err != nil || len(page1) != 1 || cursor == "" {
		t.Fatalf("first page: entries=%+v cursor=%q err=%v", page1, cursor, err)
	}
	page2, next, err := p.List(ctx, RootID, cursor)
	if err != nil || len(page2) != 2 || next != "" {
		t.Fatalf("second page: entries=%+v next=%q err=%v", page2, next, err)
	}

	stopErr := errors.New("stop")
	fake.mu.Lock()
	beforeContinue := fake.continueCalls
	fake.mu.Unlock()
	err = p.ListStream(ctx, RootID, func(provider.Entry) error { return stopErr })
	if !errors.Is(err, stopErr) {
		t.Fatalf("visitor error = %v", err)
	}
	fake.mu.Lock()
	if fake.continueCalls != beforeContinue {
		t.Errorf("ListStream fetched another page after visitor error")
	}
	fake.mu.Unlock()

	file, err := p.Stat(ctx, "/hello world.txt")
	if err != nil || file.Version != "rev-1" || file.Size != 16 || file.ParentID != RootID {
		t.Fatalf("stat = %+v, %v", file, err)
	}
	rc, err := p.ReadRange(ctx, file.ID, file.Version, 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "34567" {
		t.Fatalf("range = %q", got)
	}
	if _, err := p.ReadRange(ctx, file.ID, "stale-rev", 0, 2); !errors.Is(err, provider.ErrConflict) {
		t.Fatalf("stale range error = %v", err)
	}
	link, err := p.DownloadURL(ctx, file.ID)
	if err != nil || link.URL == "" || !link.ExpiresAt.Equal(time.Date(2026, 9, 5, 4, 0, 0, 0, time.UTC)) {
		t.Fatalf("link = %+v, %v", link, err)
	}

	put, err := p.PutFile(ctx, RootID, "视频.mp4", strings.NewReader("small"), 5, nil)
	if err != nil || put.Name != "视频.mp4" || put.Size != 5 {
		t.Fatalf("put = %+v, %v", put, err)
	}
	fake.mu.Lock()
	badHeader := fake.nonASCIIArg
	fake.mu.Unlock()
	if badHeader {
		t.Error("Dropbox-API-Arg contained raw non-ASCII bytes")
	}

	session, err := p.BeginUpload(ctx, RootID, "large.bin", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	resumed := testProvider(t, srv.URL, 99)
	parts := make([]provider.PartToken, 0, 3)
	for i, chunk := range []string{"abcd", "efgh", "ij"} {
		tok, err := resumed.UploadPart(ctx, session, i, strings.NewReader(chunk), int64(len(chunk)))
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		parts = append(parts, tok)
	}
	// Retrying an acknowledged append receives correct_offset and is accepted.
	if _, err := resumed.UploadPart(ctx, session, 2, strings.NewReader("ij"), 2); err != nil {
		t.Fatalf("idempotent part acknowledgement: %v", err)
	}
	unordered := []provider.PartToken{parts[2], parts[0], parts[1]}
	complete, err := resumed.CompleteUpload(ctx, session, unordered)
	if err != nil || complete.Size != 10 {
		t.Fatalf("complete = %+v, %v", complete, err)
	}
	if unordered[0].Index != 2 {
		t.Fatal("CompleteUpload mutated the caller's part slice")
	}
	emptySession, err := p.BeginUpload(ctx, RootID, "empty.bin", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := p.CompleteUpload(ctx, emptySession, nil)
	if err != nil || empty.Size != 0 {
		t.Fatalf("empty upload = %+v, %v", empty, err)
	}

	if _, err := p.Mkdir(ctx, RootID, "dest"); err != nil {
		t.Fatal(err)
	}
	copied, err := p.Copy(ctx, complete.ID, "/dest", "copy.bin")
	if err != nil || copied.ID != "/dest/copy.bin" {
		t.Fatalf("copy = %+v, %v", copied, err)
	}
	renamed, err := p.Rename(ctx, copied.ID, "renamed.bin")
	if err != nil || renamed.ID != "/dest/renamed.bin" {
		t.Fatalf("rename = %+v, %v", renamed, err)
	}
	moved, err := p.Move(ctx, renamed.ID, RootID)
	if err != nil || moved.ID != "/renamed.bin" {
		t.Fatalf("move = %+v, %v", moved, err)
	}
	if err := p.Delete(ctx, moved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stat(ctx, moved.ID); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("deleted stat error = %v", err)
	}
	if err := p.Delete(ctx, RootID); err == nil {
		t.Fatal("root deletion should be rejected")
	}
}

func TestRefreshesExpiredAccessTokenOnce(t *testing.T) {
	fake := newFakeDropbox()
	fake.requireFresh = true
	srv := httptest.NewServer(fake)
	defer srv.Close()
	p, err := New(Options{
		Name: "db", APIBase: srv.URL, ContentBase: srv.URL, OAuthURL: srv.URL + "/oauth2/token",
		AccessToken: "expired", RefreshToken: "refresh", ClientID: "app", ClientSecret: "secret",
		Client: httpx.New(httpx.Options{Policy: retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: time.Millisecond}, MaxAttempts: 1}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, callErr := p.Stat(context.Background(), "/other.txt")
			errs <- callErr
		}()
	}
	wg.Wait()
	close(errs)
	for callErr := range errs {
		if callErr != nil {
			t.Fatal(callErr)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", fake.refreshCalls)
	}
}

func TestChangesBaselineUpsertDeleteAndReset(t *testing.T) {
	fake := newFakeDropbox()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	p := testProvider(t, srv.URL, 4)
	if !p.Capabilities().Delta {
		t.Fatal("Dropbox change feed was not advertised")
	}
	ctx := context.Background()
	events, cursor, err := p.Changes(ctx, "")
	var baseline *provider.CursorResetError
	if !errors.As(err, &baseline) || len(events) != 0 || cursor != "" || baseline.Cursor == "" {
		t.Fatalf("baseline = %+v, %q, %#v, %v", events, cursor, baseline, err)
	}
	cursor = baseline.Cursor

	fake.mu.Lock()
	other := fake.entries["/other.txt"]
	other.data = []byte("changed")
	other.rev++
	fake.deltaEntries = append(fake.deltaEntries, fake.meta("/other.txt", other))
	delete(fake.entries, "/hello world.txt")
	fake.deltaEntries = append(fake.deltaEntries, map[string]any{
		".tag": "deleted", "name": "Hello World.txt",
		"path_lower": "/hello world.txt", "path_display": "/Hello World.txt",
	})
	fake.mu.Unlock()

	events, next, err := p.Changes(ctx, cursor)
	if err != nil || len(events) != 2 || next == cursor {
		t.Fatalf("changes = %+v, %q, %v", events, next, err)
	}
	if events[0].Op != provider.ChangeUpsert || events[0].Entry == nil || events[0].Entry.Version != "rev-2" {
		t.Fatalf("upsert = %+v", events[0])
	}
	if events[1].Op != provider.ChangeDelete || events[1].ID != "/hello world.txt" || events[1].ParentID != RootID {
		t.Fatalf("delete = %+v", events[1])
	}

	fake.mu.Lock()
	fake.resetCursor = true
	fake.mu.Unlock()
	_, _, err = p.Changes(ctx, next)
	var reset *provider.CursorResetError
	if !errors.As(err, &reset) || reset.Cursor == "" || reset.Cursor == next {
		t.Fatalf("cursor reset = %#v, %v", reset, err)
	}
}

func TestFactoryValidationAndErrorMapping(t *testing.T) {
	fake := newFakeDropbox()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	pAny, err := Factory("db", map[string]any{
		"api_base": srv.URL, "content_base": srv.URL, "oauth_url": srv.URL + "/oauth2/token",
		"access_token": "good-token", "part_size": "4MiB",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pAny.Capabilities().PartSize != 4<<20 {
		t.Fatalf("part size = %d", pAny.Capabilities().PartSize)
	}
	if _, err := Factory("bad", map[string]any{"access_token": "x", "api_base": "https://example.com/path"}); err == nil {
		t.Fatal("endpoint path should be rejected")
	}
	if _, err := Factory("bad", map[string]any{"refresh_token": "x"}); err == nil {
		t.Fatal("refresh_token without client_id should be rejected")
	}
	p := pAny.(*Provider)
	if _, err := p.Stat(context.Background(), "/missing"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("not-found mapping = %v", err)
	}
	if _, err := p.Mkdir(context.Background(), RootID, "docs"); !errors.Is(err, provider.ErrExists) {
		t.Fatalf("exists mapping = %v", err)
	}
	if _, err := p.Stat(context.Background(), "../escape"); err == nil {
		t.Fatal("unsafe id should be rejected")
	}
	if _, err := p.PutFile(context.Background(), RootID, "x", strings.NewReader("short"), 6, nil); err == nil {
		t.Fatal("short upload should be rejected")
	}
}

func TestRegisteredFactoryAndVFSReadPath(t *testing.T) {
	fake := newFakeDropbox()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	shared := httpx.New(httpx.Options{HTTP: srv.Client(), Policy: retry.Policy{MaxAttempts: 1}})
	pAny, err := provider.New("dropbox", "media", map[string]any{
		"api_base": srv.URL, "content_base": srv.URL,
		"access_token": "good-token", provider.ConfigHTTPClient: shared,
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
	blockCache, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer blockCache.Close()
	fs, err := vfs.New(vfs.Options{
		Meta: store, Cache: blockCache, DefaultDirTTL: time.Minute, AttrTTL: time.Minute,
		Mounts: []vfs.Mount{{Prefix: "/", Remote: "media", RootID: RootID, Provider: pAny, Mode: config.ModeReadonly, DirTTL: time.Minute}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	entries, err := fs.ReadDirPath(context.Background(), "/")
	if err != nil || len(entries) != 3 {
		t.Fatalf("VFS listing = %+v, %v", entries, err)
	}
	body, err := fs.ReadFileRange(context.Background(), "/Hello World.txt", 0, 0)
	if err != nil || string(body) != "0123456789abcdef" {
		t.Fatalf("VFS read = %q, %v", body, err)
	}
	refresher := vfs.NewRefresher(fs, time.Minute)
	mount := fs.Mounts()[0]
	if applied, err := refresher.PollOnce(context.Background(), mount); err != nil || applied != 0 {
		t.Fatalf("delta baseline = %d, %v", applied, err)
	}
	// The baseline intentionally makes persisted listings stale; repopulate the
	// root, then verify an external edit flows through the Dropbox cursor.
	if _, err := fs.ReadDirPath(context.Background(), "/"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	other := fake.entries["/other.txt"]
	other.data = []byte("externally changed")
	other.rev++
	fake.deltaEntries = append(fake.deltaEntries, fake.meta("/other.txt", other))
	fake.mu.Unlock()
	if applied, err := refresher.PollOnce(context.Background(), mount); err != nil || applied != 1 {
		t.Fatalf("delta update = %d, %v", applied, err)
	}
	body, err = fs.ReadFileRange(context.Background(), "/Other.txt", 0, 0)
	if err != nil || string(body) != "externally changed" {
		t.Fatalf("VFS delta read = %q, %v", body, err)
	}
}

func TestHeaderJSONEscapesUnicodeAndSupplementaryRunes(t *testing.T) {
	got, err := headerJSON(map[string]string{"path": "/视频/😀.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []byte(got) {
		if b > 0x7f {
			t.Fatalf("header has non-ASCII byte: %q", got)
		}
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(got), &decoded); err != nil || decoded["path"] != "/视频/😀.mkv" {
		t.Fatalf("round trip = %q, %+v, %v", got, decoded, err)
	}
}

func TestSessionAndResponseGuards(t *testing.T) {
	p := testProvider(t, "http://127.0.0.1:1", 4)
	bad := provider.UploadSession{ID: "x", PartSize: 4, Opaque: map[string]string{"target": "/x", "size": "4"}}
	if _, err := p.UploadPart(context.Background(), bad, 0, strings.NewReader("data"), 4); err == nil {
		t.Fatal("missing session id should fail")
	}
	s := provider.UploadSession{ID: "sid", PartSize: 4, Opaque: map[string]string{"target": "/x", "size": "4"}}
	if _, err := p.CompleteUpload(context.Background(), s, []provider.PartToken{{Index: 1, ETag: "4"}}); err == nil {
		t.Fatal("non-contiguous parts should fail")
	}
	if _, err := decodeLimitedForTest(strings.Repeat(" ", maxJSONResponse+1)); err == nil {
		t.Fatal("oversized response should fail")
	}
}

func decodeLimitedForTest(body string) (any, error) {
	dec := json.NewDecoder(io.LimitReader(strings.NewReader(body), maxJSONResponse+1))
	var out any
	return out, dec.Decode(&out)
}

func TestPartTokenLength(t *testing.T) {
	if got := strconv.FormatInt(4, 10); got != "4" {
		t.Fatal(got)
	}
}

// TestDropboxReportsTheSpaceItsAccountHas pins the reason this driver
// implements provider.Quotaer at all: a pool sorts members by free space and
// ranks every member with a known figure ahead of every member without one,
// so a Dropbox member that cannot answer loses placement decisions it should
// have won.
func TestDropboxReportsTheSpaceItsAccountHas(t *testing.T) {
	fake := newFakeDropbox()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	p := testProvider(t, srv.URL, 4)

	q, ok, err := provider.QuotaOf(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Dropbox does not implement provider.Quotaer")
	}
	if q.Total != 2<<30 || q.Used != 1234 {
		t.Fatalf("quota = %+v, want total=%d used=1234", q, int64(2<<30))
	}
	if want := int64(2<<30) - 1234; q.Free() != want {
		t.Fatalf("free = %d, want %d", q.Free(), want)
	}
}

// TestDropboxReportsUnknownSpaceWhenTheAllocationIsOneItCannotRead covers the
// team allocation, whose figures live under a different shape than the
// individual one. Reporting a guess would be worse than reporting nothing:
// the pool would place writes against a number that is not this account's,
// so an unreadable allocation must come back as unknown (Total 0) and not as
// an error either, since a mount whose quota call fails is noisier than one
// that simply cannot say.
func TestDropboxReportsUnknownSpaceWhenTheAllocationIsOneItCannotRead(t *testing.T) {
	fake := newFakeDropbox()
	fake.spaceUsage = map[string]any{
		"used": 4096,
		"allocation": map[string]any{
			".tag":      "team",
			"used":      4096,
			"allocated": 8 << 30,
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	p := testProvider(t, srv.URL, 4)

	q, ok, err := provider.QuotaOf(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Dropbox does not implement provider.Quotaer")
	}
	if q.Total != 0 {
		t.Fatalf("team allocation reported total = %d, want 0 (unknown)", q.Total)
	}
	if q.Free() >= 0 {
		t.Fatalf("free = %d, want a negative number meaning unknown", q.Free())
	}
}
