package pan123

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// call records one request, decoded for assertions.
type call struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   map[string]any
	// Fields and Slice hold a decoded multipart slice upload.
	Fields map[string]string
	Slice  []byte
}

// openAPI is a scriptable stand-in for open-api.123pan.com.
type openAPI struct {
	t *testing.T

	mu       sync.Mutex
	calls    []call
	handlers map[string]func(c call) (int, string)
}

func newAPI(t *testing.T) (*openAPI, *httptest.Server) {
	a := &openAPI{t: t, handlers: map[string]func(call) (int, string){}}
	hs := httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(hs.Close)
	return a, hs
}

func (a *openAPI) on(path string, fn func(c call) (int, string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handlers[path] = fn
}

func (a *openAPI) reply(path, body string) {
	a.on(path, func(call) (int, string) { return http.StatusOK, body })
}

// ok wraps data in the standard envelope.
func ok(data string) string {
	return `{"code":0,"message":"ok","data":` + data + `,"x-traceID":"trace-1"}`
}

func (a *openAPI) serve(w http.ResponseWriter, r *http.Request) {
	c := call{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone()}
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			a.t.Errorf("parse multipart: %v", err)
			break
		}
		c.Fields = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			c.Fields[k] = v[0]
		}
		f, _, err := r.FormFile("slice")
		if err != nil {
			a.t.Errorf("the slice body must be a form field named \"slice\": %v", err)
			break
		}
		c.Slice, _ = io.ReadAll(f)
		f.Close()
	default:
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &c.Body)
		}
	}
	a.mu.Lock()
	a.calls = append(a.calls, c)
	fn := a.handlers[r.URL.Path]
	a.mu.Unlock()
	if fn == nil {
		a.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"code":5066,"message":"文件不存在"}`)
		return
	}
	code, body := fn(c)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	io.WriteString(w, body)
}

func (a *openAPI) last(path string) call {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.calls) - 1; i >= 0; i-- {
		if a.calls[i].Path == path {
			return a.calls[i]
		}
	}
	a.t.Fatalf("no request to %s", path)
	return call{}
}

func (a *openAPI) count(path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.calls {
		if c.Path == path {
			n++
		}
	}
	return n
}

func testClient() *httpx.Client {
	return httpx.New(httpx.Options{
		Remote: "pan123-test",
		Policy: retry.Policy{
			Backoff:     retry.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond},
			MaxAttempts: 2,
		},
	})
}

func newProvider(t *testing.T, hs *httptest.Server, opts ...func(*Options)) *Provider {
	t.Helper()
	o := Options{
		Name: "p123", Client: testClient(), BaseURL: hs.URL,
		ClientID: "cid", ClientSecret: "secret", AccessToken: "tok-1",
		PageSize: 2, PollInterval: time.Millisecond, PollAttempts: 5,
		Now: func() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) },
	}
	for _, f := range opts {
		f(&o)
	}
	p, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestListPaginatesOnLastFileID(t *testing.T) {
	a, hs := newAPI(t)
	a.on(pathList, func(c call) (int, string) {
		if c.Method != http.MethodGet {
			t.Errorf("list used %s", c.Method)
		}
		if got := c.Header.Get("Authorization"); got != "Bearer tok-1" {
			t.Errorf("Authorization = %q", got)
		}
		if got := c.Header.Get("Platform"); got != Platform {
			t.Errorf("Platform = %q, want %q (the gateway rejects the call without it)", got, Platform)
		}
		if c.Query.Get("parentFileId") != "700" {
			t.Errorf("parentFileId = %q", c.Query.Get("parentFileId"))
		}
		if c.Query.Get("limit") != "2" {
			t.Errorf("limit = %q", c.Query.Get("limit"))
		}
		if c.Query.Get("lastFileId") == "" {
			return http.StatusOK, ok(`{"lastFileId":900,"fileList":[
				{"fileId":800,"filename":"a.txt","parentFileId":700,"type":0,"size":11,
				 "etag":"098F6BCD4621D373CADE4E832627B4F6","status":2,"trashed":0,
				 "createAt":"2026-08-01 10:00:00","updateAt":"2026-08-02 18:30:00"},
				{"fileId":900,"filename":"sub","parentFileId":700,"type":1,"size":0,
				 "etag":"","status":2,"trashed":0,"createAt":"2026-08-01 10:00:00","updateAt":"2026-08-01 10:00:00"}
			]}`)
		}
		if c.Query.Get("lastFileId") != "900" {
			t.Errorf("lastFileId = %q, want the cursor from page one", c.Query.Get("lastFileId"))
		}
		return http.StatusOK, ok(`{"lastFileId":-1,"fileList":[
			{"fileId":950,"filename":"gone.txt","parentFileId":700,"type":0,"size":5,"etag":"aa","trashed":1,
			 "createAt":"2026-08-01 10:00:00","updateAt":"2026-08-01 10:00:00"},
			{"fileId":960,"filename":"b.bin","parentFileId":700,"type":0,"size":16777216,"etag":"BB","trashed":0,
			 "createAt":"2026-08-01 10:00:00","updateAt":"2026-08-03 09:00:00"}
		]}`)
	})
	p := newProvider(t, hs)

	entries, next, err := p.List(context.Background(), "700", "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "900" {
		t.Fatalf("next = %q, want the lastFileId 900", next)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %v", entries)
	}
	f := entries[0]
	if f.ID != "800" || f.Name != "a.txt" || f.Size != 11 || f.Kind != provider.KindFile {
		t.Errorf("entry = %+v", f)
	}
	if f.Hashes[provider.HashMD5] != "098f6bcd4621d373cade4e832627b4f6" {
		t.Errorf("etag = %v, want the lowercased md5", f.Hashes)
	}
	// Timestamps have no offset and the service runs on UTC+8.
	if want := time.Date(2026, 8, 2, 18, 30, 0, 0, beijing); !f.ModTime.Equal(want) {
		t.Errorf("mtime = %s, want %s", f.ModTime, want)
	}
	if entries[1].Kind != provider.KindDir {
		t.Errorf("folder entry = %+v", entries[1])
	}

	entries, next, err = p.List(context.Background(), "700", next)
	if err != nil {
		t.Fatal(err)
	}
	if next != "" {
		t.Errorf("lastFileId -1 ends the listing, got %q", next)
	}
	if len(entries) != 1 || entries[0].ID != "960" {
		t.Fatalf("entries = %v; recycle-bin items must be filtered out here", entries)
	}
	if n := a.count(pathList); n != 2 {
		t.Errorf("list called %d times", n)
	}
}

func TestStatUsesCapitalIDSpelling(t *testing.T) {
	a, hs := newAPI(t)
	a.on(pathDetail, func(c call) (int, string) {
		// This endpoint spells the parameter fileID, unlike the list endpoint.
		if c.Query.Get("fileID") != "800" {
			t.Errorf("query = %v, want fileID=800", c.Query)
		}
		return http.StatusOK, ok(`{"fileID":800,"filename":"a.txt","type":0,"size":11,
			"etag":"098f6bcd4621d373cade4e832627b4f6","status":2,"parentFileID":700,
			"createAt":"2026-08-01 10:00:00","trashed":0}`)
	})
	p := newProvider(t, hs)
	e, err := p.Stat(context.Background(), "800")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "800" || e.ParentID != "700" || e.Size != 11 {
		t.Fatalf("entry = %+v", e)
	}
	if e.Version != "098f6bcd4621d373cade4e832627b4f6" {
		t.Errorf("version = %q, want the etag", e.Version)
	}
	_ = a
}

func TestStatNotFound(t *testing.T) {
	a, hs := newAPI(t)
	a.reply(pathDetail, `{"code":5066,"message":"文件不存在","data":null,"x-traceID":"t"}`)
	p := newProvider(t, hs)
	_, err := p.Stat(context.Background(), "404")
	if !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.TraceID != "t" {
		t.Errorf("error must carry the trace id support asks for: %v", err)
	}
}

// cdn stands in for the download host.
type cdn struct {
	mu     sync.Mutex
	data   []byte
	ranges []string
	status int
}

func newCDN(t *testing.T, data []byte) (*cdn, *httptest.Server) {
	c := &cdn{data: data}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.ranges = append(c.ranges, r.Header.Get("Range"))
		status := c.status
		c.mu.Unlock()
		if status != 0 {
			http.Error(w, "expired", status)
			return
		}
		var start, end int64
		if n, _ := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); n == 2 {
			if end >= int64(len(c.data)) {
				end = int64(len(c.data)) - 1
			}
			w.WriteHeader(http.StatusPartialContent)
			w.Write(c.data[start : end+1])
			return
		}
		w.Write(c.data)
	}))
	t.Cleanup(hs.Close)
	return c, hs
}

func TestDownloadURLAndReadRange(t *testing.T) {
	cd, cdnSrv := newCDN(t, []byte("abcdefghijklmnop"))
	a, hs := newAPI(t)
	a.on(pathDownloadInfo, func(c call) (int, string) {
		// download_info is one of the lowercase-Id endpoints.
		if c.Query.Get("fileId") != "800" {
			t.Errorf("query = %v, want fileId=800", c.Query)
		}
		return http.StatusOK, ok(fmt.Sprintf(`{"downloadUrl":%q}`, cdnSrv.URL+"/dl"))
	})
	p := newProvider(t, hs)

	link, err := p.DownloadURL(context.Background(), "800")
	if err != nil {
		t.Fatal(err)
	}
	if link.URL != cdnSrv.URL+"/dl" {
		t.Errorf("url = %q", link.URL)
	}
	if want := time.Date(2026, 9, 2, 12, 30, 0, 0, time.UTC); !link.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %s, want %s", link.ExpiresAt, want)
	}

	rc, err := p.ReadRange(context.Background(), "800", "", 2, 5)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "cdefg" {
		t.Fatalf("read %q, want cdefg", got)
	}
	cd.mu.Lock()
	defer cd.mu.Unlock()
	if len(cd.ranges) != 1 || cd.ranges[0] != "bytes=2-6" {
		t.Fatalf("Range headers = %v, want [bytes=2-6]", cd.ranges)
	}
	cd.mu.Unlock()

	// DownloadURL plus the read must have cost one download_info call, not two.
	if n := a.count(pathDownloadInfo); n != 1 {
		t.Errorf("download_info called %d times, want 1 (the link is cached)", n)
	}
	cd.mu.Lock()
}

func TestReadRangeRefreshesExpiredLink(t *testing.T) {
	cd, cdnSrv := newCDN(t, []byte("hello"))
	cd.status = http.StatusForbidden
	a, hs := newAPI(t)
	a.on(pathDownloadInfo, func(call) (int, string) {
		if a.count(pathDownloadInfo) == 2 {
			cd.mu.Lock()
			cd.status = 0
			cd.mu.Unlock()
		}
		return http.StatusOK, ok(fmt.Sprintf(`{"downloadUrl":%q}`, cdnSrv.URL+"/dl"))
	})
	p := newProvider(t, hs)
	rc, err := p.ReadRange(context.Background(), "800", "", 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "hello" {
		t.Fatalf("read %q", got)
	}
	if n := a.count(pathDownloadInfo); n != 2 {
		t.Errorf("download_info called %d times, want 2 (a 403 must refresh the link)", n)
	}
}

func TestMkdir(t *testing.T) {
	a, hs := newAPI(t)
	a.on(pathMkdir, func(c call) (int, string) {
		if c.Method != http.MethodPost {
			t.Errorf("mkdir used %s", c.Method)
		}
		// mkdir lives under /upload and spells the parent "parentID".
		if c.Body["name"] != "new" || c.Body["parentID"] != float64(700) {
			t.Errorf("body = %v", c.Body)
		}
		return http.StatusOK, ok(`{"dirID":1234}`)
	})
	p := newProvider(t, hs)
	e, err := p.Mkdir(context.Background(), "700", "new")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "1234" || e.Kind != provider.KindDir || e.ParentID != "700" || e.Name != "new" {
		t.Fatalf("entry = %+v", e)
	}
	_ = a
}

func TestMkdirDuplicateNameMapsToExists(t *testing.T) {
	a, hs := newAPI(t)
	// 123 has no numeric code for a collision; it only says so in the message.
	a.reply(pathMkdir, `{"code":1,"message":"文件名不能重名","data":null}`)
	p := newProvider(t, hs)
	_, err := p.Mkdir(context.Background(), "700", "dup")
	if !errors.Is(err, provider.ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	_ = a
}

func TestRenameUsesPut(t *testing.T) {
	a, hs := newAPI(t)
	a.on(pathName, func(c call) (int, string) {
		if c.Method != http.MethodPut {
			t.Errorf("rename used %s, want PUT", c.Method)
		}
		if c.Body["fileId"] != float64(800) || c.Body["fileName"] != "b.txt" {
			t.Errorf("body = %v", c.Body)
		}
		return http.StatusOK, `{"code":0,"message":"ok","data":null}`
	})
	a.reply(pathDetail, ok(`{"fileID":800,"filename":"b.txt","type":0,"size":11,"etag":"aa","parentFileID":700,
		"createAt":"2026-08-01 10:00:00"}`))
	p := newProvider(t, hs)
	e, err := p.Rename(context.Background(), "800", "b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "b.txt" {
		t.Fatalf("entry = %+v", e)
	}
	_ = a
}

func TestMoveAndDelete(t *testing.T) {
	a, hs := newAPI(t)
	a.on(pathMove, func(c call) (int, string) {
		ids, _ := c.Body["fileIDs"].([]any)
		if len(ids) != 1 || ids[0] != float64(800) {
			t.Errorf("fileIDs = %v", c.Body["fileIDs"])
		}
		if c.Body["toParentFileID"] != float64(900) {
			t.Errorf("toParentFileID = %v", c.Body["toParentFileID"])
		}
		return http.StatusOK, `{"code":0,"message":"ok","data":null}`
	})
	a.reply(pathDetail, ok(`{"fileID":800,"filename":"a.txt","type":0,"size":11,"etag":"aa","parentFileID":900,
		"createAt":"2026-08-01 10:00:00"}`))
	a.on(pathTrash, func(c call) (int, string) {
		ids, _ := c.Body["fileIDs"].([]any)
		if len(ids) != 1 || ids[0] != float64(800) {
			t.Errorf("trash fileIDs = %v", c.Body["fileIDs"])
		}
		return http.StatusOK, `{"code":0,"message":"ok","data":null}`
	})
	p := newProvider(t, hs)

	e, err := p.Move(context.Background(), "800", "900")
	if err != nil {
		t.Fatal(err)
	}
	if e.ParentID != "900" {
		t.Fatalf("entry = %+v", e)
	}
	if err := p.Delete(context.Background(), "800"); err != nil {
		t.Fatal(err)
	}
	if a.count(pathTrash) != 1 {
		t.Errorf("trash called %d times", a.count(pathTrash))
	}
}

func TestDeleteRejectsNonNumericID(t *testing.T) {
	_, hs := newAPI(t)
	p := newProvider(t, hs)
	if err := p.Delete(context.Background(), "not-a-number"); err == nil {
		t.Fatal("a non-numeric id must not reach the API")
	}
}

// ---------------------------------------------------------------- upload ---

func TestBeginUploadRapid(t *testing.T) {
	a, hs := newAPI(t)
	a.on(pathUploadCreate, func(c call) (int, string) {
		if c.Body["parentFileID"] != float64(700) {
			t.Errorf("parentFileID = %v", c.Body["parentFileID"])
		}
		if c.Body["etag"] != "098f6bcd4621d373cade4e832627b4f6" {
			t.Errorf("etag = %v, want the lowercased md5", c.Body["etag"])
		}
		if c.Body["size"] != float64(11) {
			t.Errorf("size = %v", c.Body["size"])
		}
		if c.Body["duplicate"] != float64(2) {
			t.Errorf("duplicate = %v, want 2 (overwrite)", c.Body["duplicate"])
		}
		return http.StatusOK, ok(`{"fileID":4242,"reuse":true,"preuploadID":"","sliceSize":16777216,"servers":[]}`)
	})
	p := newProvider(t, hs)
	sess, err := p.BeginUpload(context.Background(), "700", "a.txt", 11,
		provider.Hashes{provider.HashMD5: "098F6BCD4621D373CADE4E832627B4F6"})
	if err != nil {
		t.Fatal(err)
	}
	if !sess.RapidDone || sess.Entry == nil {
		t.Fatalf("session = %+v, want a rapid hit", sess)
	}
	if sess.Entry.ID != "4242" || sess.Entry.Size != 11 {
		t.Fatalf("entry = %+v", sess.Entry)
	}
	if sess.Entry.Hashes[provider.HashMD5] != "098f6bcd4621d373cade4e832627b4f6" {
		t.Errorf("entry hashes = %v", sess.Entry.Hashes)
	}
	_ = a
}

func TestBeginUploadReuseWithoutFileIDFallsBack(t *testing.T) {
	a, hs := newAPI(t)
	a.reply(pathUploadCreate, ok(`{"fileID":0,"reuse":true,"preuploadID":"pre-9","sliceSize":1048576,
		"servers":["https://upload.example"]}`))
	p := newProvider(t, hs)
	sess, err := p.BeginUpload(context.Background(), "700", "a.txt", 11, provider.Hashes{provider.HashMD5: "aa"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.RapidDone {
		t.Fatal("reuse=true with fileID 0 is not a completed upload")
	}
	if sess.ID != "pre-9" || sess.PartSize != 1048576 {
		t.Fatalf("session = %+v; the server's sliceSize must win", sess)
	}
}

func TestSliceUploadRoundTrip(t *testing.T) {
	slice0 := strings.Repeat("a", 1024)
	slice1 := "tail"
	var uploadSrv *httptest.Server

	a, hs := newAPI(t)
	slices := map[string][]byte{}
	var mu sync.Mutex
	uploadSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathUploadSlice {
			t.Errorf("slice went to %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok-1" || r.Header.Get("Platform") != Platform {
			t.Errorf("slice upload lost its auth headers: %v", r.Header)
		}
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		if got := r.MultipartForm.Value["preuploadID"]; len(got) != 1 || got[0] != "pre-1" {
			t.Errorf("preuploadID = %v", got)
		}
		no := r.MultipartForm.Value["sliceNo"][0]
		f, _, err := r.FormFile("slice")
		if err != nil {
			t.Fatalf("the payload field must be named \"slice\": %v", err)
		}
		body, _ := io.ReadAll(f)
		f.Close()
		sum := md5.Sum(body)
		if got := r.MultipartForm.Value["sliceMD5"][0]; got != hex.EncodeToString(sum[:]) {
			t.Errorf("sliceMD5 = %q, want %q", got, hex.EncodeToString(sum[:]))
		}
		mu.Lock()
		slices[no] = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"message":"ok","data":null}`)
	}))
	defer uploadSrv.Close()

	a.on(pathUploadCreate, func(c call) (int, string) {
		return http.StatusOK, ok(fmt.Sprintf(`{"fileID":0,"reuse":false,"preuploadID":"pre-1",
			"sliceSize":1024,"servers":[%q]}`, uploadSrv.URL))
	})
	var completes int
	a.on(pathUploadComplete, func(c call) (int, string) {
		completes++
		if c.Body["preuploadID"] != "pre-1" {
			t.Errorf("complete body = %v", c.Body)
		}
		if completes == 1 {
			// The server is still assembling: the driver must poll again.
			return http.StatusOK, ok(`{"completed":false,"fileID":0}`)
		}
		return http.StatusOK, ok(`{"completed":true,"fileID":5150}`)
	})
	p := newProvider(t, hs)

	size := int64(len(slice0) + len(slice1))
	sess, err := p.BeginUpload(context.Background(), "700", "big.bin", size,
		provider.Hashes{provider.HashMD5: "cafebabecafebabecafebabecafebabe"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.PartSize != 1024 {
		t.Fatalf("part size = %d, want the server's 1024", sess.PartSize)
	}
	t0, err := p.UploadPart(context.Background(), sess, 0, strings.NewReader(slice0), int64(len(slice0)))
	if err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum([]byte(slice0))
	if t0.ETag != hex.EncodeToString(sum[:]) {
		t.Errorf("part token = %+v, want the slice md5", t0)
	}
	if _, err := p.UploadPart(context.Background(), sess, 1, strings.NewReader(slice1), int64(len(slice1))); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if string(slices["1"]) != slice0 || string(slices["2"]) != slice1 {
		t.Errorf("slice numbering is 1-based; got %d slices: %q…", len(slices), slices["1"][:min(4, len(slices["1"]))])
	}
	mu.Unlock()

	e, err := p.CompleteUpload(context.Background(), sess, []provider.PartToken{t0})
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "5150" || e.Size != size || e.Name != "big.bin" || e.ParentID != "700" {
		t.Fatalf("entry = %+v", e)
	}
	if completes != 2 {
		t.Errorf("upload_complete called %d times, want 2 (one incomplete poll)", completes)
	}
}

func TestCompleteUploadGivesUpAfterPolling(t *testing.T) {
	a, hs := newAPI(t)
	a.reply(pathUploadComplete, ok(`{"completed":false,"fileID":0}`))
	p := newProvider(t, hs)
	sess := provider.UploadSession{ID: "pre-1", Opaque: map[string]string{"preupload_id": "pre-1"}}
	_, err := p.CompleteUpload(context.Background(), sess, nil)
	if !errors.Is(err, provider.ErrTransient) {
		t.Fatalf("err = %v, want ErrTransient so the journal retries later", err)
	}
	if n := a.count(pathUploadComplete); n != 5 {
		t.Errorf("polled %d times, want the configured 5", n)
	}
}

func TestUploadPartHashesSeekableReaderWithoutBuffering(t *testing.T) {
	// A SectionReader is what the upload layer passes; hashing must leave it
	// positioned back at the start of the part.
	r := strings.NewReader("0123456789")
	sum, out, err := hashAndRewind(r, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := md5.Sum([]byte("0123456789"))
	if sum != hex.EncodeToString(want[:]) {
		t.Errorf("md5 = %q", sum)
	}
	if out != io.Reader(r) {
		t.Error("a seekable reader must be reused, not copied into memory")
	}
	got, _ := io.ReadAll(out)
	if string(got) != "0123456789" {
		t.Errorf("reader was left at %q", got)
	}

	// A non-seekable reader is buffered instead.
	sum2, out2, err := hashAndRewind(struct{ io.Reader }{strings.NewReader("abc")}, 3)
	if err != nil {
		t.Fatal(err)
	}
	w2 := md5.Sum([]byte("abc"))
	if sum2 != hex.EncodeToString(w2[:]) {
		t.Errorf("md5 = %q", sum2)
	}
	got2, _ := io.ReadAll(out2)
	if string(got2) != "abc" {
		t.Errorf("buffered reader = %q", got2)
	}
}

// ---------------------------------------------------------- error mapping ---

func TestRefreshesTokenOn401(t *testing.T) {
	a, hs := newAPI(t)
	a.on(pathToken, func(c call) (int, string) {
		if c.Body["clientID"] != "cid" || c.Body["clientSecret"] != "secret" {
			t.Errorf("token body = %v", c.Body)
		}
		if c.Header.Get("Platform") != Platform {
			t.Errorf("the token call still needs the Platform header, got %v", c.Header)
		}
		return http.StatusOK, ok(`{"accessToken":"tok-2","expiredAt":"2026-10-02T15:48:37+08:00"}`)
	})
	a.on(pathList, func(c call) (int, string) {
		if c.Header.Get("Authorization") == "Bearer tok-1" {
			return http.StatusOK, `{"code":401,"message":"token is invalid","data":null}`
		}
		if c.Header.Get("Authorization") != "Bearer tok-2" {
			t.Errorf("retry used %q", c.Header.Get("Authorization"))
		}
		return http.StatusOK, ok(`{"lastFileId":-1,"fileList":[]}`)
	})
	p := newProvider(t, hs)
	if _, _, err := p.List(context.Background(), "0", ""); err != nil {
		t.Fatal(err)
	}
	if n := a.count(pathList); n != 2 {
		t.Errorf("list called %d times, want 2", n)
	}
	if p.AccessToken() != "tok-2" {
		t.Errorf("token = %q", p.AccessToken())
	}
}

func TestErrorCodeMapping(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{"unauthorized", `{"code":401,"message":"token is invalid"}`, provider.ErrAuth},
		{"throttled", `{"code":429,"message":"请求太频繁"}`, provider.ErrRateLimited},
		{"not found", `{"code":5066,"message":"文件不存在"}`, provider.ErrNotFound},
		{"internal", `{"code":1,"message":"internal error"}`, provider.ErrTransient},
		{"duplicate name", `{"code":1,"message":"不能重名"}`, provider.ErrExists},
		{"daily quota", `{"code":5113,"message":"您今日自用下载流量已超出1GB上限"}`, provider.ErrRateLimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, hs := newAPI(t)
			a.reply(pathDetail, tc.body)
			// The 401 case refreshes once; the refreshed token hits the same
			// failing handler, which is what a truly revoked token looks like.
			a.reply(pathToken, ok(`{"accessToken":"tok-2","expiredAt":"2026-10-02T15:48:37+08:00"}`))
			p := newProvider(t, hs)
			_, err := p.Stat(context.Background(), "800")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestHTTPStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, provider.ErrAuth},
		{http.StatusNotFound, provider.ErrNotFound},
		{http.StatusTooManyRequests, provider.ErrRateLimited},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			a, hs := newAPI(t)
			a.on(pathDetail, func(call) (int, string) { return tc.status, `{"code":0}` })
			a.reply(pathToken, ok(`{"accessToken":"tok-2","expiredAt":"2026-10-02T15:48:37+08:00"}`))
			p := newProvider(t, hs)
			_, err := p.Stat(context.Background(), "800")
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d gave %v, want %v", tc.status, err, tc.want)
			}
		})
	}
}

