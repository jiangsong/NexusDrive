package pan115

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// recorded is one request the stub server saw.
type recorded struct {
	Method string
	Path   string
	Query  url.Values
	Form   url.Values
	Header http.Header
	Body   []byte
}

// recorder collects requests so a test can assert what actually went on the
// wire rather than only that a call returned no error.
type recorder struct {
	mu   sync.Mutex
	reqs []recorded
}

func (rec *recorder) add(r *http.Request) recorded {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	got := recorded{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.Query(),
		Header: r.Header.Clone(),
		Body:   body,
	}
	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		if v, err := url.ParseQuery(string(body)); err == nil {
			got.Form = v
		}
	}
	rec.mu.Lock()
	rec.reqs = append(rec.reqs, got)
	rec.mu.Unlock()
	return got
}

func (rec *recorder) all(path string) []recorded {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []recorded
	for _, r := range rec.reqs {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (rec *recorder) count(path string) int { return len(rec.all(path)) }

func (rec *recorder) last(t *testing.T, path string) recorded {
	t.Helper()
	all := rec.all(path)
	if len(all) == 0 {
		t.Fatalf("no request recorded for %s (saw %v)", path, rec.paths())
	}
	return all[len(all)-1]
}

func (rec *recorder) paths() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]string, 0, len(rec.reqs))
	for _, r := range rec.reqs {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

// newTest wires a Pan115 to an httptest.Server. Every endpoint the driver can
// reach — API, passport and OSS — is served by that one origin, which is what
// the injectable BaseURL/PassportURL/OSSEndpoint options exist for.
func newTest(t *testing.T, h http.HandlerFunc) (*Pan115, *recorder, *httptest.Server) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	p, err := NewWithOptions("p115", Options{
		Client: httpx.New(httpx.Options{
			Remote: "p115",
			// Keep retries fast; the defaults sleep half a second.
			Policy:    retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond}, MaxAttempts: 2},
			UserAgent: DownloadUA,
		}),
		BaseURL:      srv.URL,
		PassportURL:  srv.URL,
		OSSEndpoint:  srv.URL,
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return p, rec, srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// okEnvelope wraps data the way 115 Open does.
func okEnvelope(data any) map[string]any {
	return map[string]any{"state": true, "code": 0, "message": "", "data": data}
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New("p115", map[string]any{}); err == nil || !strings.Contains(err.Error(), "refresh_token") {
		t.Fatalf("empty config error = %v, want one naming refresh_token", err)
	}
	// A wrongly typed key must name the key, not blow up somewhere later.
	if _, err := New("p115", map[string]any{"refresh_token": map[string]any{}}); err == nil ||
		!strings.Contains(err.Error(), "refresh_token") {
		t.Fatalf("typed config error = %v, want one naming refresh_token", err)
	}
	pr, err := New("p115", map[string]any{"refresh_token": "rt", "root_id": 42, "client_id": "cid"})
	if err != nil {
		t.Fatal(err)
	}
	p := pr.(*Pan115)
	if p.RootID() != "42" {
		t.Errorf("root_id = %q, want 42 (YAML gives ints)", p.RootID())
	}
	if p.Name() != "p115" {
		t.Errorf("Name = %q", p.Name())
	}
	if p.RefreshToken() != "rt" {
		t.Errorf("RefreshToken = %q", p.RefreshToken())
	}
	// An access_token alone is enough to run without a refresh token.
	if _, err := New("p115", map[string]any{"access_token": "at"}); err != nil {
		t.Errorf("access_token-only config rejected: %v", err)
	}
}

func TestRegisteredInRegistry(t *testing.T) {
	found := false
	for _, typ := range provider.Types() {
		if typ == "pan115" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pan115 not registered; Types() = %v", provider.Types())
	}
	if _, err := provider.New("pan115", "p115", map[string]any{"refresh_token": "rt"}); err != nil {
		t.Fatalf("provider.New: %v", err)
	}
}

func TestCapabilitiesAreConservative(t *testing.T) {
	p, _, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {})
	c := p.Capabilities()
	if c.QPS != (provider.QPS{Meta: 1, Download: 2, Upload: 1}) {
		t.Errorf("QPS = %+v, want {1 2 1}: 115 bans clients above ~3 req/s", c.QPS)
	}
	if c.Tier != provider.TierOfficial {
		t.Errorf("Tier = %q", c.Tier)
	}
	if c.Delta {
		t.Error("115 Open has no change feed; Delta must be false")
	}
	if _, ok := any(p).(provider.ChangeLister); ok {
		t.Error("115 Open publishes no delta feed; the driver must not implement ChangeLister")
	}
	if c.PartSize != 8<<20 {
		t.Errorf("PartSize = %d", c.PartSize)
	}
	if !c.RangeRead || !c.ServerMove || !c.ServerRename || !c.LinkShareable {
		t.Errorf("caps = %+v", c)
	}
	if c.LinkTTL != 2*time.Hour {
		t.Errorf("LinkTTL = %s", c.LinkTTL)
	}
	if c.LinkHeaders["User-Agent"] != DownloadUA {
		t.Errorf("LinkHeaders = %v, want the UA the link is bound to", c.LinkHeaders)
	}
	if len(c.HashTypes) != 1 || c.HashTypes[0] != provider.HashSHA1 ||
		len(c.RapidUpload) != 1 || c.RapidUpload[0] != provider.HashSHA1 {
		t.Errorf("hash caps = %v / %v", c.HashTypes, c.RapidUpload)
	}
}

