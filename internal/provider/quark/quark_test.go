package quark

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// ---------------------------------------------------------------- test harness

type recorded struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// capture records every request the fake service received so a test can assert
// what was actually sent, not merely that a call succeeded.
type capture struct {
	mu   sync.Mutex
	reqs []recorded
}

func (c *capture) add(r *http.Request) []byte {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, recorded{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(),
		Header: r.Header.Clone(), Body: body,
	})
	return body
}

func (c *capture) byPath(p string) []recorded {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []recorded
	for _, r := range c.reqs {
		if r.Path == p {
			out = append(out, r)
		}
	}
	return out
}

func (c *capture) count(p string) int { return len(c.byPath(p)) }

func (c *capture) last(t *testing.T, p string) recorded {
	t.Helper()
	rs := c.byPath(p)
	if len(rs) == 0 {
		t.Fatalf("no request recorded for %s", p)
	}
	return rs[len(rs)-1]
}

func writeEnvelope(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{"status": 200, "code": 0, "message": "ok", "req_id": "req-1"}
	if data != nil {
		body["data"] = data
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeList(w http.ResponseWriter, list any, total int) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": 200, "code": 0, "message": "ok", "req_id": "req-1",
		"data":     map[string]any{"list": list},
		"metadata": map[string]any{"_total": total, "_size": 2, "_page": 1, "_count": 2},
	})
}

func writeFailure(w http.ResponseWriter, httpStatus, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": httpStatus, "code": code, "message": message, "req_id": "req-err",
	})
}

func decodeBody(t *testing.T, r recorded, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decode request body for %s: %v (body %s)", r.Path, err, r.Body)
	}
}

// newTestQuark builds a driver pointed at srv with test-friendly pacing.
func newTestQuark(t *testing.T, srv *httptest.Server) *Quark {
	t.Helper()
	q, err := NewWithOptions("q", Options{
		Client:  httpx.New(httpx.Options{Remote: "q", Policy: retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond}, MaxAttempts: 2}}),
		BaseURL: srv.URL,
		Cookie:  "__pus=abc; __puus=v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	q.taskPollInterval = time.Millisecond
	q.pageSize = 2
	return q
}

const (
	pList     = "/1/clouddrive/file/sort"
	pInfo     = "/1/clouddrive/file/info"
	pFile     = "/1/clouddrive/file"
	pRename   = "/1/clouddrive/file/rename"
	pMove     = "/1/clouddrive/file/move"
	pDelete   = "/1/clouddrive/file/delete"
	pTask     = "/1/clouddrive/task"
	pDownload = "/1/clouddrive/file/download"
	pPre      = "/1/clouddrive/file/upload/pre"
	pHash     = "/1/clouddrive/file/update/hash"
	pAuth     = "/1/clouddrive/file/upload/auth"
	pFinish   = "/1/clouddrive/file/upload/finish"
)

// ---------------------------------------------------------------- basic wiring

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New("q", map[string]any{}); err == nil {
		t.Fatal("missing cookie should be an error")
	} else if !strings.Contains(err.Error(), `"cookie"`) {
		t.Fatalf("error should name the missing key, got %v", err)
	}
	if _, err := New("q", map[string]any{"cookie": "   "}); err == nil {
		t.Fatal("blank cookie should be an error")
	}
	q, err := New("q", map[string]any{"cookie": "a=b", "root_id": 0})
	if err != nil {
		t.Fatal(err)
	}
	// root_id: 0 in YAML decodes to an int; it must still reach the driver as
	// the string id the API expects.
	if q.RootID() != "0" {
		t.Fatalf("RootID = %q, want \"0\"", q.RootID())
	}
	if q.Name() != "q" {
		t.Fatalf("Name = %q", q.Name())
	}
}

