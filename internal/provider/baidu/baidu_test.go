package baidu

import (
	"context"
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

// call is one request the driver made, decoded for assertions.
type call struct {
	Method    string
	Path      string
	APIMethod string // the xpan "method" query parameter
	Opera     string
	Query     url.Values
	Form      url.Values
	Header    http.Header
	// File is the multipart payload of a superfile2 upload.
	File []byte
}

// xpan is a scriptable stand-in for pan.baidu.com.
type xpan struct {
	t *testing.T

	mu       sync.Mutex
	calls    []call
	handlers map[string]func(c call) (int, string)
}

// key identifies a handler: the path plus the method/opera query parameters
// that xpan multiplexes several operations onto.
func key(path, method, opera string) string {
	k := path + "?" + method
	if opera != "" {
		k += ":" + opera
	}
	return k
}

func newXpan(t *testing.T) (*xpan, *httptest.Server) {
	x := &xpan{t: t, handlers: map[string]func(call) (int, string){}}
	hs := httptest.NewServer(http.HandlerFunc(x.serve))
	t.Cleanup(hs.Close)
	return x, hs
}

func (x *xpan) on(path, method, opera string, fn func(c call) (int, string)) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.handlers[key(path, method, opera)] = fn
}

func (x *xpan) reply(path, method, opera, body string) {
	x.on(path, method, opera, func(call) (int, string) { return http.StatusOK, body })
}