func TestListPaginates(t *testing.T) {
	p, rec, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/open/ufile/files" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch r.URL.Query().Get("offset") {
		case "0":
			writeJSON(w, map[string]any{
				"state": true, "code": 0, "count": 3, "offset": 0, "limit": 1000,
				"data": []map[string]any{
					{"fid": "100", "pid": "0", "fc": "0", "fn": "docs", "upt": "1700000000"},
					{"fid": "101", "pid": "0", "fc": "1", "fn": "a.bin", "fs": "1048576",
						"sha1": "AAAABBBBCCCCDDDDEEEEFFFF0000111122223333", "pc": "pc-101", "upt": 1700000100},
				},
			})
		case "2":
			writeJSON(w, map[string]any{
				"state": true, "code": 0, "count": 3, "offset": 2, "limit": 1000,
				"data": []map[string]any{
					{"fid": "102", "pid": "0", "fc": "1", "fn": "b.bin", "fs": 42,
						"sha1": "1111222233334444555566667777888899990000", "pc": "pc-102", "upt": 1700000200},
				},
			})
		default:
			t.Errorf("unexpected offset %q", r.URL.Query().Get("offset"))
		}
	})

	entries, next, err := p.List(context.Background(), "0", "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "2" {
		t.Fatalf("next cursor = %q, want 2", next)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].Kind != provider.KindDir || entries[0].Name != "docs" || entries[0].ID != "100" {
		t.Errorf("dir entry = %+v", entries[0])
	}
	if entries[0].Size != 0 {
		t.Errorf("directory size should be zeroed, got %d", entries[0].Size)
	}
	f := entries[1]
	if f.Kind != provider.KindFile || f.Size != 1<<20 || f.ParentID != "0" {
		t.Errorf("file entry = %+v", f)
	}
	if got := f.Hashes[provider.HashSHA1]; got != "aaaabbbbccccddddeeeeffff0000111122223333" {
		t.Errorf("sha1 = %q, want lowercased", got)
	}
	if !f.ModTime.Equal(time.Unix(1700000100, 0).UTC()) {
		t.Errorf("ModTime = %s", f.ModTime)
	}

	first := rec.all("/open/ufile/files")[0]
	if first.Query.Get("cid") != "0" || first.Query.Get("limit") != "1000" || first.Query.Get("show_dir") != "1" {
		t.Errorf("list query = %v", first.Query)
	}
	if got := first.Header.Get("Authorization"); got != "Bearer access-1" {
		t.Errorf("Authorization = %q", got)
	}

	entries2, next2, err := p.List(context.Background(), "0", next)
	if err != nil {
		t.Fatal(err)
	}
	if next2 != "" {
		t.Errorf("last page cursor = %q, want empty", next2)
	}
	if len(entries2) != 1 || entries2[0].ID != "102" {
		t.Fatalf("page 2 = %+v", entries2)
	}
	if rec.count("/open/ufile/files") != 2 {
		t.Errorf("made %d list calls", rec.count("/open/ufile/files"))
	}

	// The pick codes seen while listing must be cached, so a later download
	// does not spend a metadata call re-reading them.
	p.mu.Lock()
	pc := p.pickCodes["101"]
	p.mu.Unlock()
	if pc != "pc-101" {
		t.Errorf("cached pick code = %q", pc)
	}
}