func TestCapabilities(t *testing.T) {
	_, hs := newAPI(t)
	p := newProvider(t, hs)
	c := p.Capabilities()
	if c.Delta {
		t.Error("123 publishes no delta feed")
	}
	if _, ok := any(p).(provider.ChangeLister); ok {
		t.Error("the driver must not implement ChangeLister")
	}
	if _, ok := any(p).(provider.ServerCopier); ok {
		t.Error("the Open API has no copy endpoint, so ServerCopier must not be implemented")
	}
	if c.PartSize != 16<<20 {
		t.Errorf("part size = %d, want 16 MiB", c.PartSize)
	}
	if c.ServerCopy {
		t.Error("ServerCopy must be false")
	}
	if !c.RangeRead || !c.ServerMove || !c.ServerRename {
		t.Errorf("caps = %+v", c)
	}
	if c.Tier != provider.TierOfficial {
		t.Errorf("tier = %v", c.Tier)
	}
	if len(c.HashTypes) != 1 || c.HashTypes[0] != provider.HashMD5 {
		t.Errorf("hash types = %v", c.HashTypes)
	}
	if len(c.RapidUpload) != 1 || c.RapidUpload[0] != provider.HashMD5 {
		t.Errorf("rapid upload hashes = %v", c.RapidUpload)
	}
}

func TestFactoryRequiresCredentials(t *testing.T) {
	if _, err := Factory("p", map[string]any{}); err == nil || !strings.Contains(err.Error(), "client_id") {
		t.Fatalf("err = %v, want it to name the missing client_id key", err)
	}
	if _, err := Factory("p", map[string]any{"client_id": "x"}); err == nil ||
		!strings.Contains(err.Error(), "client_secret") {
		t.Fatalf("err = %v, want it to name the missing client_secret key", err)
	}
	p, err := Factory("p", map[string]any{"client_id": "x", "client_secret": "y"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "p" {
		t.Errorf("name = %q", p.Name())
	}
}

func TestRegistered(t *testing.T) {
	p, err := provider.New("pan123", "p", map[string]any{"client_id": "x", "client_secret": "y"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*Provider); !ok {
		t.Fatalf("registry returned %T", p)
	}
}
