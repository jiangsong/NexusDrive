package aliyun

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// call records one request the driver made, decoded far enough to assert on.
type call struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

// server is a scriptable stand-in for openapi.alipan.com.
type server struct {
	t *testing.T

	mu      sync.Mutex
	calls   []call
	handler map[string]func(c call) (int, string)
}

func newServer(t *testing.T) (*server, *httptest.Server) {
	s := &server{t: t, handler: map[string]func(call) (int, string){}}
	hs := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(hs.Close)
	return s, hs
}

func (s *server) on(path string, fn func(c call) (int, string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handler[path] = fn
}

// json replies with a fixed 200 body.
func (s *server) json(path, body string) {
	s.on(path, func(call) (int, string) { return http.StatusOK, body })
}

func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	c := call{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Raw: raw}
	if len(raw) > 0 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(raw, &c.Body)
	}
	s.mu.Lock()
	s.calls = append(s.calls, c)
	fn := s.handler[r.URL.Path]
	s.mu.Unlock()
	if fn == nil {
		s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"code":"NotFound.Endpoint","message":"no handler"}`, http.StatusNotFound)
		return
	}
	code, body := fn(c)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	io.WriteString(w, body)
}

func (s *server) requests() []call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]call(nil), s.calls...)
}

// last returns the most recent request to path.
func (s *server) last(path string) call {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.calls) - 1; i >= 0; i-- {
		if s.calls[i].Path == path {
			return s.calls[i]
		}
	}
	s.t.Fatalf("no request to %s (saw %v)", path, s.paths())
	return call{}
}

func (s *server) paths() []string {
	var out []string
	for _, c := range s.calls {
		out = append(out, c.Path)
	}
	return out
}

func (s *server) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c.Path == path {
			n++
		}
	}
	return n
}

// testClient keeps retries fast so a 429 test does not sleep for seconds.
func testClient() *httpx.Client {
	return httpx.New(httpx.Options{
		Remote: "ali-test",
		Policy: retry.Policy{
			Backoff:     retry.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond},
			MaxAttempts: 2,
		},
	})
}

func newProvider(t *testing.T, hs *httptest.Server, opts ...func(*Options)) *Provider {
	t.Helper()
	o := Options{
		Name: "ali", Client: testClient(), BaseURL: hs.URL,
		ClientID: "cid", ClientSecret: "secret", RefreshToken: "rt-1",
		AccessToken: "at-1", DriveID: "drive-9",
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

const listPage1 = `{"items":[
 {"drive_id":"drive-9","file_id":"f1","parent_file_id":"root","name":"a.txt","type":"file","size":11,
  "content_hash":"ABCDEF0123456789ABCDEF0123456789ABCDEF01","content_hash_name":"sha1",
  "created_at":"2026-08-01T10:00:00.000Z","updated_at":"2026-08-02T10:00:00.000Z"},
 {"drive_id":"drive-9","file_id":"d1","parent_file_id":"root","name":"sub","type":"folder","size":0,
  "created_at":"2026-08-01T10:00:00.000Z","updated_at":"2026-08-03T11:22:33.000Z"}
],"next_marker":"MARK2"}`

const listPage2 = `{"items":[
 {"drive_id":"drive-9","file_id":"f2","parent_file_id":"root","name":"b.bin","type":"file","size":4194304,
  "content_hash":"1111111111111111111111111111111111111111","content_hash_name":"sha1",
  "updated_at":"2026-08-04T09:00:00.000Z"}
],"next_marker":""}`

func TestListPaginatesAndDecodesEntries(t *testing.T) {
	s, hs := newServer(t)
	s.on(pathList, func(c call) (int, string) {
		if c.Body["drive_id"] != "drive-9" {
			t.Errorf("drive_id = %v, want drive-9", c.Body["drive_id"])
		}
		if c.Body["parent_file_id"] != "root" {
			t.Errorf("parent_file_id = %v", c.Body["parent_file_id"])
		}
		if c.Header.Get("Authorization") != "Bearer at-1" {
			t.Errorf("Authorization = %q", c.Header.Get("Authorization"))
		}
		if _, ok := c.Body["marker"]; !ok {
			return http.StatusOK, listPage1
		}
		if c.Body["marker"] != "MARK2" {
			t.Errorf("marker = %v, want MARK2", c.Body["marker"])
		}
		return http.StatusOK, listPage2
	})
	p := newProvider(t, hs)

	entries, next, err := p.List(context.Background(), "root", "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "MARK2" {
		t.Fatalf("next = %q, want MARK2", next)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries", len(entries))
	}
	f := entries[0]
	if f.ID != "f1" || f.Name != "a.txt" || f.Kind != provider.KindFile || f.Size != 11 {
		t.Errorf("file entry = %+v", f)
	}
	// content_hash is normalised to lowercase and doubles as the version.
	if got := f.Hashes[provider.HashSHA1]; got != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Errorf("sha1 = %q", got)
	}
	if f.Version != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Errorf("version = %q, want the content hash", f.Version)
	}
	if want := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC); !f.ModTime.Equal(want) {
		t.Errorf("mtime = %s, want %s", f.ModTime, want)
	}
	d := entries[1]
	if d.Kind != provider.KindDir || d.Version != "2026-08-03T11:22:33.000Z" {
		t.Errorf("dir entry = %+v (folders fall back to updated_at)", d)
	}

	entries, next, err = p.List(context.Background(), "root", next)
	if err != nil {
		t.Fatal(err)
	}
	if next != "" || len(entries) != 1 || entries[0].ID != "f2" {
		t.Fatalf("second page: %v %q", entries, next)
	}
	if n := s.count(pathList); n != 2 {
		t.Errorf("list called %d times", n)
	}
}

func TestListResolvesDriveIDOnce(t *testing.T) {
	s, hs := newServer(t)
	s.json(pathDriveInfo, `{"user_id":"u","default_drive_id":"1","resource_drive_id":"2","backup_drive_id":"3"}`)
	s.on(pathList, func(c call) (int, string) {
		// The resource drive is the user's own file tree; the backup drive
		// holds phone backups and must not be picked.
		if c.Body["drive_id"] != "2" {
			t.Errorf("drive_id = %v, want the resource drive 2", c.Body["drive_id"])
		}
		return http.StatusOK, `{"items":[],"next_marker":""}`
	})
	p := newProvider(t, hs, func(o *Options) { o.DriveID = "" })

	for i := 0; i < 2; i++ {
		if _, _, err := p.List(context.Background(), "root", ""); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.count(pathDriveInfo); n != 1 {
		t.Errorf("getDriveInfo called %d times, want 1 (it must be cached)", n)
	}
}

func TestStat(t *testing.T) {
	s, hs := newServer(t)
	s.on(pathGet, func(c call) (int, string) {
		if c.Body["file_id"] != "f1" {
			t.Errorf("file_id = %v", c.Body["file_id"])
		}
		return http.StatusOK, `{"drive_id":"drive-9","file_id":"f1","parent_file_id":"root","name":"a.txt",
			"type":"file","size":11,"content_hash":"aa11","content_hash_name":"sha1",
			"updated_at":"2026-08-02T10:00:00.000Z"}`
	})
	p := newProvider(t, hs)
	e, err := p.Stat(context.Background(), "f1")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "f1" || e.Size != 11 || e.Hashes[provider.HashSHA1] != "aa11" {
		t.Fatalf("entry = %+v", e)
	}
	_ = s
}

func TestStatNotFoundMapsToSentinel(t *testing.T) {
	s, hs := newServer(t)
	s.on(pathGet, func(call) (int, string) {
		return http.StatusNotFound, `{"code":"NotFound.FileId","message":"file not found","requestId":"r1"}`
	})
	p := newProvider(t, hs)
	_, err := p.Stat(context.Background(), "missing")
	if !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// cdn serves a fixed blob and records the Range header it received.
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
		rng := r.Header.Get("Range")
		var start, end int64
		if n, _ := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); n == 2 {
			if end >= int64(len(c.data)) {
				end = int64(len(c.data)) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(c.data)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(c.data[start : end+1])
			return
		}
		w.Write(c.data)
	}))
	t.Cleanup(hs.Close)
	return c, hs
}

func (c *cdn) seenRanges() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ranges...)
}

func TestReadRangeSendsRangeHeaderAndCachesLink(t *testing.T) {
	blob := []byte("0123456789abcdefghij")
	cd, cdnSrv := newCDN(t, blob)
	s, hs := newServer(t)
	s.on(pathDownloadURL, func(c call) (int, string) {
		if c.Body["file_id"] != "f1" {
			t.Errorf("file_id = %v", c.Body["file_id"])
		}
		if c.Body["expire_sec"] != float64(900) {
			t.Errorf("expire_sec = %v, want 900", c.Body["expire_sec"])
		}
		return http.StatusOK, fmt.Sprintf(`{"url":%q,"expiration":"2026-09-02T12:15:00.000Z","method":"GET"}`, cdnSrv.URL+"/blob")
	})
	p := newProvider(t, hs)

	rc, err := p.ReadRange(context.Background(), "f1", "", 4, 6)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "456789" {
		t.Fatalf("read %q, want 456789", got)
	}
	if r := cd.seenRanges(); len(r) != 1 || r[0] != "bytes=4-9" {
		t.Fatalf("Range headers = %v, want [bytes=4-9]", r)
	}

	// A second read of the same file must reuse the cached link.
	rc, err = p.ReadRange(context.Background(), "f1", "", 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if string(got) != "0123" {
		t.Fatalf("read %q", got)
	}
	if n := s.count(pathDownloadURL); n != 1 {
		t.Errorf("getDownloadUrl called %d times, want 1 (the link must be cached)", n)
	}
	if r := cd.seenRanges(); len(r) != 2 || r[1] != "bytes=0-3" {
		t.Fatalf("Range headers = %v", r)
	}
}

func TestReadRangeRefreshesExpiredLink(t *testing.T) {
	blob := []byte("hello world")
	cd, cdnSrv := newCDN(t, blob)
	cd.status = http.StatusForbidden
	s, hs := newServer(t)
	s.on(pathDownloadURL, func(call) (int, string) {
		// The second link points at a working path; flip the CDN to healthy so
		// the retry succeeds.
		if s.count(pathDownloadURL) == 2 {
			cd.mu.Lock()
			cd.status = 0
			cd.mu.Unlock()
		}
		return http.StatusOK, fmt.Sprintf(`{"url":%q,"expiration":"2026-09-02T12:15:00.000Z"}`, cdnSrv.URL+"/blob")
	})
	p := newProvider(t, hs)

	rc, err := p.ReadRange(context.Background(), "f1", "", 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "hello" {
		t.Fatalf("read %q", got)
	}
	if n := s.count(pathDownloadURL); n != 2 {
		t.Errorf("getDownloadUrl called %d times, want 2 (403 must refresh the link)", n)
	}
}

func TestDownloadURLExposesExpiry(t *testing.T) {
	s, hs := newServer(t)
	s.json(pathDownloadURL, `{"url":"https://cdn.example/x","expiration":"2026-09-02T12:15:00.000Z","method":"GET"}`)
	p := newProvider(t, hs)
	link, err := p.DownloadURL(context.Background(), "f1")
	if err != nil {
		t.Fatal(err)
	}
	if link.URL != "https://cdn.example/x" {
		t.Errorf("url = %q", link.URL)
	}
	want := time.Date(2026, 9, 2, 12, 15, 0, 0, time.UTC)
	if !link.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %s, want %s", link.ExpiresAt, want)
	}
	_ = s
}

func TestMkdirAndExists(t *testing.T) {
	s, hs := newServer(t)
	s.on(pathCreate, func(c call) (int, string) {
		if c.Body["type"] != "folder" {
			t.Errorf("type = %v, want folder", c.Body["type"])
		}
		if c.Body["check_name_mode"] != "refuse" {
			t.Errorf("check_name_mode = %v, want refuse", c.Body["check_name_mode"])
		}
		if c.Body["name"] == "dup" {
			return http.StatusOK, `{"drive_id":"drive-9","file_id":"d9","file_name":"dup","exist":true}`
		}
		return http.StatusOK, `{"drive_id":"drive-9","file_id":"d2","file_name":"new","exist":false}`
	})
	p := newProvider(t, hs)

	e, err := p.Mkdir(context.Background(), "root", "new")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "d2" || e.Kind != provider.KindDir || e.Name != "new" {
		t.Fatalf("entry = %+v", e)
	}
	if _, err := p.Mkdir(context.Background(), "root", "dup"); !errors.Is(err, provider.ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	_ = s
}

func TestRename(t *testing.T) {
	s, hs := newServer(t)
	s.on(pathUpdate, func(c call) (int, string) {
		if c.Body["file_id"] != "f1" || c.Body["name"] != "b.txt" {
			t.Errorf("body = %v", c.Body)
		}
		return http.StatusOK, `{"file_id":"f1","parent_file_id":"root","name":"b.txt","type":"file","size":11,
			"updated_at":"2026-08-05T00:00:00.000Z"}`
	})
	p := newProvider(t, hs)
	e, err := p.Rename(context.Background(), "f1", "b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "b.txt" {
		t.Fatalf("entry = %+v", e)
	}
	_ = s
}

func TestMoveStatsTheResult(t *testing.T) {
	s, hs := newServer(t)
	s.on(pathMove, func(c call) (int, string) {
		if c.Body["to_parent_file_id"] != "d1" {
			t.Errorf("to_parent_file_id = %v", c.Body["to_parent_file_id"])
		}
		return http.StatusOK, `{"drive_id":"drive-9","file_id":"f1","async_task_id":"","exist":false}`
	})
	s.json(pathGet, `{"file_id":"f1","parent_file_id":"d1","name":"a.txt","type":"file","size":11,
		"updated_at":"2026-08-06T00:00:00.000Z"}`)
	p := newProvider(t, hs)
	e, err := p.Move(context.Background(), "f1", "d1")
	if err != nil {
		t.Fatal(err)
	}
	if e.ParentID != "d1" {
		t.Fatalf("entry = %+v", e)
	}
	_ = s
}

func TestMoveNameCollision(t *testing.T) {
	s, hs := newServer(t)
	s.json(pathMove, `{"drive_id":"drive-9","file_id":"f1","exist":true}`)
	p := newProvider(t, hs)
	if _, err := p.Move(context.Background(), "f1", "d1"); !errors.Is(err, provider.ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	_ = s
}

func TestDeleteUsesRecycleBin(t *testing.T) {
	s, hs := newServer(t)
	s.json(pathTrash, `{"drive_id":"drive-9","file_id":"f1","async_task_id":""}`)
	p := newProvider(t, hs)
	if err := p.Delete(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if c := s.last(pathTrash); c.Body["file_id"] != "f1" {
		t.Fatalf("body = %v", c.Body)
	}
}

func TestCopy(t *testing.T) {
	s, hs := newServer(t)
	s.on(pathCopy, func(c call) (int, string) {
		if c.Body["new_name"] != "copy.txt" {
			t.Errorf("new_name = %v", c.Body["new_name"])
		}
		return http.StatusOK, `{"drive_id":"drive-9","file_id":"f9"}`
	})
	s.json(pathGet, `{"file_id":"f9","parent_file_id":"d1","name":"copy.txt","type":"file","size":11}`)
	p := newProvider(t, hs)
	e, err := p.Copy(context.Background(), "f1", "d1", "copy.txt")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "f9" {
		t.Fatalf("entry = %+v", e)
	}
	_ = s
}

// ------------------------------------------------------------ upload path ---

func TestProofRangeIsTokenDerived(t *testing.T) {
	const tok = "at-1"
	sum := md5.Sum([]byte(tok))
	v, err := strconv.ParseUint(hex.EncodeToString(sum[:])[:16], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	const size = 1000
	off, n := ProofRange(tok, size)
	if want := int64(v % size); off != want {
		t.Errorf("offset = %d, want %d", off, want)
	}
	if n != 8 {
		t.Errorf("length = %d, want 8", n)
	}
	// The range is always clamped at EOF rather than reading past it.
	for _, size := range []int64{1, 3, 7, 8, 9, 64, 4096} {
		o, l := ProofRange(tok, size)
		if o < 0 || o >= size || o+l > size {
			t.Errorf("size %d: range %d+%d is out of bounds", size, o, l)
		}
	}
	if o, l := ProofRange(tok, 0); o != 0 || l != 0 {
		t.Errorf("empty file range = %d+%d, want 0+0", o, l)
	}
}

func TestBeginUploadRapidAfterPreHashMatch(t *testing.T) {
	blob := []byte(strings.Repeat("x", 4096))
	s, hs := newServer(t)
	var round int
	s.on(pathCreate, func(c call) (int, string) {
		round++
		switch round {
		case 1:
			if c.Body["pre_hash"] != "PRE1" {
				t.Errorf("first create pre_hash = %v", c.Body["pre_hash"])
			}
			if _, ok := c.Body["content_hash"]; ok {
				t.Error("the pre_hash probe must not carry content_hash")
			}
			return http.StatusConflict, `{"code":"PreHashMatched","message":"pre hash matched"}`
		case 2:
			if c.Body["content_hash"] != "SHA1SUM" || c.Body["content_hash_name"] != "sha1" {
				t.Errorf("second create body = %v", c.Body)
			}
			if c.Body["proof_version"] != "v1" {
				t.Errorf("proof_version = %v", c.Body["proof_version"])
			}
			off, n := ProofRange("at-1", int64(len(blob)))
			want := base64.StdEncoding.EncodeToString(blob[off : off+n])
			if c.Body["proof_code"] != want {
				t.Errorf("proof_code = %v, want %q", c.Body["proof_code"], want)
			}
			return http.StatusOK, `{"drive_id":"drive-9","file_id":"f-rapid","file_name":"a.txt","rapid_upload":true}`
		}
		t.Fatalf("unexpected third create: %v", c.Body)
		return 0, ""
	})
	p := newProvider(t, hs, func(o *Options) {
		o.ProofBytes = func(_ context.Context, parentID, name string, off, n int64) ([]byte, error) {
			if parentID != "root" || name != "a.txt" {
				t.Errorf("proof asked for %s/%s", parentID, name)
			}
			return blob[off : off+n], nil
		}
	})

	sess, err := p.BeginUpload(context.Background(), "root", "a.txt", int64(len(blob)),
		provider.Hashes{provider.HashPreSHA1: "pre1", provider.HashSHA1: "sha1sum"})
	if err != nil {
		t.Fatal(err)
	}
	if !sess.RapidDone || sess.Entry == nil {
		t.Fatalf("session = %+v, want a finished rapid upload", sess)
	}
	if sess.Entry.ID != "f-rapid" || sess.Entry.Size != int64(len(blob)) {
		t.Fatalf("entry = %+v", sess.Entry)
	}
	if got := sess.Entry.Hashes[provider.HashSHA1]; got != "sha1sum" {
		t.Errorf("entry sha1 = %q", got)
	}
	if round != 2 {
		t.Errorf("create called %d times, want 2", round)
	}
	_ = s
}

func TestBeginUploadPreHashMissReusesTheSession(t *testing.T) {
	s, hs := newServer(t)
	s.on(pathCreate, func(c call) (int, string) {
		if c.Body["pre_hash"] != "PRE1" {
			t.Errorf("pre_hash = %v", c.Body["pre_hash"])
		}
		return http.StatusOK, `{"drive_id":"drive-9","file_id":"f-new","upload_id":"up-1","rapid_upload":false,
			"part_info_list":[{"part_number":1,"upload_url":"https://oss.example/p1"}]}`
	})
	p := newProvider(t, hs)
	sess, err := p.BeginUpload(context.Background(), "root", "a.txt", 4096,
		provider.Hashes{provider.HashPreSHA1: "pre1", provider.HashSHA1: "sha1sum"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.RapidDone {
		t.Fatal("a pre_hash miss must not report a rapid upload")
	}
	if sess.ID != "up-1" || sess.Opaque["file_id"] != "f-new" {
		t.Fatalf("session = %+v", sess)
	}
	// One round trip only: the miss already produced a usable session.
	if n := s.count(pathCreate); n != 1 {
		t.Errorf("create called %d times, want 1", n)
	}
}

func TestChunkedUploadRoundTrip(t *testing.T) {
	var parts sync.Map
	oss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("part upload used %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "" {
			t.Errorf("part upload sent Content-Type %q; OSS rejects a signed PUT that carries one", ct)
		}
		body, _ := io.ReadAll(r.Body)
		parts.Store(r.URL.Path, string(body))
		w.Header().Set("ETag", `"etag-`+strings.TrimPrefix(r.URL.Path, "/")+`"`)
	}))
	defer oss.Close()

	s, hs := newServer(t)
	s.on(pathCreate, func(c call) (int, string) {
		if c.Body["size"] != float64(10) {
			t.Errorf("size = %v", c.Body["size"])
		}
		list, _ := c.Body["part_info_list"].([]any)
		if len(list) != 1 {
			t.Errorf("part_info_list = %v, want one part for a 10 byte file", list)
		}
		return http.StatusOK, fmt.Sprintf(`{"drive_id":"drive-9","file_id":"f-new","upload_id":"up-1",
			"part_info_list":[{"part_number":1,"upload_url":%q}]}`, oss.URL+"/p1")
	})
	s.on(pathComplete, func(c call) (int, string) {
		if c.Body["upload_id"] != "up-1" || c.Body["file_id"] != "f-new" {
			t.Errorf("complete body = %v", c.Body)
		}
		return http.StatusOK, `{"drive_id":"drive-9","file_id":"f-new","parent_file_id":"root","name":"a.txt",
			"type":"file","size":10,"content_hash":"CAFE","content_hash_name":"sha1",
			"updated_at":"2026-09-02T12:00:00.000Z"}`
	})
	p := newProvider(t, hs)

	sess, err := p.BeginUpload(context.Background(), "root", "a.txt", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.PartSize != PartSize {
		t.Errorf("PartSize = %d", sess.PartSize)
	}
	tok, err := p.UploadPart(context.Background(), sess, 0, strings.NewReader("0123456789"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Index != 0 || tok.ETag != "etag-p1" {
		t.Fatalf("part token = %+v", tok)
	}
	if v, _ := parts.Load("/p1"); v != "0123456789" {
		t.Fatalf("uploaded body = %v", v)
	}
	e, err := p.CompleteUpload(context.Background(), sess, []provider.PartToken{tok})
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "f-new" || e.Size != 10 || e.Hashes[provider.HashSHA1] != "cafe" {
		t.Fatalf("entry = %+v", e)
	}
}

func TestUploadPartFetchesURLForResumedSession(t *testing.T) {
	oss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", "etag-2")
	}))
	defer oss.Close()

	s, hs := newServer(t)
	s.on(pathUploadURL, func(c call) (int, string) {
		if c.Body["upload_id"] != "up-1" || c.Body["file_id"] != "f-new" {
			t.Errorf("getUploadUrl body = %v", c.Body)
		}
		list, _ := c.Body["part_info_list"].([]any)
		if len(list) != 1 {
			t.Fatalf("part_info_list = %v", list)
		}
		if pn := list[0].(map[string]any)["part_number"]; pn != float64(2) {
			t.Errorf("part_number = %v, want 2 (index 1 is 1-based part 2)", pn)
		}
		return http.StatusOK, fmt.Sprintf(`{"part_info_list":[{"part_number":2,"upload_url":%q}]}`, oss.URL+"/p2")
	})
	p := newProvider(t, hs)

	// A session restored from the journal carries only Opaque, never the URLs.
	sess := provider.UploadSession{
		ID: "up-1", PartSize: PartSize,
		Opaque: map[string]string{"drive_id": "drive-9", "file_id": "f-new", "upload_id": "up-1"},
	}
	tok, err := p.UploadPart(context.Background(), sess, 1, strings.NewReader("data"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if tok.ETag != "etag-2" {
		t.Fatalf("token = %+v", tok)
	}
	if n := s.count(pathUploadURL); n != 1 {
		t.Errorf("getUploadUrl called %d times", n)
	}
}

func TestBeginUploadFallsBackWhenRapidIsRejected(t *testing.T) {
	s, hs := newServer(t)
	var round int
	s.on(pathCreate, func(c call) (int, string) {
		round++
		if round == 1 {
			// The content-hash round is refused with a terminal 400.
			return http.StatusBadRequest, `{"code":"InvalidParameter.ContentHash","message":"bad hash"}`
		}
		if _, ok := c.Body["content_hash"]; ok {
			t.Error("the fallback create must not repeat the rejected hash")
		}
		return http.StatusOK, `{"drive_id":"drive-9","file_id":"f-new","upload_id":"up-2",
			"part_info_list":[{"part_number":1,"upload_url":"https://oss.example/p1"}]}`
	})
	p := newProvider(t, hs, func(o *Options) {
		o.ProofBytes = func(context.Context, string, string, int64, int64) ([]byte, error) {
			return []byte("12345678"), nil
		}
	})
	sess, err := p.BeginUpload(context.Background(), "root", "a.txt", 512, provider.Hashes{provider.HashSHA1: "sha1sum"})
	if err != nil {
		t.Fatalf("a rejected rapid upload must degrade to a chunked one, got %v", err)
	}
	if sess.ID != "up-2" || sess.RapidDone {
		t.Fatalf("session = %+v", sess)
	}
	if round != 2 {
		t.Errorf("create called %d times, want 2", round)
	}
	_ = s
}

// ---------------------------------------------------------- error mapping ---

func TestRefreshesTokenOn401AndRetries(t *testing.T) {
	s, hs := newServer(t)
	s.on(pathToken, func(c call) (int, string) {
		if c.Body["grant_type"] != "refresh_token" || c.Body["refresh_token"] != "rt-1" {
			t.Errorf("token body = %v", c.Body)
		}
		if c.Body["client_id"] != "cid" {
			t.Errorf("client_id = %v", c.Body["client_id"])
		}
		return http.StatusOK, `{"token_type":"Bearer","access_token":"at-2","refresh_token":"rt-2","expires_in":7200}`
	})
	s.on(pathList, func(c call) (int, string) {
		if c.Header.Get("Authorization") == "Bearer at-1" {
			return http.StatusUnauthorized, `{"code":"AccessTokenInvalid","message":"token expired"}`
		}
		if c.Header.Get("Authorization") != "Bearer at-2" {
			t.Errorf("retry used %q", c.Header.Get("Authorization"))
		}
		return http.StatusOK, `{"items":[],"next_marker":""}`
	})
	p := newProvider(t, hs)

	if _, _, err := p.List(context.Background(), "root", ""); err != nil {
		t.Fatal(err)
	}
	if n := s.count(pathList); n != 2 {
		t.Errorf("list called %d times, want 2 (one 401 plus the retry)", n)
	}
	// The rotated refresh token has to be kept: Aliyun invalidates the old one.
	if got := p.RefreshToken(); got != "rt-2" {
		t.Errorf("refresh token = %q, want the rotated rt-2", got)
	}
}

func TestRefreshFailureIsAuthError(t *testing.T) {
	s, hs := newServer(t)
	s.on(pathToken, func(call) (int, string) {
		return http.StatusBadRequest, `{"code":"InvalidParameter.RefreshToken","message":"refresh token expired"}`
	})
	s.on(pathList, func(call) (int, string) {
		return http.StatusUnauthorized, `{"code":"AccessTokenInvalid","message":"token expired"}`
	})
	p := newProvider(t, hs)
	_, _, err := p.List(context.Background(), "root", "")
	if !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	_ = s
}

func TestErrorCodeMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"not found", http.StatusNotFound, `{"code":"NotFound.FileId","message":"file not found"}`, provider.ErrNotFound},
		{"not found 400", http.StatusBadRequest, `{"code":"NotFound.File","message":"file not found"}`, provider.ErrNotFound},
		{"dead upload", http.StatusBadRequest, `{"code":"NotFound.UploadId","message":"upload gone"}`, provider.ErrNotFound},
		{"throttled", http.StatusTooManyRequests, `{"code":"TooManyRequests","message":"slow down"}`, provider.ErrRiskControl},
		{"qps", http.StatusTooManyRequests, `{"code":"QpsLimitExceed.Api","message":"qps"}`, provider.ErrRateLimited},
		{"recycled", http.StatusForbidden, `{"code":"ForbiddenFileInTheRecycleBin","message":"in bin"}`, provider.ErrNotFound},
		{"denied", http.StatusForbidden, `{"code":"PermissionDenied","message":"no scope"}`, provider.ErrAuth},
		// No envelope: the transport's status mapping still applies.
		{"bare 500", http.StatusInternalServerError, `oops`, provider.ErrTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, hs := newServer(t)
			s.on(pathGet, func(call) (int, string) { return tc.status, tc.body })
			// A 401 would trigger a refresh; these cases never do.
			s.on(pathToken, func(call) (int, string) {
				return http.StatusOK, `{"access_token":"at-2","expires_in":7200}`
			})
			p := newProvider(t, hs)
			_, err := p.Stat(context.Background(), "f1")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			var ae *APIError
			if tc.body != "oops" && errors.As(err, &ae) && ae.Status != tc.status {
				t.Errorf("APIError.Status = %d, want %d", ae.Status, tc.status)
			}
		})
	}
}

func TestCapabilities(t *testing.T) {
	_, hs := newServer(t)
	p := newProvider(t, hs)
	c := p.Capabilities()
	if c.Delta {
		t.Error("Aliyun publishes no delta feed; Caps.Delta must be false")
	}
	if _, ok := any(p).(provider.ChangeLister); ok {
		t.Error("the driver must not implement ChangeLister")
	}
	if c.PartSize != 4<<20 || c.MaxParts != 10000 {
		t.Errorf("part config = %d/%d", c.PartSize, c.MaxParts)
	}
	if c.LinkTTL != 15*time.Minute || !c.LinkShareable {
		t.Errorf("link config = %v/%v", c.LinkTTL, c.LinkShareable)
	}
	if !c.RangeRead || !c.ServerMove || !c.ServerRename || !c.ServerCopy {
		t.Errorf("caps = %+v", c)
	}
	if c.QPS != (provider.QPS{Meta: 4, Download: 4, Upload: 2}) {
		t.Errorf("qps = %+v", c.QPS)
	}
	if c.Tier != provider.TierOfficial {
		t.Errorf("tier = %v", c.Tier)
	}
	if len(c.HashTypes) != 1 || c.HashTypes[0] != provider.HashSHA1 {
		t.Errorf("hash types = %v", c.HashTypes)
	}
	if len(c.RapidUpload) != 2 {
		t.Errorf("rapid upload hashes = %v", c.RapidUpload)
	}
}

func TestFactoryRequiresRefreshToken(t *testing.T) {
	if _, err := Factory("ali", map[string]any{"client_id": "x"}); err == nil ||
		!strings.Contains(err.Error(), "refresh_token") {
		t.Fatalf("err = %v, want it to name the missing refresh_token key", err)
	}
	p, err := Factory("ali", map[string]any{"refresh_token": "rt", "client_id": "id", "client_secret": "s"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "ali" {
		t.Errorf("name = %q", p.Name())
	}
}

func TestRegistered(t *testing.T) {
	p, err := provider.New("aliyun", "ali", map[string]any{"refresh_token": "rt"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*Provider); !ok {
		t.Fatalf("registry returned %T", p)
	}
}