func TestStatDecodesFileAndFolder(t *testing.T) {
	p, rec, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("file_id") {
		case "101":
			writeJSON(w, okEnvelope(map[string]any{
				"file_id": "101", "file_name": "a.bin", "file_category": "1",
				"size_byte": 1048576, "size": "1.0MB", "pick_code": "pc-101",
				"sha1":  "AAAABBBBCCCCDDDDEEEEFFFF0000111122223333",
				"utime": "1700000100",
				"paths": []map[string]any{{"file_id": "0", "file_name": ""}, {"file_id": "100", "file_name": "docs"}},
			}))
		case "100":
			writeJSON(w, okEnvelope(map[string]any{
				"file_id": "100", "file_name": "docs", "file_category": "0",
				"size": "0", "count": "3", "utime": 1700000000,
				"paths": []map[string]any{{"file_id": "0", "file_name": ""}},
			}))
		default:
			writeJSON(w, map[string]any{"state": false, "code": codeNotFound, "message": "文件不存在"})
		}
	})

	f, err := p.Stat(context.Background(), "101")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != provider.KindFile || f.Size != 1<<20 || f.ParentID != "100" || f.Name != "a.bin" {
		t.Errorf("file stat = %+v", f)
	}
	if f.Hashes[provider.HashSHA1] != "aaaabbbbccccddddeeeeffff0000111122223333" {
		t.Errorf("hashes = %v", f.Hashes)
	}
	if !f.ModTime.Equal(time.Unix(1700000100, 0).UTC()) {
		t.Errorf("ModTime = %s", f.ModTime)
	}
	if rec.last(t, "/open/folder/get_info").Query.Get("file_id") != "101" {
		t.Errorf("stat query = %v", rec.last(t, "/open/folder/get_info").Query)
	}

	d, err := p.Stat(context.Background(), "100")
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != provider.KindDir || d.ParentID != "0" {
		t.Errorf("dir stat = %+v", d)
	}

	if _, err := p.Stat(context.Background(), "999"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("missing id error = %v, want ErrNotFound", err)
	}
}

// blobHandler serves a range of content and records what it was asked for.
func blobHandler(t *testing.T, content []byte, gotRange, gotUA *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*gotRange = r.Header.Get("Range")
		*gotUA = r.Header.Get("User-Agent")
		var start, end int
		if _, err := fmt.Sscanf(*gotRange, "bytes=%d-%d", &start, &end); err != nil {
			t.Errorf("bad range header %q: %v", *gotRange, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[start : end+1])
	}
}

func TestReadRangeSendsRangeAndBoundUserAgent(t *testing.T) {
	content := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	var gotRange, gotUA string
	var srvURL string
	p, rec, srv := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/folder/get_info":
			writeJSON(w, okEnvelope(map[string]any{
				"file_id": "101", "file_name": "a.bin", "file_category": "1",
				"size_byte": len(content), "pick_code": "pc-101", "sha1": "abc", "utime": 1700000100,
			}))
		case "/open/ufile/downurl":
			writeJSON(w, okEnvelope(map[string]any{
				"101": map[string]any{
					"file_name": "a.bin", "file_size": len(content), "pick_code": "pc-101",
					"url": map[string]any{"url": srvURL + "/cdn/blob"},
				},
			}))
		case "/cdn/blob":
			blobHandler(t, content, &gotRange, &gotUA)(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	srvURL = srv.URL

	rc, err := p.ReadRange(context.Background(), "101", "v1", 4, 6)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "456789" {
		t.Fatalf("read %q, want 456789", got)
	}
	if gotRange != "bytes=4-9" {
		t.Errorf("Range header = %q", gotRange)
	}
	if gotUA != DownloadUA {
		t.Errorf("CDN User-Agent = %q, want the UA the link is bound to (%q)", gotUA, DownloadUA)
	}
	dl := rec.last(t, "/open/ufile/downurl")
	if dl.Form.Get("pick_code") != "pc-101" {
		t.Errorf("downurl form = %v, want the pick code, not the file id", dl.Form)
	}
	if dl.Form.Get("ua") != DownloadUA || dl.Header.Get("User-Agent") != DownloadUA {
		t.Errorf("downurl UA: form=%q header=%q", dl.Form.Get("ua"), dl.Header.Get("User-Agent"))
	}

	// A second range must reuse the cached link: no extra metadata calls.
	before := rec.count("/open/ufile/downurl")
	rc2, err := p.ReadRange(context.Background(), "101", "v1", 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(rc2)
	rc2.Close()
	if string(b2) != "0123" {
		t.Errorf("second read = %q", b2)
	}
	if rec.count("/open/ufile/downurl") != before {
		t.Errorf("link was re-resolved: %d -> %d calls", before, rec.count("/open/ufile/downurl"))
	}
	if rec.count("/open/folder/get_info") != 1 {
		t.Errorf("pick code was re-read %d times", rec.count("/open/folder/get_info"))
	}
}

func TestReadRangeRefreshesExpiredLink(t *testing.T) {
	content := []byte("0123456789")
	var hits int
	var mu sync.Mutex
	var srvURL string
	p, rec, srv := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/folder/get_info":
			writeJSON(w, okEnvelope(map[string]any{
				"file_id": "101", "file_name": "a.bin", "file_category": "1",
				"size_byte": len(content), "pick_code": "pc-101", "sha1": "abc",
			}))
		case "/open/ufile/downurl":
			writeJSON(w, okEnvelope(map[string]any{
				"101": map[string]any{"url": map[string]any{"url": srvURL + "/cdn/blob"}},
			}))
		case "/cdn/blob":
			mu.Lock()
			hits++
			n := hits
			mu.Unlock()
			if n == 1 {
				// 115 CDN links die early under load; the driver must
				// re-resolve rather than fail the read.
				w.WriteHeader(http.StatusForbidden)
				return
			}
			var start, end int
			fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content[start : end+1])
		default:
			http.NotFound(w, r)
		}
	})
	srvURL = srv.URL

	rc, err := p.ReadRange(context.Background(), "101", "", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "234" {
		t.Errorf("read %q", got)
	}
	if n := rec.count("/open/ufile/downurl"); n != 2 {
		t.Errorf("downurl called %d times, want 2 (initial + refresh)", n)
	}
}