func TestRegisteredWithProviderRegistry(t *testing.T) {
	p, err := provider.New("quark", "q", map[string]any{"cookie": "a=b"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "q" {
		t.Fatalf("Name = %q", p.Name())
	}
	if _, err := provider.New("quark", "q", map[string]any{}); err == nil {
		t.Fatal("factory should reject a config without a cookie")
	}
}

func TestCapabilities(t *testing.T) {
	q, err := NewWithOptions("q", Options{Cookie: "a=b"})
	if err != nil {
		t.Fatal(err)
	}
	c := q.Capabilities()
	if c.Tier != provider.TierUnofficial {
		t.Errorf("Tier = %v, want unofficial", c.Tier)
	}
	if c.QPS != (provider.QPS{Meta: 1, Download: 2, Upload: 1}) {
		t.Errorf("QPS = %+v", c.QPS)
	}
	if c.Delta {
		t.Error("Quark has no delta feed; Caps.Delta must be false")
	}
	if _, ok := any(q).(provider.ChangeLister); ok {
		t.Error("driver must not implement ChangeLister")
	}
	if c.LinkShareable {
		t.Error("Quark links need the account cookie; LinkShareable must be false")
	}
	if c.LinkHeaders["User-Agent"] != DefaultUserAgent || c.LinkHeaders["Referer"] != DefaultReferer {
		t.Errorf("LinkHeaders = %v", c.LinkHeaders)
	}
	if c.PartSize != 8<<20 {
		t.Errorf("PartSize = %d", c.PartSize)
	}
	if len(c.RapidUpload) != 2 {
		t.Errorf("RapidUpload = %v, want md5+sha1", c.RapidUpload)
	}
}

// ---------------------------------------------------------------- metadata

func TestListPaginates(t *testing.T) {
	rec := &capture{}
	mux := http.NewServeMux()
	mux.HandleFunc(pList, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		switch r.URL.Query().Get("_page") {
		case "1":
			writeList(w, []map[string]any{
				{"fid": "d1", "file_name": "sub", "pdir_fid": "root", "dir": true, "file_type": 0, "updated_at": int64(1700000000000)},
				{"fid": "f1", "file_name": "a.bin", "pdir_fid": "root", "file": true, "file_type": 1, "size": 42, "updated_at": int64(1700000001000), "md5": "abc123"},
			}, 3)
		case "2":
			writeList(w, []map[string]any{
				{"fid": "f2", "file_name": "b.bin", "pdir_fid": "root", "file": true, "file_type": 1, "size": 7, "updated_at": int64(1700000002000)},
			}, 3)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("_page"))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	q := newTestQuark(t, srv)

	entries, next, err := q.List(context.Background(), "root", "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "2" {
		t.Fatalf("next cursor = %q, want 2", next)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].Kind != provider.KindDir || entries[0].ID != "d1" || entries[0].Size != 0 {
		t.Errorf("dir entry = %+v", entries[0])
	}
	f := entries[1]
	if f.Kind != provider.KindFile || f.Size != 42 || f.Name != "a.bin" || f.ParentID != "root" {
		t.Errorf("file entry = %+v", f)
	}
	if f.Hashes[provider.HashMD5] != "abc123" || f.Version != "abc123" {
		t.Errorf("hashes/version = %v / %q", f.Hashes, f.Version)
	}
	if got, want := f.ModTime.UTC(), time.UnixMilli(1700000001000).UTC(); !got.Equal(want) {
		t.Errorf("ModTime = %v, want %v", got, want)
	}

	page1 := rec.last(t, pList)
	if page1.Query.Get("pdir_fid") != "root" || page1.Query.Get("_size") != "2" ||
		page1.Query.Get("pr") != "ucpro" || page1.Query.Get("fr") != "pc" || page1.Query.Get("_fetch_total") != "1" {
		t.Errorf("list query = %v", page1.Query)
	}

	entries, next, err = q.List(context.Background(), "root", next)
	if err != nil {
		t.Fatal(err)
	}
	if next != "" {
		t.Fatalf("last page should end the listing, got cursor %q", next)
	}
	if len(entries) != 1 || entries[0].ID != "f2" {
		t.Fatalf("page 2 = %+v", entries)
	}
	if rec.count(pList) != 2 {
		t.Fatalf("expected exactly 2 list calls, got %d", rec.count(pList))
	}
	if _, _, err := q.List(context.Background(), "root", "not-a-number"); err == nil {
		t.Error("a malformed cursor should be rejected")
	}
}

func TestStat(t *testing.T) {
	rec := &capture{}
	mux := http.NewServeMux()
	mux.HandleFunc(pInfo, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		if r.URL.Query().Get("fid") != "f1" {
			writeFailure(w, 200, 41013, "文件不存在")
			return
		}
		writeEnvelope(w, map[string]any{
			"fid": "f1", "file_name": "a.bin", "pdir_fid": "root",
			"file": true, "file_type": 1, "size": 42, "updated_at": int64(1700000001000),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	q := newTestQuark(t, srv)

	e, err := q.Stat(context.Background(), "f1")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "f1" || e.Name != "a.bin" || e.Size != 42 || e.Kind != provider.KindFile {
		t.Fatalf("entry = %+v", e)
	}
	if _, err := q.Stat(context.Background(), "nope"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("unknown id error = %v, want ErrNotFound", err)
	}
	if _, err := q.Stat(context.Background(), ""); err == nil {
		t.Error("empty id should be rejected before any request")
	}
}

func TestMkdir(t *testing.T) {
	rec := &capture{}
	mux := http.NewServeMux()
	mux.HandleFunc(pFile, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		if r.Method != http.MethodPost {
			t.Errorf("mkdir method = %s", r.Method)
		}
		writeEnvelope(w, map[string]any{"fid": "newdir", "file_name": "photos"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	q := newTestQuark(t, srv)

	e, err := q.Mkdir(context.Background(), "root", "photos")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "newdir" || e.Name != "photos" || e.Kind != provider.KindDir || e.ParentID != "root" {
		t.Fatalf("entry = %+v", e)
	}
	var body map[string]any
	decodeBody(t, rec.last(t, pFile), &body)
	if body["pdir_fid"] != "root" || body["file_name"] != "photos" || body["dir_init_lock"] != false {
		t.Fatalf("mkdir body = %v", body)
	}
}

func TestRenameReReadsTheEntry(t *testing.T) {
	rec := &capture{}
	mux := http.NewServeMux()
	mux.HandleFunc(pRename, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		writeEnvelope(w, nil)
	})
	mux.HandleFunc(pInfo, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		writeEnvelope(w, map[string]any{
			"fid": "f1", "file_name": "renamed.bin", "pdir_fid": "root", "file": true, "size": 9,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	q := newTestQuark(t, srv)

	e, err := q.Rename(context.Background(), "f1", "renamed.bin")
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "renamed.bin" || e.Size != 9 {
		t.Fatalf("entry = %+v", e)
	}
	var body map[string]any
	decodeBody(t, rec.last(t, pRename), &body)
	if body["fid"] != "f1" || body["file_name"] != "renamed.bin" {
		t.Fatalf("rename body = %v", body)
	}
}

func TestRenameSurvivesAnUnverifiableStat(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(pRename, func(w http.ResponseWriter, r *http.Request) { writeEnvelope(w, nil) })
	// Stat's endpoint shape is unverified; if it is wrong on a live account
	// the rename has still happened and must not be reported as a failure.
	mux.HandleFunc(pInfo, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	q := newTestQuark(t, srv)

	e, err := q.Rename(context.Background(), "f1", "renamed.bin")
	if err != nil {
		t.Fatalf("rename must not fail because the follow-up stat did: %v", err)
	}
	if e.ID != "f1" || e.Name != "renamed.bin" {
		t.Fatalf("fallback entry = %+v", e)
	}
}

func TestMovePollsTheTask(t *testing.T) {
	rec := &capture{}
	mux := http.NewServeMux()
	mux.HandleFunc(pMove, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		writeEnvelope(w, map[string]any{"task_id": "T1"})
	})
	mux.HandleFunc(pTask, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		// First poll still running, second finished.
		if len(rec.byPath(pTask)) == 1 {
			writeEnvelope(w, map[string]any{"task_id": "T1", "status": 0})
			return
		}
		writeEnvelope(w, map[string]any{"task_id": "T1", "status": 2})
	})
	mux.HandleFunc(pInfo, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, map[string]any{"fid": "f1", "file_name": "a.bin", "pdir_fid": "dst", "file": true, "size": 3})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	q := newTestQuark(t, srv)

	e, err := q.Move(context.Background(), "f1", "dst")
	if err != nil {
		t.Fatal(err)
	}
	if e.ParentID != "dst" {
		t.Fatalf("entry = %+v", e)
	}
	var body map[string]any
	decodeBody(t, rec.last(t, pMove), &body)
	if body["to_pdir_fid"] != "dst" || body["action_type"] != float64(1) {
		t.Fatalf("move body = %v", body)
	}
	if fl, ok := body["filelist"].([]any); !ok || len(fl) != 1 || fl[0] != "f1" {
		t.Fatalf("move filelist = %v", body["filelist"])
	}
	polls := rec.byPath(pTask)
	if len(polls) != 2 {
		t.Fatalf("expected 2 task polls, got %d", len(polls))
	}
	if polls[0].Query.Get("retry_index") != "0" || polls[1].Query.Get("retry_index") != "1" {
		t.Fatalf("retry_index not incremented: %q, %q",
			polls[0].Query.Get("retry_index"), polls[1].Query.Get("retry_index"))
	}
	if polls[0].Query.Get("task_id") != "T1" {
		t.Fatalf("task_id = %q", polls[0].Query.Get("task_id"))
	}
}

func TestMoveReportsAFailedTask(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(pMove, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, map[string]any{"task_id": "T1"})
	})
	mux.HandleFunc(pTask, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, map[string]any{"task_id": "T1", "status": 3, "message": "目标目录不存在"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	q := newTestQuark(t, srv)

	if _, err := q.Move(context.Background(), "f1", "dst"); err == nil {
		t.Fatal("a failed task must surface as an error")
	} else if !strings.Contains(err.Error(), "status 3") {
		t.Fatalf("error should name the task status, got %v", err)
	}
}

func TestDelete(t *testing.T) {
	rec := &capture{}
	mux := http.NewServeMux()
	mux.HandleFunc(pDelete, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		writeEnvelope(w, map[string]any{"task_id": "T2"})
	})
	mux.HandleFunc(pTask, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		writeEnvelope(w, map[string]any{"task_id": "T2", "status": 2})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	q := newTestQuark(t, srv)

	if err := q.Delete(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	decodeBody(t, rec.last(t, pDelete), &body)
	if body["action_type"] != float64(2) {
		t.Fatalf("delete body = %v", body)
	}
	if rec.count(pTask) != 1 {
		t.Fatalf("expected one task poll, got %d", rec.count(pTask))
	}
}

// ---------------------------------------------------------------- downloads

func downloadServer(t *testing.T, rec *capture, payload string, cdnStatus func() int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc(pDownload, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		writeEnvelope(w, []map[string]any{
			{"fid": "f1", "file_name": "a.bin", "download_url": srv.URL + "/cdn/f1"},
		})
	})
	mux.HandleFunc("/cdn/", func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		if cdnStatus != nil {
			if code := cdnStatus(); code != 0 {
				w.WriteHeader(code)
				return
			}
		}
		rng := r.Header.Get("Range")
		if rng != "bytes=2-5" {
			t.Errorf("Range header = %q, want bytes=2-5", rng)
		}
		w.Header().Set("Content-Range", "bytes 2-5/"+fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, payload[2:6])
	})
	srv = httptest.NewServer(mux)
	return srv
}

func TestDownloadURLCarriesTheRequiredHeaders(t *testing.T) {
	rec := &capture{}
	srv := downloadServer(t, rec, "0123456789", nil)
	defer srv.Close()
	q := newTestQuark(t, srv)

	link, err := q.DownloadURL(context.Background(), "f1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(link.URL, "/cdn/f1") {
		t.Fatalf("URL = %q", link.URL)
	}
	if link.Headers["User-Agent"] != DefaultUserAgent || link.Headers["Referer"] != DefaultReferer {
		t.Fatalf("link headers = %v", link.Headers)
	}
	if !strings.Contains(link.Headers["Cookie"], "__puus=v1") {
		t.Fatalf("link must carry the account cookie, got %q", link.Headers["Cookie"])
	}
	if link.ExpiresAt.IsZero() || time.Until(link.ExpiresAt) > LinkTTL+time.Minute {
		t.Fatalf("ExpiresAt = %v", link.ExpiresAt)
	}
	var body map[string]any
	decodeBody(t, rec.last(t, pDownload), &body)
	if fids, ok := body["fids"].([]any); !ok || len(fids) != 1 || fids[0] != "f1" {
		t.Fatalf("download body = %v", body)
	}
}

func TestReadRangeSendsRangeAndCachesTheLink(t *testing.T) {
	rec := &capture{}
	const payload = "0123456789"
	srv := downloadServer(t, rec, payload, nil)
	defer srv.Close()
	q := newTestQuark(t, srv)

	for i := 0; i < 2; i++ {
		rc, err := q.ReadRange(context.Background(), "f1", "v1", 2, 4)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != payload[2:6] {
			t.Fatalf("read %q, want %q", got, payload[2:6])
		}
	}
	if n := rec.count(pDownload); n != 1 {
		t.Fatalf("link should be resolved once for two reads, got %d resolutions", n)
	}
	cdn := rec.byPath("/cdn/f1")
	if len(cdn) != 2 {
		t.Fatalf("expected 2 CDN reads, got %d", len(cdn))
	}
	h := cdn[0].Header
	if h.Get("User-Agent") != DefaultUserAgent {
		t.Errorf("CDN User-Agent = %q", h.Get("User-Agent"))
	}
	if h.Get("Referer") != DefaultReferer {
		t.Errorf("CDN Referer = %q", h.Get("Referer"))
	}
	if !strings.Contains(h.Get("Cookie"), "__puus=v1") {
		t.Errorf("CDN Cookie = %q", h.Get("Cookie"))
	}
	if h.Get("Range") != "bytes=2-5" {
		t.Errorf("Range = %q", h.Get("Range"))
	}
}

func TestReadRangeMapsExpiredLink(t *testing.T) {
	rec := &capture{}
	var expired atomic.Bool
	expired.Store(true)
	srv := downloadServer(t, rec, "0123456789", func() int {
		if expired.Load() {
			return http.StatusForbidden
		}
		return 0
	})
	defer srv.Close()
	q := newTestQuark(t, srv)

	_, err := q.ReadRange(context.Background(), "f1", "", 2, 4)
	if !errors.Is(err, provider.ErrLinkExpired) {
		t.Fatalf("error = %v, want ErrLinkExpired", err)
	}
	if retry.Classify(err) != retry.ClassLinkExpired {
		t.Fatalf("classified as %v", retry.Classify(err))
	}
	// The dead link must have been dropped, so the next read resolves again.
	expired.Store(false)
	rc, err := q.ReadRange(context.Background(), "f1", "", 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if n := rec.count(pDownload); n != 2 {
		t.Fatalf("expired link was not invalidated: %d resolutions", n)
	}
}

// ---------------------------------------------------------------- uploads

func TestBeginUploadRapidHit(t *testing.T) {
	rec := &capture{}
	mux := http.NewServeMux()
	mux.HandleFunc(pPre, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		writeEnvelope(w, map[string]any{
			"task_id": "T9", "finish": false, "upload_id": "U9", "obj_key": "oss/obj9",
			"bucket": "quark-bucket", "upload_url": "https://oss.example",
			"auth_info": "AUTHINFO", "part_size": 4 << 20,
		})
	})
	mux.HandleFunc(pHash, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		writeEnvelope(w, map[string]any{"finish": true, "fid": "rapid1"})
	})
	mux.HandleFunc(pInfo, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		writeEnvelope(w, map[string]any{
			"fid": "rapid1", "file_name": "a.bin", "pdir_fid": "root", "file": true, "size": 1024,
			"md5": "d41d8", "updated_at": int64(1700000009000),
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	q := newTestQuark(t, srv)

	s, err := q.BeginUpload(context.Background(), "root", "a.bin", 1024,
		provider.Hashes{provider.HashMD5: "d41d8", provider.HashSHA1: "da39a3"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.RapidDone || s.Entry == nil {
		t.Fatalf("session = %+v", s)
	}
	if s.Entry.ID != "rapid1" || s.Entry.Size != 1024 {
		t.Fatalf("entry = %+v", s.Entry)
	}
	if s.PartSize != 4<<20 {
		t.Fatalf("PartSize = %d, want the server's 4 MiB", s.PartSize)
	}
	var pre map[string]any
	decodeBody(t, rec.last(t, pPre), &pre)
	if pre["pdir_fid"] != "root" || pre["file_name"] != "a.bin" || pre["size"] != float64(1024) || pre["ccp_hash_update"] != true {
		t.Fatalf("pre body = %v", pre)
	}
	var hash map[string]any
	decodeBody(t, rec.last(t, pHash), &hash)
	if hash["task_id"] != "T9" || hash["md5"] != "d41d8" || hash["sha1"] != "da39a3" {
		t.Fatalf("hash body = %v", hash)
	}
}

func TestBeginUploadSkipsHashWhenOnlyOneDigestIsKnown(t *testing.T) {
	rec := &capture{}
	mux := http.NewServeMux()
	mux.HandleFunc(pPre, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		writeEnvelope(w, map[string]any{
			"task_id": "T9", "upload_id": "U9", "obj_key": "oss/obj9",
			"bucket": "b", "upload_url": "https://oss.example", "auth_info": "A",
		})
	})
	mux.HandleFunc(pHash, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		t.Error("hash handshake must not be attempted without both md5 and sha1")
		writeEnvelope(w, map[string]any{"finish": false})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	q := newTestQuark(t, srv)

	s, err := q.BeginUpload(context.Background(), "root", "a.bin", 10, provider.Hashes{provider.HashMD5: "only-md5"})
	if err != nil {
		t.Fatal(err)
	}
	if s.RapidDone {
		t.Fatal("no rapid upload was possible")
	}
	if s.PartSize != defaultPartSize {
		t.Fatalf("PartSize = %d, want the default", s.PartSize)
	}
	if s.Opaque[OpaqueTaskID] != "T9" || s.Opaque[OpaqueUploadID] != "U9" ||
		s.Opaque[OpaqueObjKey] != "oss/obj9" || s.Opaque[OpaqueAuthInfo] != "A" ||
		s.Opaque[OpaqueParentID] != "root" || s.Opaque[OpaqueFileName] != "a.bin" ||
		s.Opaque[OpaqueSize] != "10" {
		t.Fatalf("opaque = %v", s.Opaque)
	}
	if rec.count(pHash) != 0 {
		t.Fatal("hash endpoint was called")
	}
}

// uploadServer serves the whole chunked path, including the OSS object host.
func uploadServer(t *testing.T, rec *capture) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc(pPre, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		writeEnvelope(w, map[string]any{
			"task_id": "T1", "finish": false, "upload_id": "UP1", "obj_key": "oss/obj1",
			"bucket": "quark-bucket", "upload_url": srv.URL, "auth_info": "AUTHINFO",
			"callback": map[string]any{"callbackUrl": "https://callback.quark", "callbackBody": "fid=${x:fid}"},
		})
	})
	mux.HandleFunc(pAuth, func(w http.ResponseWriter, r *http.Request) {
		body := rec.add(r)
		var req struct {
			AuthInfo string `json:"auth_info"`
			AuthMeta string `json:"auth_meta"`
			TaskID   string `json:"task_id"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("auth body: %v", err)
		}
		if req.AuthInfo != "AUTHINFO" || req.TaskID != "T1" {
			t.Errorf("auth request = %+v", req)
		}
		writeEnvelope(w, map[string]any{"auth_key": "OSS quark:sig-for-" + fmt.Sprint(len(req.AuthMeta))})
	})
	mux.HandleFunc("/oss/", func(w http.ResponseWriter, r *http.Request) {
		body := rec.add(r)
		if r.Header.Get("Cookie") != "" {
			t.Errorf("the account cookie must never be sent to the OSS host, got %q", r.Header.Get("Cookie"))
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "OSS quark:") {
			t.Errorf("OSS Authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.Method {
		case http.MethodPut:
			w.Header().Set("ETag", `"etag-`+r.URL.Query().Get("partNumber")+`"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodPost:
			var doc completeXML
			if err := xml.Unmarshal(body, &doc); err != nil {
				t.Errorf("complete body: %v (%s)", err, body)
			}
			if len(doc.Parts) != 2 || doc.Parts[0].PartNumber != 1 || doc.Parts[1].PartNumber != 2 {
				t.Errorf("complete parts = %+v", doc.Parts)
			}
			if doc.Parts[0].ETag != `"etag-1"` {
				t.Errorf("part 1 etag = %q", doc.Parts[0].ETag)
			}
			if r.Header.Get("x-oss-callback") == "" {
				t.Error("complete must carry the x-oss-callback header")
			}
			writeEnvelope(w, map[string]any{"fid": "uploaded1"})
		}
	})
	mux.HandleFunc(pFinish, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		writeEnvelope(w, map[string]any{"finish": true, "fid": "uploaded1"})
	})
	mux.HandleFunc(pInfo, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		writeEnvelope(w, map[string]any{
			"fid": "uploaded1", "file_name": "big.bin", "pdir_fid": "root",
			"file": true, "size": 12, "updated_at": int64(1700000123000), "md5": "themd5",
		})
	})
	srv = httptest.NewServer(mux)
	return srv
}

func TestChunkedUpload(t *testing.T) {
	rec := &capture{}
	srv := uploadServer(t, rec)
	defer srv.Close()
	q := newTestQuark(t, srv)
	ctx := context.Background()

	s, err := q.BeginUpload(ctx, "root", "big.bin", 12, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.RapidDone {
		t.Fatal("no hashes were supplied, so no rapid upload is possible")
	}

	var parts []provider.PartToken
	for i, chunk := range []string{"hello ", "world!"} {
		pt, err := q.UploadPart(ctx, s, i, strings.NewReader(chunk), int64(len(chunk)))
		if err != nil {
			t.Fatal(err)
		}
		if pt.Index != i {
			t.Fatalf("token index = %d, want %d", pt.Index, i)
		}
		parts = append(parts, pt)
	}

	puts := rec.byPath("/oss/obj1")
	if len(puts) != 2 {
		t.Fatalf("expected 2 part PUTs, got %d", len(puts))
	}
	if puts[0].Query.Get("partNumber") != "1" || puts[0].Query.Get("uploadId") != "UP1" {
		t.Fatalf("part 1 query = %v", puts[0].Query)
	}
	if puts[1].Query.Get("partNumber") != "2" {
		t.Fatalf("part 2 partNumber = %q", puts[1].Query.Get("partNumber"))
	}
	if string(puts[0].Body) != "hello " || string(puts[1].Body) != "world!" {
		t.Fatalf("part bodies = %q / %q", puts[0].Body, puts[1].Body)
	}
	if puts[0].Header.Get("x-oss-date") == "" || puts[0].Header.Get("x-oss-user-agent") != ossUserAgent {
		t.Fatalf("part headers = %v", puts[0].Header)
	}

	// The signed canonical string must name this part's resource.
	var authReq struct {
		AuthMeta string `json:"auth_meta"`
	}
	decodeBody(t, rec.byPath(pAuth)[0], &authReq)
	if !strings.HasPrefix(authReq.AuthMeta, "PUT\n") ||
		!strings.Contains(authReq.AuthMeta, "/quark-bucket/oss/obj1?partNumber=1&uploadId=UP1") {
		t.Fatalf("auth_meta for part 1 = %q", authReq.AuthMeta)
	}

	e, err := q.CompleteUpload(ctx, s, parts)
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "uploaded1" || e.Name != "big.bin" || e.Size != 12 || e.Hashes[provider.HashMD5] != "themd5" {
		t.Fatalf("entry = %+v", e)
	}
	if rec.count(pFinish) != 1 {
		t.Fatalf("finish called %d times", rec.count(pFinish))
	}
	var finishBody map[string]any
	decodeBody(t, rec.last(t, pFinish), &finishBody)
	if finishBody["obj_key"] != "oss/obj1" || finishBody["task_id"] != "T1" {
		t.Fatalf("finish body = %v", finishBody)
	}
	// The complete step must be signed as a POST over the whole upload.
	completeAuth := rec.byPath(pAuth)
	var lastAuth struct {
		AuthMeta string `json:"auth_meta"`
	}
	decodeBody(t, completeAuth[len(completeAuth)-1], &lastAuth)
	if !strings.HasPrefix(lastAuth.AuthMeta, "POST\n") || !strings.Contains(lastAuth.AuthMeta, "x-oss-callback:") {
		t.Fatalf("complete auth_meta = %q", lastAuth.AuthMeta)
	}
}

func TestUploadSessionSurvivesAJournalRoundTrip(t *testing.T) {
	rec := &capture{}
	srv := uploadServer(t, rec)
	defer srv.Close()
	q := newTestQuark(t, srv)
	ctx := context.Background()

	s, err := q.BeginUpload(ctx, "root", "big.bin", 12, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild the session the way the journal would after a restart: only the
	// id, part size and opaque map survive.
	revived := provider.UploadSession{ID: s.ID, PartSize: s.PartSize, Opaque: map[string]string{}}
	for k, v := range s.Opaque {
		revived.Opaque[k] = v
	}
	pt, err := q.UploadPart(ctx, revived, 0, strings.NewReader("hello "), 6)
	if err != nil {
		t.Fatalf("resumed part upload failed: %v", err)
	}
	if pt.ETag != `"etag-1"` {
		t.Fatalf("etag = %q", pt.ETag)
	}
}

func TestUploadPartRejectsAnIncompleteSession(t *testing.T) {
	q, err := NewWithOptions("q", Options{Cookie: "a=b"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = q.UploadPart(context.Background(), provider.UploadSession{ID: "x"}, 0, strings.NewReader("a"), 1)
	if !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound for a session with no opaque state", err)
	}
}

// ---------------------------------------------------------------- cookies

func TestRotatedCookieIsSentOnLaterRequests(t *testing.T) {
	rec := &capture{}
	mux := http.NewServeMux()
	mux.HandleFunc(pInfo, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		// Quark rotates __puus on its own schedule and invalidates the old
		// value; a client that keeps replaying the configured one is logged
		// out within hours.
		if len(rec.byPath(pInfo)) == 1 {
			http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "v2", Path: "/"})
		}
		writeEnvelope(w, map[string]any{"fid": "f1", "file_name": "a.bin", "file": true})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	q := newTestQuark(t, srv)

	if _, err := q.Stat(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := q.CookieHeader(); !strings.Contains(got, "__puus=v2") || !strings.Contains(got, "__pus=abc") {
		t.Fatalf("cookie jar = %q, want the rotated __puus alongside the original cookies", got)
	}
	if _, err := q.Stat(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	reqs := rec.byPath(pInfo)
	if got := reqs[0].Header.Get("Cookie"); !strings.Contains(got, "__puus=v1") {
		t.Fatalf("first request cookie = %q", got)
	}
	if got := reqs[1].Header.Get("Cookie"); !strings.Contains(got, "__puus=v2") {
		t.Fatalf("second request must use the rotated cookie, got %q", got)
	}
}

// ---------------------------------------------------------------- errors

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		httpStatus int
		code       int
		message    string
		html       bool
		want       error
		wantClass  retry.Class
	}{
		// UNVERIFIED: the numeric codes below are the driver's own placeholder
		// table (see codeSentinels); the message-keyword rows are the paths
		// that do not depend on unconfirmed numbers.
		{name: "risk code", httpStatus: 200, code: 43001, message: "需要验证", want: provider.ErrRiskControl, wantClass: retry.ClassRiskControl},
		{name: "risk message", httpStatus: 200, code: 99999, message: "操作过于频繁，请稍后再试", want: provider.ErrRiskControl, wantClass: retry.ClassRiskControl},
		{name: "risk captcha", httpStatus: 200, code: 99999, message: "please solve the captcha", want: provider.ErrRiskControl, wantClass: retry.ClassRiskControl},
		{name: "waf 403 html", httpStatus: 403, html: true, want: provider.ErrRiskControl, wantClass: retry.ClassRiskControl},
		{name: "auth code", httpStatus: 200, code: 31001, message: "未登录", want: provider.ErrAuth, wantClass: retry.ClassAuth},
		{name: "auth status", httpStatus: 401, code: 1, message: "unauthorized request", want: provider.ErrAuth, wantClass: retry.ClassAuth},
		{name: "not found", httpStatus: 200, code: 41013, message: "文件不存在", want: provider.ErrNotFound, wantClass: retry.ClassTerminal},
		{name: "exists", httpStatus: 200, code: 32003, message: "已存在同名文件", want: provider.ErrExists, wantClass: retry.ClassTerminal},
		{name: "rate limited", httpStatus: 200, code: 42001, message: "too many requests", want: provider.ErrRateLimited, wantClass: retry.ClassRetryable},
		{name: "unknown", httpStatus: 200, code: 88888, message: "something new", want: nil, wantClass: retry.ClassTerminal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc(pInfo, func(w http.ResponseWriter, r *http.Request) {
				if tc.html {
					w.Header().Set("Content-Type", "text/html")
					w.WriteHeader(tc.httpStatus)
					_, _ = io.WriteString(w, "<html><body>captcha challenge</body></html>")
					return
				}
				writeFailure(w, tc.httpStatus, tc.code, tc.message)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			q := newTestQuark(t, srv)

			_, err := q.Stat(context.Background(), "f1")
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("error %v does not match %v", err, tc.want)
			}
			if got := retry.Classify(err); got != tc.wantClass {
				t.Fatalf("classified %v as %v, want %v", err, got, tc.wantClass)
			}
			if !tc.html {
				var ae *APIError
				if !errors.As(err, &ae) {
					t.Fatalf("error %v is not an *APIError", err)
				}
				if ae.Code != tc.code || ae.Message != tc.message {
					t.Fatalf("APIError = %+v, want the server's own code and message", ae)
				}
				if !strings.Contains(err.Error(), tc.message) {
					t.Fatalf("error text should quote the server message: %v", err)
				}
			}
		})
	}
}

func TestRiskControlOnAnyEndpointIsCircuitBreakerMaterial(t *testing.T) {
	// The whole point of mapping risk control to its own sentinel: the upper
	// layer breaks the circuit on it instead of retrying into a ban.
	mux := http.NewServeMux()
	mux.HandleFunc(pList, func(w http.ResponseWriter, r *http.Request) {
		writeFailure(w, 200, 43002, "账号存在风控")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	q := newTestQuark(t, srv)

	_, _, err := q.List(context.Background(), "root", "")
	if !errors.Is(err, provider.ErrRiskControl) {
		t.Fatalf("error = %v, want ErrRiskControl", err)
	}
	if retry.Classify(err) != retry.ClassRiskControl {
		t.Fatalf("classified as %v", retry.Classify(err))
	}
}