func (x *xpan) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c := call{
		Method: r.Method, Path: r.URL.Path, APIMethod: q.Get("method"), Opera: q.Get("opera"),
		Query: q, Header: r.Header.Clone(), Form: url.Values{},
	}
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			x.t.Errorf("parse multipart: %v", err)
		} else {
			f, _, err := r.FormFile("file")
			if err != nil {
				x.t.Errorf("superfile2 must send the block in a form field named \"file\": %v", err)
			} else {
				c.File, _ = io.ReadAll(f)
				f.Close()
			}
		}
	case r.Method == http.MethodPost:
		if err := r.ParseForm(); err == nil {
			c.Form = r.PostForm
		}
	}
	x.mu.Lock()
	x.calls = append(x.calls, c)
	fn := x.handlers[key(c.Path, c.APIMethod, c.Opera)]
	if fn == nil {
		fn = x.handlers[key(c.Path, c.APIMethod, "")]
	}
	x.mu.Unlock()
	if fn == nil {
		x.t.Errorf("unexpected request %s %s (method=%s opera=%s)", r.Method, r.URL.Path, c.APIMethod, c.Opera)
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"errno":-9}`)
		return
	}
	code, body := fn(c)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	io.WriteString(w, body)
}

func (x *xpan) all() []call {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]call(nil), x.calls...)
}

func (x *xpan) last(method string) call {
	x.mu.Lock()
	defer x.mu.Unlock()
	for i := len(x.calls) - 1; i >= 0; i-- {
		if x.calls[i].APIMethod == method {
			return x.calls[i]
		}
	}
	x.t.Fatalf("no request with method=%s", method)
	return call{}
}

func (x *xpan) count(method string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	n := 0
	for _, c := range x.calls {
		if c.APIMethod == method {
			n++
		}
	}
	return n
}

func testClient() *httpx.Client {
	return httpx.New(httpx.Options{
		Remote:    "baidu-test",
		UserAgent: DownloadUserAgent,
		Policy: retry.Policy{
			Backoff:     retry.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond},
			MaxAttempts: 2,
		},
	})
}

func newProvider(t *testing.T, hs *httptest.Server, opts ...func(*Options)) *Provider {
	t.Helper()
	o := Options{
		Name: "bd", Client: testClient(),
		BaseURL: hs.URL, UploadURL: hs.URL, OAuthURL: hs.URL,
		AccessToken: "tok-1", PageSize: 2,
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

const listPage1 = `{"errno":0,"guid":0,"list":[
 {"fs_id":111,"path":"/apps/cloudfs/a.txt","server_filename":"a.txt","isdir":0,"size":11,
  "server_mtime":1756800000,"md5":"098f6bcd4621d373cade4e832627b4f6","category":4},
 {"fs_id":222,"path":"/apps/cloudfs/sub","server_filename":"sub","isdir":1,"size":0,
  "server_mtime":1756800001,"category":6}
],"request_id":1}`

const listPage2 = `{"errno":0,"list":[
 {"fs_id":333,"path":"/apps/cloudfs/b.bin","server_filename":"b.bin","isdir":0,"size":4194304,
  "server_mtime":1756800002,"md5":"5d41402abc4b2a76b9719d911017c592"}
],"request_id":2}`

func TestListPaginatesByOffset(t *testing.T) {
	x, hs := newXpan(t)
	x.on(pathFile, "list", "", func(c call) (int, string) {
		if c.Query.Get("access_token") != "tok-1" {
			t.Errorf("access_token = %q; Baidu authenticates by query parameter", c.Query.Get("access_token"))
		}
		if c.Query.Get("dir") != "/apps/cloudfs" {
			t.Errorf("dir = %q", c.Query.Get("dir"))
		}
		if c.Query.Get("limit") != "2" {
			t.Errorf("limit = %q", c.Query.Get("limit"))
		}
		switch c.Query.Get("start") {
		case "0":
			return http.StatusOK, listPage1
		case "2":
			return http.StatusOK, listPage2
		}
		t.Errorf("unexpected start = %q", c.Query.Get("start"))
		return http.StatusOK, `{"errno":0,"list":[]}`
	})
	p := newProvider(t, hs)

	entries, next, err := p.List(context.Background(), "/apps/cloudfs", "")
	if err != nil {
		t.Fatal(err)
	}
	// A full page means there may be more: the API reports no has_more.
	if next != "2" {
		t.Fatalf("next = %q, want the offset 2", next)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %v", entries)
	}
	f := entries[0]
	if f.ID != "111:/apps/cloudfs/a.txt" {
		t.Errorf("id = %q, want the fs_id:path composite", f.ID)
	}
	if f.Name != "a.txt" || f.Size != 11 || f.Kind != provider.KindFile {
		t.Errorf("entry = %+v", f)
	}
	if f.Hashes[provider.HashMD5] != "098f6bcd4621d373cade4e832627b4f6" || f.Version != f.Hashes[provider.HashMD5] {
		t.Errorf("hash/version = %+v", f)
	}
	if !f.ModTime.Equal(time.Unix(1756800000, 0).UTC()) {
		t.Errorf("mtime = %s", f.ModTime)
	}
	if entries[1].Kind != provider.KindDir || entries[1].Hashes != nil {
		t.Errorf("dir entry = %+v", entries[1])
	}

	entries, next, err = p.List(context.Background(), "/apps/cloudfs", next)
	if err != nil {
		t.Fatal(err)
	}
	if next != "" {
		t.Errorf("a short page ends the listing, got next %q", next)
	}
	if len(entries) != 1 || entries[0].ID != "333:/apps/cloudfs/b.bin" {
		t.Fatalf("entries = %v", entries)
	}
	if n := x.count("list"); n != 2 {
		t.Errorf("list called %d times", n)
	}
}

func TestStatByFSIDUsesFilemetas(t *testing.T) {
	x, hs := newXpan(t)
	x.on(pathMultimedia, "filemetas", "", func(c call) (int, string) {
		if c.Query.Get("fsids") != "[111]" {
			t.Errorf("fsids = %q, want a JSON array literal", c.Query.Get("fsids"))
		}
		return http.StatusOK, `{"errno":0,"list":[{"fs_id":111,"path":"/apps/cloudfs/a.txt","filename":"a.txt",
			"isdir":0,"size":11,"server_mtime":1756800000,"md5":"098f6bcd4621d373cade4e832627b4f6"}]}`
	})
	p := newProvider(t, hs)
	e, err := p.Stat(context.Background(), "111:/apps/cloudfs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	// filemetas spells the name "filename" where list says "server_filename".
	if e.Name != "a.txt" || e.Size != 11 {
		t.Fatalf("entry = %+v", e)
	}
	_ = x
}

func TestStatByPathListsTheParent(t *testing.T) {
	x, hs := newXpan(t)
	x.on(pathFile, "list", "", func(c call) (int, string) {
		if c.Query.Get("dir") != "/apps/cloudfs" {
			t.Errorf("dir = %q, want the parent directory", c.Query.Get("dir"))
		}
		if c.Query.Get("start") == "0" {
			return http.StatusOK, listPage1
		}
		return http.StatusOK, listPage2
	})
	p := newProvider(t, hs)
	e, err := p.Stat(context.Background(), "/apps/cloudfs/b.bin")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "333:/apps/cloudfs/b.bin" {
		t.Fatalf("entry = %+v", e)
	}
	// It had to walk both pages to find it.
	if n := x.count("list"); n != 2 {
		t.Errorf("list called %d times, want 2", n)
	}
}

func TestStatMissingPathIsNotFound(t *testing.T) {
	x, hs := newXpan(t)
	x.reply(pathFile, "list", "", `{"errno":0,"list":[]}`)
	p := newProvider(t, hs)
	if _, err := p.Stat(context.Background(), "/apps/cloudfs/nope"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	_ = x
}

// cdn stands in for the d.pcs.baidu.com dlink host.
type cdn struct {
	mu     sync.Mutex
	data   []byte
	ranges []string
	uas    []string
	querys []url.Values
}

func newCDN(t *testing.T, data []byte) (*cdn, *httptest.Server) {
	c := &cdn{data: data}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.ranges = append(c.ranges, r.Header.Get("Range"))
		c.uas = append(c.uas, r.Header.Get("User-Agent"))
		c.querys = append(c.querys, r.URL.Query())
		c.mu.Unlock()
		if r.Header.Get("User-Agent") != DownloadUserAgent {
			// Mirrors errno 31326: the anti-leech check refuses the download.
			http.Error(w, "referer check failed", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/expired" {
			// Mirrors errno 31360: the signed link is past its 8 h window.
			http.Error(w, "link expired", http.StatusForbidden)
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

func TestDownloadURLCarriesTokenAndUserAgent(t *testing.T) {
	x, hs := newXpan(t)
	x.on(pathMultimedia, "filemetas", "", func(c call) (int, string) {
		if c.Query.Get("dlink") != "1" {
			t.Errorf("dlink = %q, want 1", c.Query.Get("dlink"))
		}
		return http.StatusOK, `{"errno":0,"list":[{"fs_id":111,"path":"/apps/cloudfs/a.txt",
			"dlink":"https://d.pcs.baidu.com/file/abc?fid=1&sign=x"}]}`
	})
	p := newProvider(t, hs)
	link, err := p.DownloadURL(context.Background(), "111:/apps/cloudfs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	// The CDN authenticates the query string, not a header.
	if !strings.Contains(link.URL, "&access_token=tok-1") {
		t.Errorf("url = %q, want the access token appended", link.URL)
	}
	if link.Headers["User-Agent"] != DownloadUserAgent {
		t.Errorf("headers = %v; the UA must reach whoever follows the link", link.Headers)
	}
	if want := time.Date(2026, 9, 2, 20, 0, 0, 0, time.UTC); !link.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %s, want now+8h = %s", link.ExpiresAt, want)
	}
	_ = x
}

func TestReadRangeSendsRangeAndUserAgent(t *testing.T) {
	cd, cdnSrv := newCDN(t, []byte("0123456789abcdef"))
	x, hs := newXpan(t)
	x.on(pathMultimedia, "filemetas", "", func(call) (int, string) {
		return http.StatusOK, fmt.Sprintf(`{"errno":0,"list":[{"fs_id":111,"path":"/apps/cloudfs/a.txt","dlink":%q}]}`,
			cdnSrv.URL+"/file/abc?fid=1")
	})
	p := newProvider(t, hs)
	rc, err := p.ReadRange(context.Background(), "111:/apps/cloudfs/a.txt", "", 6, 4)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "6789" {
		t.Fatalf("read %q, want 6789", got)
	}
	cd.mu.Lock()
	defer cd.mu.Unlock()
	if len(cd.ranges) != 1 || cd.ranges[0] != "bytes=6-9" {
		t.Errorf("Range headers = %v", cd.ranges)
	}
	if cd.uas[0] != DownloadUserAgent {
		t.Errorf("User-Agent = %q, want %q (Baidu refuses large downloads without it)", cd.uas[0], DownloadUserAgent)
	}
	if cd.querys[0].Get("access_token") != "tok-1" {
		t.Errorf("dlink query = %v, want the access token", cd.querys[0])
	}
	cd.mu.Unlock()

	// A second range of the same file must not spend another filemetas call:
	// the meta bucket only allows about two requests a second.
	rc, err = p.ReadRange(context.Background(), "111:/apps/cloudfs/a.txt", "", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(rc)
	rc.Close()
	if n := x.count("filemetas"); n != 1 {
		t.Errorf("filemetas called %d times, want 1 (the dlink is cached for 8 h)", n)
	}
	cd.mu.Lock()
}

func TestReadRangeRefreshesRejectedDlink(t *testing.T) {
	cd, cdnSrv := newCDN(t, []byte("hello world"))
	x, hs := newXpan(t)
	x.on(pathMultimedia, "filemetas", "", func(call) (int, string) {
		// The first dlink is served by a path the CDN refuses, standing in for
		// errno 31360 (link expired) surfacing as a 403.
		path := "/expired"
		if x.count("filemetas") == 2 {
			path = "/file/abc"
		}
		return http.StatusOK, fmt.Sprintf(`{"errno":0,"list":[{"fs_id":111,"path":"/apps/cloudfs/a.txt","dlink":%q}]}`,
			cdnSrv.URL+path)
	})
	p := newProvider(t, hs)
	rc, err := p.ReadRange(context.Background(), "111:/apps/cloudfs/a.txt", "", 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "hello" {
		t.Fatalf("read %q", got)
	}
	if n := x.count("filemetas"); n != 2 {
		t.Errorf("filemetas called %d times, want 2 (a 403 must refresh the dlink)", n)
	}
	_ = cd
}

func TestMkdir(t *testing.T) {
	x, hs := newXpan(t)
	x.on(pathFile, "create", "", func(c call) (int, string) {
		if c.Form.Get("isdir") != "1" {
			t.Errorf("isdir = %q, want 1", c.Form.Get("isdir"))
		}
		if c.Form.Get("path") != "/apps/cloudfs/new" {
			t.Errorf("path = %q", c.Form.Get("path"))
		}
		if c.Form.Get("rtype") != "0" {
			t.Errorf("rtype = %q; mkdir must refuse an existing name rather than rename it", c.Form.Get("rtype"))
		}
		return http.StatusOK, `{"errno":0,"fs_id":444,"path":"/apps/cloudfs/new","isdir":1,
			"ctime":1756800100,"mtime":1756800100,"category":6}`
	})
	p := newProvider(t, hs)
	e, err := p.Mkdir(context.Background(), "/apps/cloudfs", "new")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "444:/apps/cloudfs/new" || e.Kind != provider.KindDir || e.Name != "new" {
		t.Fatalf("entry = %+v", e)
	}
	_ = x
}

func TestMkdirExisting(t *testing.T) {
	x, hs := newXpan(t)
	x.reply(pathFile, "create", "", `{"errno":-8}`)
	p := newProvider(t, hs)
	if _, err := p.Mkdir(context.Background(), "/apps/cloudfs", "dup"); !errors.Is(err, provider.ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	_ = x
}

func TestRenameMoveDelete(t *testing.T) {
	x, hs := newXpan(t)
	x.on(pathFile, "filemanager", "rename", func(c call) (int, string) {
		if c.Form.Get("async") != "0" {
			t.Errorf("async = %q, want 0 so the tree is consistent on return", c.Form.Get("async"))
		}
		if got, want := c.Form.Get("filelist"), `[{"newname":"b.txt","path":"/apps/cloudfs/a.txt"}]`; got != want {
			t.Errorf("filelist = %s, want %s", got, want)
		}
		if c.Form.Get("ondup") != "fail" {
			t.Errorf("ondup = %q, want fail so a collision surfaces as ErrExists", c.Form.Get("ondup"))
		}
		return http.StatusOK, `{"errno":0,"info":[{"errno":0,"path":"/apps/cloudfs/a.txt"}]}`
	})
	x.on(pathFile, "filemanager", "move", func(c call) (int, string) {
		if got, want := c.Form.Get("filelist"),
			`[{"dest":"/apps/cloudfs/sub","newname":"b.txt","path":"/apps/cloudfs/b.txt"}]`; got != want {
			t.Errorf("filelist = %s, want %s", got, want)
		}
		return http.StatusOK, `{"errno":0,"info":[{"errno":0,"path":"/apps/cloudfs/sub/b.txt"}]}`
	})
	x.on(pathFile, "filemanager", "delete", func(c call) (int, string) {
		if got, want := c.Form.Get("filelist"), `["/apps/cloudfs/sub/b.txt"]`; got != want {
			t.Errorf("delete filelist = %s, want a bare array of paths %s", got, want)
		}
		return http.StatusOK, `{"errno":0,"info":[]}`
	})
	x.on(pathMultimedia, "filemetas", "", func(c call) (int, string) {
		return http.StatusOK, `{"errno":0,"list":[{"fs_id":111,"path":"/apps/cloudfs/sub/b.txt","filename":"b.txt",
			"isdir":0,"size":11,"server_mtime":1756800000}]}`
	})
	p := newProvider(t, hs)

	if _, err := p.Rename(context.Background(), "111:/apps/cloudfs/a.txt", "b.txt"); err != nil {
		t.Fatal(err)
	}
	e, err := p.Move(context.Background(), "111:/apps/cloudfs/b.txt", "/apps/cloudfs/sub")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "111:/apps/cloudfs/sub/b.txt" {
		t.Fatalf("entry = %+v", e)
	}
	if err := p.Delete(context.Background(), e.ID); err != nil {
		t.Fatal(err)
	}
	_ = x
}

func TestFilemanagerPerEntryErrno(t *testing.T) {
	x, hs := newXpan(t)
	// The envelope succeeds while the one entry failed: both must be checked.
	x.reply(pathFile, "filemanager", "delete", `{"errno":0,"info":[{"errno":-9,"path":"/apps/cloudfs/gone"}]}`)
	p := newProvider(t, hs)
	err := p.Delete(context.Background(), "/apps/cloudfs/gone")
	if !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound from the per-entry errno", err)
	}
	_ = x
}

// ---------------------------------------------------------------- upload ---

func TestBeginUploadRapid(t *testing.T) {
	x, hs := newXpan(t)
	x.on(pathFile, "precreate", "", func(c call) (int, string) {
		if c.Form.Get("content-md5") != "098f6bcd4621d373cade4e832627b4f6" {
			t.Errorf("content-md5 = %q", c.Form.Get("content-md5"))
		}
		if c.Form.Get("slice-md5") != "5d41402abc4b2a76b9719d911017c592" {
			t.Errorf("slice-md5 = %q", c.Form.Get("slice-md5"))
		}
		if c.Form.Get("autoinit") != "1" {
			t.Errorf("autoinit = %q", c.Form.Get("autoinit"))
		}
		if c.Form.Get("path") != "/apps/cloudfs/a.txt" {
			t.Errorf("path = %q", c.Form.Get("path"))
		}
		// A single-block file sends its content MD5 as the whole block list.
		if got, want := c.Form.Get("block_list"), `["098f6bcd4621d373cade4e832627b4f6"]`; got != want {
			t.Errorf("block_list = %s, want %s", got, want)
		}
		return http.StatusOK, `{"errno":0,"return_type":2,"path":"/apps/cloudfs/a.txt",
			"info":{"fs_id":555,"path":"/apps/cloudfs/a.txt","server_filename":"a.txt","size":11,"isdir":0,
			"md5":"098f6bcd4621d373cade4e832627b4f6","mtime":1756800000,"ctime":1756800000}}`
	})
	p := newProvider(t, hs)

	sess, err := p.BeginUpload(context.Background(), "/apps/cloudfs", "a.txt", 11, provider.Hashes{
		provider.HashMD5:      "098f6bcd4621d373cade4e832627b4f6",
		provider.HashSliceMD5: "5d41402abc4b2a76b9719d911017c592",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sess.RapidDone || sess.Entry == nil {
		t.Fatalf("session = %+v, want a rapid hit", sess)
	}
	if sess.Entry.ID != "555:/apps/cloudfs/a.txt" || sess.Entry.Size != 11 {
		t.Fatalf("entry = %+v", sess.Entry)
	}
	if sess.Entry.Hashes[provider.HashMD5] != "098f6bcd4621d373cade4e832627b4f6" {
		t.Errorf("entry hashes = %v", sess.Entry.Hashes)
	}
	if n := x.count("precreate"); n != 1 {
		t.Errorf("precreate called %d times", n)
	}
}

func TestChunkedUploadRoundTrip(t *testing.T) {
	block0 := strings.Repeat("a", BlockSize)
	block1 := "tail"
	size := int64(len(block0) + len(block1))

	x, hs := newXpan(t)
	x.on(pathFile, "precreate", "", func(c call) (int, string) {
		// Two blocks: the digests are not known yet, so a placeholder list of
		// the right length goes out and create sends the real one.
		if got, want := c.Form.Get("block_list"),
			`["`+placeholderBlockMD5+`","`+placeholderBlockMD5+`"]`; got != want {
			t.Errorf("block_list = %s, want %s", got, want)
		}
		if c.Form.Get("rtype") != "3" {
			t.Errorf("rtype = %q, want 3", c.Form.Get("rtype"))
		}
		return http.StatusOK, `{"errno":0,"return_type":1,"uploadid":"up-1","block_list":[0,1],
			"path":"/apps/cloudfs/big.bin"}`
	})
	x.on(pathSuperfile2, "upload", "", func(c call) (int, string) {
		if c.Query.Get("type") != "tmpfile" {
			t.Errorf("type = %q, want tmpfile", c.Query.Get("type"))
		}
		if c.Query.Get("uploadid") != "up-1" {
			t.Errorf("uploadid = %q", c.Query.Get("uploadid"))
		}
		if c.Query.Get("path") != "/apps/cloudfs/big.bin" {
			t.Errorf("path = %q, want the precreate path", c.Query.Get("path"))
		}
		switch c.Query.Get("partseq") {
		case "0":
			if len(c.File) != BlockSize {
				t.Errorf("block 0 is %d bytes, want a full %d byte block", len(c.File), BlockSize)
			}
			return http.StatusOK, `{"md5":"AAAA0000AAAA0000AAAA0000AAAA0000","request_id":1}`
		case "1":
			if string(c.File) != block1 {
				t.Errorf("block 1 = %q", c.File)
			}
			return http.StatusOK, `{"md5":"bbbb1111bbbb1111bbbb1111bbbb1111","request_id":2}`
		}
		t.Errorf("unexpected partseq %q", c.Query.Get("partseq"))
		return http.StatusOK, `{"md5":"x"}`
	})
	x.on(pathFile, "create", "", func(c call) (int, string) {
		// The authoritative block list is the one superfile2 confirmed, in
		// slice order and lowercased.
		want := `["aaaa0000aaaa0000aaaa0000aaaa0000","bbbb1111bbbb1111bbbb1111bbbb1111"]`
		if got := c.Form.Get("block_list"); got != want {
			t.Errorf("create block_list = %s, want %s", got, want)
		}
		if c.Form.Get("uploadid") != "up-1" {
			t.Errorf("uploadid = %q", c.Form.Get("uploadid"))
		}
		if c.Form.Get("size") != fmt.Sprint(size) {
			t.Errorf("size = %q, want %d", c.Form.Get("size"), size)
		}
		return http.StatusOK, `{"errno":0,"fs_id":666,"path":"/apps/cloudfs/big.bin","server_filename":"big.bin",
			"size":4194308,"isdir":0,"md5":"77777777777777777777777777777777","ctime":1756800200,"mtime":1756800200}`
	})
	p := newProvider(t, hs)

	sess, err := p.BeginUpload(context.Background(), "/apps/cloudfs", "big.bin", size, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.RapidDone {
		t.Fatal("return_type 1 is not a rapid hit")
	}
	if sess.ID != "up-1" || sess.PartSize != BlockSize {
		t.Fatalf("session = %+v", sess)
	}
	if sess.Opaque["path"] != "/apps/cloudfs/big.bin" {
		t.Errorf("opaque = %v; the path must survive a journal round trip", sess.Opaque)
	}

	t0, err := p.UploadPart(context.Background(), sess, 0, strings.NewReader(block0), int64(len(block0)))
	if err != nil {
		t.Fatal(err)
	}
	if t0.ETag != "aaaa0000aaaa0000aaaa0000aaaa0000" {
		t.Errorf("part 0 etag = %q, want the server's block md5 lowercased", t0.ETag)
	}
	t1, err := p.UploadPart(context.Background(), sess, 1, strings.NewReader(block1), int64(len(block1)))
	if err != nil {
		t.Fatal(err)
	}

	// Parts arrive out of order; CompleteUpload has to sort them.
	e, err := p.CompleteUpload(context.Background(), sess, []provider.PartToken{t1, t0})
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "666:/apps/cloudfs/big.bin" || e.Name != "big.bin" {
		t.Fatalf("entry = %+v", e)
	}
	if e.Hashes[provider.HashMD5] != "77777777777777777777777777777777" {
		t.Errorf("entry hashes = %v", e.Hashes)
	}
}

// ---------------------------------------------------------- error mapping ---

func TestErrnoMapping(t *testing.T) {
	cases := []struct {
		errno int
		want  error
	}{
		{-9, provider.ErrNotFound},
		{-3, provider.ErrNotFound},
		{31066, provider.ErrNotFound},
		{-8, provider.ErrExists},
		{31061, provider.ErrExists},
		{31034, provider.ErrRateLimited},
		{20012, provider.ErrRateLimited},
		{9013, provider.ErrRateLimited},
		{31326, provider.ErrRiskControl},
		{31045, provider.ErrAuth},
		{31360, provider.ErrLinkExpired},
		{31190, provider.ErrNotFound},
		{31355, provider.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.errno), func(t *testing.T) {
			x, hs := newXpan(t)
			x.reply(pathFile, "list", "", fmt.Sprintf(`{"errno":%d,"errmsg":"boom"}`, tc.errno))
			p := newProvider(t, hs)
			_, _, err := p.List(context.Background(), "/apps/cloudfs", "")
			if !errors.Is(err, tc.want) {
				t.Fatalf("errno %d gave %v, want %v", tc.errno, err, tc.want)
			}
			var ae *APIError
			if !errors.As(err, &ae) || ae.Errno != tc.errno {
				t.Errorf("error does not carry the errno: %v", err)
			}
		})
	}
}

func TestTerminalErrnosAreNotRetried(t *testing.T) {
	// Baidu reports these with HTTP 200 and no sentinel fits them, so the
	// driver has to tell the classifier not to retry: a full drive or an
	// illegal path never fixes itself.
	for _, errno := range []int{-10, 31062, 31064, 31365} {
		err := &APIError{Errno: errno, Op: "create"}
		if got := retry.Classify(err); got != retry.ClassTerminal {
			t.Errorf("errno %d classified as %v, want terminal", errno, got)
		}
	}
	// A throttle still has to be retried.
	if got := retry.Classify(&APIError{Errno: 31034}); got != retry.ClassRetryable {
		t.Errorf("errno 31034 classified as %v, want retryable", got)
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
			x, hs := newXpan(t)
			x.on(pathFile, "list", "", func(call) (int, string) { return tc.status, `{"errno":0}` })
			p := newProvider(t, hs)
			_, _, err := p.List(context.Background(), "/apps/cloudfs", "")
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d gave %v, want %v", tc.status, err, tc.want)
			}
		})
	}
}

func TestRefreshesTokenOnAuthErrno(t *testing.T) {
	x, hs := newXpan(t)
	x.on(pathOAuthToken, "", "", func(c call) (int, string) {
		if c.Query.Get("grant_type") != "refresh_token" || c.Query.Get("refresh_token") != "rt-1" {
			t.Errorf("refresh query = %v", c.Query)
		}
		return http.StatusOK, `{"access_token":"tok-2","refresh_token":"rt-2","expires_in":2592000}`
	})
	x.on(pathFile, "list", "", func(c call) (int, string) {
		if c.Query.Get("access_token") == "tok-1" {
			// errno -6 is Baidu's "identity verification failed".
			return http.StatusOK, `{"errno":-6}`
		}
		if c.Query.Get("access_token") != "tok-2" {
			t.Errorf("retry used token %q", c.Query.Get("access_token"))
		}
		return http.StatusOK, `{"errno":0,"list":[]}`
	})
	p := newProvider(t, hs, func(o *Options) {
		o.RefreshToken = "rt-1"
		o.ClientID = "id"
		o.ClientSecret = "secret"
	})
	if _, _, err := p.List(context.Background(), "/apps/cloudfs", ""); err != nil {
		t.Fatal(err)
	}
	if n := x.count("list"); n != 2 {
		t.Errorf("list called %d times, want 2 (the failure plus the retry)", n)
	}
	if p.AccessToken() != "tok-2" {
		t.Errorf("access token = %q, want the refreshed one", p.AccessToken())
	}
}

func TestAuthErrnoWithoutRefreshCredentials(t *testing.T) {
	x, hs := newXpan(t)
	x.reply(pathFile, "list", "", `{"errno":-6}`)
	p := newProvider(t, hs)
	_, _, err := p.List(context.Background(), "/apps/cloudfs", "")
	if !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	_ = x
}

func TestParseID(t *testing.T) {
	cases := []struct {
		in   string
		fsid uint64
		path string
	}{
		{"111:/a/b", 111, "/a/b"},
		{"/a/b", 0, "/a/b"},
		{"", 0, "/"},
		{"/a/b/", 0, "/a/b"},
	}
	for _, tc := range cases {
		got := ParseID(tc.in)
		if got.FSID != tc.fsid || got.Path != tc.path {
			t.Errorf("ParseID(%q) = %+v, want %d %q", tc.in, got, tc.fsid, tc.path)
		}
		if tc.in != "" && tc.in != "/a/b/" && got.String() != tc.in {
			t.Errorf("round trip of %q gave %q", tc.in, got.String())
		}
	}
}

func TestCapabilities(t *testing.T) {
	_, hs := newXpan(t)
	p := newProvider(t, hs)
	c := p.Capabilities()
	if c.Delta {
		t.Error("Baidu publishes no delta feed")
	}
	if _, ok := any(p).(provider.ChangeLister); ok {
		t.Error("the driver must not implement ChangeLister")
	}
	if c.PartSize != 4<<20 {
		t.Errorf("part size = %d", c.PartSize)
	}
	if c.LinkTTL != 8*time.Hour {
		t.Errorf("link ttl = %v, want 8h", c.LinkTTL)
	}
	if c.LinkHeaders["User-Agent"] != DownloadUserAgent {
		t.Errorf("link headers = %v; MCP's get_download_url has to pass the UA on", c.LinkHeaders)
	}
	if c.QPS != (provider.QPS{Meta: 2, Download: 2, Upload: 1}) {
		t.Errorf("qps = %+v", c.QPS)
	}
	if c.Tier != provider.TierOfficial {
		t.Errorf("tier = %v", c.Tier)
	}
	if len(c.HashTypes) != 1 || c.HashTypes[0] != provider.HashMD5 {
		t.Errorf("hash types = %v", c.HashTypes)
	}
	if len(c.RapidUpload) != 2 || c.RapidUpload[0] != provider.HashMD5 || c.RapidUpload[1] != provider.HashSliceMD5 {
		t.Errorf("rapid upload hashes = %v", c.RapidUpload)
	}
}

func TestFactoryRequiresAccessToken(t *testing.T) {
	if _, err := Factory("bd", map[string]any{"client_id": "x"}); err == nil ||
		!strings.Contains(err.Error(), "access_token") {
		t.Fatalf("err = %v, want it to name the missing access_token key", err)
	}
	p, err := Factory("bd", map[string]any{"access_token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "bd" {
		t.Errorf("name = %q", p.Name())
	}
}

func TestRegistered(t *testing.T) {
	p, err := provider.New("baidu", "bd", map[string]any{"access_token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*Provider); !ok {
		t.Fatalf("registry returned %T", p)
	}
}