func TestDownloadURLReportsHeadersAndExpiry(t *testing.T) {
	p, _, srv := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/folder/get_info":
			writeJSON(w, okEnvelope(map[string]any{"file_id": "101", "file_category": "1", "pick_code": "pc-101", "sha1": "x"}))
		case "/open/ufile/downurl":
			writeJSON(w, okEnvelope(map[string]any{
				"77": map[string]any{"url": map[string]any{"url": "https://cdn.115.com/blob"}},
			}))
		}
	})
	_ = srv
	l, err := p.DownloadURL(context.Background(), "101")
	if err != nil {
		t.Fatal(err)
	}
	// The map is keyed by 115's own id, which need not equal the caller's:
	// a single entry is still unambiguous.
	if l.URL != "https://cdn.115.com/blob" {
		t.Errorf("URL = %q", l.URL)
	}
	if l.Headers["User-Agent"] != DownloadUA {
		t.Errorf("link headers = %v", l.Headers)
	}
	if time.Until(l.ExpiresAt) < time.Hour {
		t.Errorf("ExpiresAt = %s, want ~2h out", l.ExpiresAt)
	}
}

func TestMkdirRenameMoveDelete(t *testing.T) {
	p, rec, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/folder/add":
			writeJSON(w, okEnvelope(map[string]any{"file_id": "200", "file_name": "new"}))
		case "/open/ufile/update", "/open/ufile/move":
			writeJSON(w, okEnvelope(nil))
		case "/open/ufile/delete":
			writeJSON(w, map[string]any{"state": true, "code": 0})
		case "/open/folder/get_info":
			writeJSON(w, okEnvelope(map[string]any{
				"file_id": "200", "file_name": "renamed", "file_category": "0",
				"utime": 1700000300,
				"paths": []map[string]any{{"file_id": "0"}, {"file_id": "300"}},
			}))
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()

	d, err := p.Mkdir(ctx, "0", "new")
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "200" || d.Kind != provider.KindDir || d.ParentID != "0" {
		t.Errorf("mkdir entry = %+v", d)
	}
	if f := rec.last(t, "/open/folder/add").Form; f.Get("pid") != "0" || f.Get("file_name") != "new" {
		t.Errorf("mkdir form = %v", f)
	}

	r, err := p.Rename(ctx, "200", "renamed")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "renamed" {
		t.Errorf("rename entry = %+v (must be read back, not assumed)", r)
	}
	if f := rec.last(t, "/open/ufile/update").Form; f.Get("file_id") != "200" || f.Get("file_name") != "renamed" {
		t.Errorf("rename form = %v", f)
	}

	m, err := p.Move(ctx, "200", "300")
	if err != nil {
		t.Fatal(err)
	}
	if m.ParentID != "300" {
		t.Errorf("move entry = %+v", m)
	}
	if f := rec.last(t, "/open/ufile/move").Form; f.Get("file_ids") != "200" || f.Get("to_cid") != "300" {
		t.Errorf("move form = %v", f)
	}

	if err := p.Delete(ctx, "200"); err != nil {
		t.Fatal(err)
	}
	if f := rec.last(t, "/open/ufile/delete").Form; f.Get("file_ids") != "200" {
		t.Errorf("delete form = %v", f)
	}
}

func TestMkdirDuplicateMapsToErrExists(t *testing.T) {
	p, _, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"state": false, "code": codeDirExists, "message": "该目录名称已存在"})
	})
	if _, err := p.Mkdir(context.Background(), "0", "dup"); !errors.Is(err, provider.ErrExists) {
		t.Fatalf("error = %v, want ErrExists", err)
	}
}

// writeBlob stages content on disk so FileRangeHasher can answer the
// rapid-upload challenge, exactly as the upload journal would.
func writeBlob(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func sha1hex(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}

func TestBeginUploadRapidWithRangeSignature(t *testing.T) {
	content := []byte(strings.Repeat("cloudfs-115-", 20)) // 240 bytes
	full := strings.ToUpper(sha1hex(content))
	signRange := "10-19"
	wantSignVal := strings.ToUpper(sha1hex(content[10:20]))
	wantPreID := strings.ToUpper(sha1hex(content))

	var round int
	p, rec, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/upload/init":
			round++
			_ = r.ParseForm()
			if round == 1 {
				// 115 has a file with this SHA1 and wants proof the client
				// really holds the bytes.
				writeJSON(w, okEnvelope(map[string]any{
					"status": 7, "statuscode": signCheckStatus,
					"sign_key": "SK-1", "sign_check": signRange,
				}))
				return
			}
			writeJSON(w, okEnvelope(map[string]any{
				"status": statusRapidUploaded, "statuscode": 0,
				"file_id": "900", "pick_code": "pc-900",
			}))
		case "/open/folder/get_info":
			writeJSON(w, okEnvelope(map[string]any{
				"file_id": "900", "file_name": "big.bin", "file_category": "1",
				"size_byte": len(content), "sha1": full, "pick_code": "pc-900",
				"paths": []map[string]any{{"file_id": "0"}, {"file_id": "5"}},
			}))
		default:
			http.NotFound(w, r)
		}
	})

	ctx := WithRangeHasher(context.Background(), FileRangeHasher(writeBlob(t, content)))
	s, err := p.BeginUpload(ctx, "5", "big.bin", int64(len(content)),
		provider.Hashes{provider.HashSHA1: strings.ToLower(full)})
	if err != nil {
		t.Fatal(err)
	}
	if !s.RapidDone {
		t.Fatal("expected a rapid upload hit")
	}
	if s.Entry == nil || s.Entry.ID != "900" || s.Entry.Size != int64(len(content)) {
		t.Fatalf("entry = %+v", s.Entry)
	}

	inits := rec.all("/open/upload/init")
	if len(inits) != 2 {
		t.Fatalf("init called %d times, want 2 (the two-step handshake)", len(inits))
	}
	first := inits[0].Form
	if first.Get("fileid") != full {
		t.Errorf("fileid = %q, want the uppercase full-file sha1", first.Get("fileid"))
	}
	if first.Get("target") != "U_1_5" {
		t.Errorf("target = %q, want U_1_<cid>", first.Get("target"))
	}
	if first.Get("file_size") != "240" || first.Get("file_name") != "big.bin" {
		t.Errorf("init form = %v", first)
	}
	if first.Get("preid") != wantPreID {
		t.Errorf("preid = %q, want sha1 of the first 128KiB (whole file here) = %q", first.Get("preid"), wantPreID)
	}
	if first.Get("sign_key") != "" || first.Get("sign_val") != "" {
		t.Errorf("first init must not carry a signature: %v", first)
	}
	second := inits[1].Form
	if second.Get("sign_key") != "SK-1" {
		t.Errorf("sign_key = %q", second.Get("sign_key"))
	}
	if second.Get("sign_val") != wantSignVal {
		t.Errorf("sign_val = %q, want sha1(content[10:20]) = %q", second.Get("sign_val"), wantSignVal)
	}
}

func TestBeginUploadWithoutRangeHasherDegradesSafely(t *testing.T) {
	p, _, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, okEnvelope(map[string]any{
			"status": 7, "statuscode": signCheckStatus, "sign_key": "SK-1", "sign_check": "0-1023",
		}))
	})
	_, err := p.BeginUpload(context.Background(), "0", "x.bin", 4096,
		provider.Hashes{provider.HashSHA1: strings.Repeat("a", 40)})
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "WithRangeHasher") {
		t.Errorf("error should name the missing hook: %v", err)
	}
}

func TestBeginUploadWithoutSHA1IsRejected(t *testing.T) {
	p, _, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be made without a sha1")
	})
	_, err := p.BeginUpload(context.Background(), "0", "x.bin", 10, provider.Hashes{})
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

const (
	testBucket = "fhnfile"
	testObject = "uploads/2025/abc"
)

// ossState is the tiny OSS multipart implementation the chunked-upload test
// replays.
type ossState struct {
	mu       sync.Mutex
	parts    map[int][]byte
	complete []byte
	callback string
}

func TestChunkedUploadRoundTrip(t *testing.T) {
	content := []byte(strings.Repeat("A", 1000) + strings.Repeat("B", 500))
	full := strings.ToUpper(sha1hex(content))
	oss := &ossState{parts: map[int][]byte{}}
	objectPath := "/" + testBucket + "/" + testObject

	p, rec, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/open/upload/init":
			writeJSON(w, okEnvelope(map[string]any{
				"status": statusNeedUpload, "statuscode": 0,
				"bucket": testBucket, "object": testObject, "pick_code": "pc-up",
				"callback": map[string]any{
					"callback":     "eyJjYWxsYmFja1VybCI6Imh0dHBzOi8vdXAuMTE1LmNvbS9jYWxsYmFjayJ9",
					"callback_var": "x:1",
				},
			}))
		case r.URL.Path == "/open/upload/get_token":
			writeJSON(w, okEnvelope(map[string]any{
				"AccessKeyId": "AK", "AccessKeySecret": "SK", "SecurityToken": "STS-TOKEN",
				"expiration": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				"endpoint":   "", // the driver's OSSEndpoint override wins
			}))
		case r.URL.Path == objectPath && r.URL.RawQuery == "uploads":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<InitiateMultipartUploadResult><Bucket>` + testBucket + `</Bucket><Key>` + testObject + `</Key><UploadId>UP-1</UploadId></InitiateMultipartUploadResult>`))
		case r.URL.Path == objectPath && r.Method == "PUT":
			q := r.URL.Query()
			if q.Get("uploadId") != "UP-1" {
				t.Errorf("part uploadId = %q", q.Get("uploadId"))
			}
			if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "OSS AK:") {
				t.Errorf("part Authorization = %q, want an OSS V1 signature", auth)
			}
			if r.Header.Get("x-oss-security-token") != "STS-TOKEN" {
				t.Errorf("missing STS token header: %v", r.Header)
			}
			if r.Header.Get("Date") == "" {
				t.Error("OSS requires a Date header in the signature")
			}
			body, _ := io.ReadAll(r.Body)
			var n int
			fmt.Sscanf(q.Get("partNumber"), "%d", &n)
			oss.mu.Lock()
			oss.parts[n] = body
			oss.mu.Unlock()
			w.Header().Set("ETag", `"etag-`+q.Get("partNumber")+`"`)
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == objectPath && r.Method == "POST":
			body, _ := io.ReadAll(r.Body)
			oss.mu.Lock()
			oss.complete = body
			oss.callback = r.Header.Get("x-oss-callback")
			oss.mu.Unlock()
			// OSS forwards 115's callback answer verbatim: a 115 envelope.
			writeJSON(w, okEnvelope(map[string]any{
				"file_id": "901", "file_name": "big.bin", "pick_code": "pc-up",
				"sha1": full, "file_size": len(content), "cid": "5",
			}))
		default:
			t.Errorf("unexpected %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
		}
	})

	ctx := WithRangeHasher(context.Background(), FileRangeHasher(writeBlob(t, content)))
	s, err := p.BeginUpload(ctx, "5", "big.bin", int64(len(content)),
		provider.Hashes{provider.HashSHA1: strings.ToLower(full)})
	if err != nil {
		t.Fatal(err)
	}
	if s.RapidDone {
		t.Fatal("status 1 must not be reported as a rapid upload")
	}
	if s.PartSize != 8<<20 {
		t.Errorf("PartSize = %d", s.PartSize)
	}
	for _, k := range []string{"bucket", "object", "upload_id", "endpoint", "parent_id", "name", "sha1", "size"} {
		if s.Opaque[k] == "" {
			t.Errorf("Opaque[%q] is empty; a journal-resumed session would be unusable", k)
		}
	}
	if s.Opaque["upload_id"] != "UP-1" {
		t.Errorf("upload_id = %q", s.Opaque["upload_id"])
	}
	// Credentials must NOT be journalled: they expire in about an hour.
	for _, k := range []string{"access_key_secret", "security_token", "AccessKeySecret"} {
		if _, ok := s.Opaque[k]; ok {
			t.Errorf("Opaque leaks credential %q", k)
		}
	}

	// Rebuild the session from Opaque alone, the way a restart would.
	resumed := provider.UploadSession{ID: s.ID, PartSize: s.PartSize, Opaque: s.Opaque}

	var parts []provider.PartToken
	for i, chunk := range [][]byte{content[:1000], content[1000:]} {
		pt, err := p.UploadPart(ctx, resumed, i, strings.NewReader(string(chunk)), int64(len(chunk)))
		if err != nil {
			t.Fatal(err)
		}
		if pt.Index != i || pt.ETag != fmt.Sprintf("etag-%d", i+1) {
			t.Fatalf("part token = %+v (OSS part numbers are 1-based)", pt)
		}
		parts = append(parts, pt)
	}
	oss.mu.Lock()
	if string(oss.parts[1]) != string(content[:1000]) || string(oss.parts[2]) != string(content[1000:]) {
		t.Error("uploaded part bodies do not match the content")
	}
	oss.mu.Unlock()

	// Complete out of order to prove the driver sorts parts: OSS rejects an
	// unsorted CompleteMultipartUpload.
	e, err := p.CompleteUpload(ctx, resumed, []provider.PartToken{parts[1], parts[0]})
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "901" || e.Name != "big.bin" || e.Size != int64(len(content)) || e.ParentID != "5" {
		t.Fatalf("entry = %+v", e)
	}
	if e.Hashes[provider.HashSHA1] != strings.ToLower(full) || e.Version != strings.ToLower(full) {
		t.Errorf("entry hashes = %v version = %q", e.Hashes, e.Version)
	}

	oss.mu.Lock()
	body, cb := oss.complete, oss.callback
	oss.mu.Unlock()
	var done completeMultipartUpload
	if err := xml.Unmarshal(body, &done); err != nil {
		t.Fatalf("complete body is not CompleteMultipartUpload XML: %v (%s)", err, body)
	}
	if len(done.Parts) != 2 || done.Parts[0].PartNumber != 1 || done.Parts[1].PartNumber != 2 {
		t.Fatalf("complete parts = %+v", done.Parts)
	}
	if done.Parts[0].ETag != `"etag-1"` {
		t.Errorf("ETag = %q, OSS wants it quoted", done.Parts[0].ETag)
	}
	decoded, err := base64.StdEncoding.DecodeString(cb)
	if err != nil || string(decoded) != "eyJjYWxsYmFja1VybCI6Imh0dHBzOi8vdXAuMTE1LmNvbS9jYWxsYmFjayJ9" {
		t.Errorf("x-oss-callback = %q (decoded %q), want the base64 of the callback 115 handed out", cb, decoded)
	}
	// One token fetch is enough for the whole upload.
	if n := rec.count("/open/upload/get_token"); n != 1 {
		t.Errorf("get_token called %d times, want 1 (credentials are cached)", n)
	}
}

func TestUploadPartWithoutSessionStateIsNotFound(t *testing.T) {
	p, _, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request expected")
	})
	_, err := p.UploadPart(context.Background(), provider.UploadSession{ID: "x"}, 0, strings.NewReader("a"), 1)
	if !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound so the uploader restarts the session", err)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		code    int
		message string
		want    error
	}{
		// 115 answers a suspected third-party client with a verification
		// demand; the upper layer must circuit-break, not retry.
		{"app verification", codeNeedAppVerify, "PermissionDenied: 请在 115 客户端中验证账号", provider.ErrRiskControl},
		{"risk message only", 0, "账号存在风控，请稍后再试", provider.ErrRiskControl},
		{"too frequent", codeTooFrequent, "操作频繁，请稍后再试", provider.ErrRateLimited},
		{"access token expired", codeAccessTokenExpired, "access_token expired", provider.ErrAuth},
		{"refresh token invalid", codeRefreshTokenInvalid, "refresh_token invalid", provider.ErrAuth},
		{"dir exists", codeDirExists, "该目录名称已存在", provider.ErrExists},
		{"not found", codeNotFound, "文件不存在", provider.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mapError(tc.code, tc.message)
			if !errors.Is(err, tc.want) {
				t.Fatalf("mapError(%d, %q) = %v, want %v", tc.code, tc.message, err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error text drops the server message: %v", err)
			}
		})
	}
}

func TestRiskControlOverTheWire(t *testing.T) {
	p, rec, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"state": false, "code": codeNeedAppVerify,
			"message": "PermissionDenied，请在 115 客户端中验证账号",
		})
	})
	_, _, err := p.List(context.Background(), "0", "")
	if !errors.Is(err, provider.ErrRiskControl) {
		t.Fatalf("error = %v, want ErrRiskControl", err)
	}
	if retry.Classify(err) != retry.ClassRiskControl {
		t.Fatalf("classified as %v, want risk_control so the breaker trips", retry.Classify(err))
	}
	// A risk-control answer must not be retried into a ban.
	if n := rec.count("/open/ufile/files"); n != 1 {
		t.Errorf("risk-control response was retried %d times", n)
	}
}

func TestAuthErrorTriggersOneRefreshThenRetries(t *testing.T) {
	var calls int
	p, rec, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open/refreshToken":
			_ = r.ParseForm()
			if r.PostFormValue("refresh_token") != "refresh-1" {
				t.Errorf("refresh form = %v", r.PostForm)
			}
			writeJSON(w, okEnvelope(map[string]any{
				"access_token": "access-2", "refresh_token": "refresh-2", "expires_in": 7200,
			}))
		case "/open/ufile/files":
			calls++
			if calls == 1 {
				writeJSON(w, map[string]any{"state": false, "code": codeAccessTokenExpired, "message": "access_token expired"})
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer access-2" {
				t.Errorf("retry Authorization = %q, want the refreshed token", got)
			}
			writeJSON(w, map[string]any{"state": true, "code": 0, "count": 0, "data": []any{}})
		default:
			http.NotFound(w, r)
		}
	})

	if _, _, err := p.List(context.Background(), "0", ""); err != nil {
		t.Fatal(err)
	}
	if rec.count("/open/refreshToken") != 1 {
		t.Errorf("refreshed %d times, want exactly 1", rec.count("/open/refreshToken"))
	}
	// 115 rotates the refresh token and remembers only two per app, so the
	// new one must replace the configured value.
	if p.RefreshToken() != "refresh-2" {
		t.Errorf("RefreshToken = %q, want the rotated value", p.RefreshToken())
	}
	if p.AccessToken() != "access-2" {
		t.Errorf("AccessToken = %q", p.AccessToken())
	}
}

func TestDeadRefreshTokenIsNotRetried(t *testing.T) {
	p, rec, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"state": false, "code": codeRefreshTokenInvalid, "message": "refresh_token invalid",
		})
	})
	_, _, err := p.List(context.Background(), "0", "")
	if !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("error = %v, want ErrAuth", err)
	}
	if n := rec.count("/open/refreshToken"); n != 0 {
		t.Errorf("tried to refresh with a dead refresh token %d times", n)
	}
}

func TestOSSStringToSignMatchesHandComputedVector(t *testing.T) {
	h := http.Header{}
	h.Set("x-oss-security-token", "TOK")
	h.Set("Content-Type", "application/xml")
	q := url.Values{"uploadId": {"UP-1"}, "partNumber": {"2"}}
	got := ossStringToSign("PUT", "", "application/xml", "Mon, 02 Jan 2006 15:04:05 GMT",
		h, "bkt", "a/b.bin", q)
	want := "PUT\n\napplication/xml\nMon, 02 Jan 2006 15:04:05 GMT\n" +
		"x-oss-security-token:TOK\n" +
		"/bkt/a/b.bin?partNumber=2&uploadId=UP-1"
	if got != want {
		t.Fatalf("string to sign =\n%q\nwant\n%q", got, want)
	}
	// A valueless sub-resource keeps its bare form.
	got = ossStringToSign("POST", "", "", "D", http.Header{}, "bkt", "k", url.Values{"uploads": {""}})
	if want := "POST\n\n\nD\n/bkt/k?uploads"; got != want {
		t.Fatalf("uploads string to sign = %q, want %q", got, want)
	}
	if auth := ossAuthorization("AK", "SK", "PUT\n"); !strings.HasPrefix(auth, "OSS AK:") || len(auth) < 12 {
		t.Fatalf("authorization = %q", auth)
	}
}

func TestOSSURLStyles(t *testing.T) {
	if got := ossURL("https://127.0.0.1:1/", "b", "k/1", url.Values{"uploads": {""}}); got != "https://127.0.0.1:1/b/k/1?uploads" {
		t.Errorf("path-style url = %q", got)
	}
	if got := ossURL("oss-cn-shenzhen.aliyuncs.com", "b", "k", nil); got != "https://b.oss-cn-shenzhen.aliyuncs.com/k" {
		t.Errorf("virtual-host url = %q", got)
	}
}

func TestParseSignCheck(t *testing.T) {
	start, end, err := parseSignCheck(" 1024-2047 ")
	if err != nil || start != 1024 || end != 2047 {
		t.Fatalf("parseSignCheck = %d,%d,%v", start, end, err)
	}
	for _, bad := range []string{"", "abc", "10", "20-10"} {
		if _, _, err := parseSignCheck(bad); err == nil {
			t.Errorf("parseSignCheck(%q) should fail", bad)
		}
	}
}

func TestFlexScalars(t *testing.T) {
	var v struct {
		A flexInt    `json:"a"`
		B flexInt64  `json:"b"`
		C flexString `json:"c"`
		D flexBool   `json:"d"`
	}
	if err := json.Unmarshal([]byte(`{"a":"7","b":123456789012,"c":900,"d":"true"}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != 7 || v.B != 123456789012 || v.C != "900" || !bool(v.D) {
		t.Fatalf("decoded = %+v", v)
	}
	if err := json.Unmarshal([]byte(`{"a":null,"c":null,"d":0}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != 0 || v.C != "" || bool(v.D) {
		t.Fatalf("null decode = %+v", v)
	}
}

func TestFirstCallRefreshesWhenOnlyARefreshTokenIsConfigured(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		switch r.URL.Path {
		case "/open/refreshToken":
			// Some 115 deployments answer without the `data` wrapper; the
			// driver must cope with both.
			writeJSON(w, map[string]any{
				"state": true, "code": 0,
				"access_token": "fresh", "refresh_token": "rotated", "expires_in": "7200",
			})
		case "/open/ufile/files":
			if got := r.Header.Get("Authorization"); got != "Bearer fresh" {
				t.Errorf("Authorization = %q, want the freshly minted token", got)
			}
			writeJSON(w, map[string]any{"state": true, "code": 0, "count": 0, "data": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p, err := NewWithOptions("p115", Options{
		Client:       httpx.New(httpx.Options{Remote: "p115"}),
		BaseURL:      srv.URL,
		PassportURL:  srv.URL,
		RefreshToken: "refresh-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.List(context.Background(), "0", ""); err != nil {
		t.Fatal(err)
	}
	if rec.count("/open/refreshToken") != 1 {
		t.Errorf("refreshed %d times", rec.count("/open/refreshToken"))
	}
	if p.RefreshToken() != "rotated" {
		t.Errorf("RefreshToken = %q, want the rotated value persisted", p.RefreshToken())
	}
}
