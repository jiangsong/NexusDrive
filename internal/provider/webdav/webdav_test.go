package webdav

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"

	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// davServer is a small in-memory WebDAV server: enough of the protocol to
// exercise the driver end to end.
type davServer struct {
	mu    sync.Mutex
	files map[string][]byte // path -> content ("" content marks a collection)
	dirs  map[string]bool
	// requests records method+path for assertions.
	requests []string
	headers  []http.Header
}

func newDAV() *davServer {
	return &davServer{files: map[string][]byte{}, dirs: map[string]bool{"/": true}}
}

func (d *davServer) handler(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, prefix)
		if p == "" {
			p = "/"
		}
		p = path.Clean("/" + strings.TrimSuffix(p, "/"))
		d.mu.Lock()
		d.requests = append(d.requests, r.Method+" "+p)
		d.headers = append(d.headers, r.Header.Clone())
		d.mu.Unlock()

		switch r.Method {
		case "PROPFIND":
			d.propfind(w, r, p, prefix)
		case http.MethodGet:
			d.get(w, r, p)
		case http.MethodPut:
			d.put(w, r, p)
		case "MKCOL":
			d.mkcol(w, p)
		case "MOVE", "COPY":
			d.moveCopy(w, r, p, prefix)
		case http.MethodDelete:
			d.delete(w, p)
		default:
			http.Error(w, "not implemented", http.StatusNotImplemented)
		}
	})
}

func (d *davServer) propfind(w http.ResponseWriter, r *http.Request, p, prefix string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, isFile := d.files[p]
	if !d.dirs[p] && !isFile {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	depth := r.Header.Get("Depth")
	var body strings.Builder
	body.WriteString(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">`)
	write := func(target string) {
		href := prefix + target
		if d.dirs[target] {
			fmt.Fprintf(&body, `<d:response><d:href>%s</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop><d:displayname>%s</d:displayname><d:resourcetype><d:collection/></d:resourcetype><d:getlastmodified>Wed, 02 Sep 2026 10:00:00 GMT</d:getlastmodified></d:prop></d:propstat></d:response>`,
				href, path.Base(target))
			return
		}
		content := d.files[target]
		fmt.Fprintf(&body, `<d:response><d:href>%s</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop><d:displayname>%s</d:displayname><d:getcontentlength>%d</d:getcontentlength><d:getetag>"etag-%s"</d:getetag><d:getlastmodified>Wed, 02 Sep 2026 10:00:00 GMT</d:getlastmodified><d:resourcetype/></d:prop></d:propstat></d:response>`,
			href, path.Base(target), len(content), path.Base(target))
	}
	write(p)
	if depth == "1" && d.dirs[p] {
		var kids []string
		for f := range d.files {
			if path.Dir(f) == p {
				kids = append(kids, f)
			}
		}
		for dir := range d.dirs {
			if dir != p && path.Dir(dir) == p {
				kids = append(kids, dir)
			}
		}
		sort.Strings(kids)
		for _, k := range kids {
			write(k)
		}
	}
	body.WriteString(`</d:multistatus>`)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusMultiStatus)
	io.WriteString(w, body.String())
}

func (d *davServer) get(w http.ResponseWriter, r *http.Request, p string) {
	d.mu.Lock()
	content, ok := d.files[p]
	d.mu.Unlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rng := r.Header.Get("Range")
	if rng == "" {
		w.Write(content)
		return
	}
	var start, end int64
	if n, _ := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); n == 2 {
		if end >= int64(len(content)) {
			end = int64(len(content)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(content[start : end+1])
		return
	}
	if n, _ := fmt.Sscanf(rng, "bytes=%d-", &start); n == 1 {
		w.WriteHeader(http.StatusPartialContent)
		w.Write(content[start:])
		return
	}
	http.Error(w, "bad range", http.StatusBadRequest)
}

func (d *davServer) put(w http.ResponseWriter, r *http.Request, p string) {
	body, _ := io.ReadAll(r.Body)
	d.mu.Lock()
	_, existed := d.files[p]
	d.files[p] = body
	d.mu.Unlock()
	w.Header().Set("ETag", `"etag-`+path.Base(p)+`"`)
	if existed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (d *davServer) mkcol(w http.ResponseWriter, p string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dirs[p] {
		http.Error(w, "exists", http.StatusMethodNotAllowed)
		return
	}
	d.dirs[p] = true
	w.WriteHeader(http.StatusCreated)
}

func (d *davServer) moveCopy(w http.ResponseWriter, r *http.Request, p, prefix string) {
	dest := r.Header.Get("Destination")
	if dest == "" {
		http.Error(w, "no destination", http.StatusBadRequest)
		return
	}
	if i := strings.Index(dest, prefix); i >= 0 {
		dest = dest[i+len(prefix):]
	}
	dest = path.Clean("/" + strings.TrimSuffix(dest, "/"))

	d.mu.Lock()
	defer d.mu.Unlock()
	if r.Header.Get("Overwrite") == "F" {
		if _, exists := d.files[dest]; exists || d.dirs[dest] {
			http.Error(w, "target exists", http.StatusPreconditionFailed)
			return
		}
	}
	if content, ok := d.files[p]; ok {
		d.files[dest] = content
		if r.Method == "MOVE" {
			delete(d.files, p)
		}
		w.WriteHeader(http.StatusCreated)
		return
	}
	if d.dirs[p] {
		d.dirs[dest] = true
		if r.Method == "MOVE" {
			delete(d.dirs, p)
		}
		w.WriteHeader(http.StatusCreated)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func (d *davServer) delete(w http.ResponseWriter, p string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.files[p]; ok {
		delete(d.files, p)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if d.dirs[p] {
		delete(d.dirs, p)
		for f := range d.files {
			if strings.HasPrefix(f, p+"/") {
				delete(d.files, f)
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func (d *davServer) sawHeader(method, name, value string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, req := range d.requests {
		if strings.HasPrefix(req, method+" ") && d.headers[i].Get(name) == value {
			return true
		}
	}
	return false
}

func newProvider(t *testing.T, prefix string) (*Provider, *davServer, *httptest.Server) {
	t.Helper()
	dav := newDAV()
	srv := httptest.NewServer(http.StripPrefix("", dav.handler(prefix)))
	t.Cleanup(srv.Close)
	p, err := New(Options{
		Name:    "nas",
		BaseURL: srv.URL + prefix,
		User:    "alice",
		Pass:    "secret",
		Client:  httpx.New(httpx.Options{Remote: "nas"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p, dav, srv
}

func TestListAndStat(t *testing.T) {
	p, dav, _ := newProvider(t, "/dav")
	dav.files["/notes.txt"] = []byte("hello")
	dav.files["/data.bin"] = []byte("0123456789")
	dav.dirs["/sub"] = true
	dav.files["/sub/inner.txt"] = []byte("inner")

	ctx := context.Background()
	entries, next, err := p.List(ctx, RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "" {
		t.Fatalf("PROPFIND returns everything at once, cursor = %q", next)
	}
	byName := map[string]provider.Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %+v", entries)
	}
	if e := byName["notes.txt"]; e.Kind != provider.KindFile || e.Size != 5 || e.ID != "/notes.txt" {
		t.Fatalf("file entry = %+v", e)
	}
	if e := byName["sub"]; e.Kind != provider.KindDir {
		t.Fatalf("dir entry = %+v", e)
	}
	if e := byName["notes.txt"]; e.Version != "etag-notes.txt" {
		t.Fatalf("etag not used as version: %q", e.Version)
	}
	if e := byName["notes.txt"]; e.ModTime.IsZero() {
		t.Fatal("mtime not parsed")
	}

	// The collection must not list itself.
	for _, e := range entries {
		if e.ID == "/" {
			t.Fatal("listing included the collection itself")
		}
	}
	// Nested listing works with the right parent id.
	sub, _, err := p.List(ctx, "/sub", "")
	if err != nil || len(sub) != 1 || sub[0].Name != "inner.txt" {
		t.Fatalf("nested list = %+v, %v", sub, err)
	}

	e, err := p.Stat(ctx, "/notes.txt")
	if err != nil || e.Size != 5 {
		t.Fatalf("stat = %+v, %v", e, err)
	}
	if _, err := p.Stat(ctx, "/missing.txt"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("stat missing = %v", err)
	}
	// Basic auth is sent.
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret"))
	if !dav.sawHeader("PROPFIND", "Authorization", want) {
		t.Fatal("basic auth header not sent")
	}
}

func TestReadRange(t *testing.T) {
	p, dav, _ := newProvider(t, "/dav")
	dav.files["/data.bin"] = []byte("0123456789abcdef")
	ctx := context.Background()

	rc, err := p.ReadRange(ctx, "/data.bin", "", 4, 6)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "456789" {
		t.Fatalf("range = %q", got)
	}
	if !dav.sawHeader("GET", "Range", "bytes=4-9") {
		t.Fatal("Range header not sent")
	}
	// An open-ended read returns the tail.
	rc, err = p.ReadRange(ctx, "/data.bin", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if string(got) != "abcdef" {
		t.Fatalf("tail = %q", got)
	}
}

func TestReadRangeWhenServerIgnoresRange(t *testing.T) {
	// Some WebDAV servers ignore Range and return 200 with the whole file.
	// The driver must still hand back the requested window.
	content := []byte("0123456789abcdef")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			w.Write(content)
			return
		}
		http.Error(w, "unexpected", http.StatusNotImplemented)
	}))
	defer srv.Close()
	p, err := New(Options{Name: "n", BaseURL: srv.URL, Client: httpx.New(httpx.Options{Remote: "n"})})
	if err != nil {
		t.Fatal(err)
	}
	rc, err := p.ReadRange(context.Background(), "/f", "", 4, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "456789" {
		t.Fatalf("compensated range = %q", got)
	}
}

func TestUploadRoundTrip(t *testing.T) {
	p, dav, _ := newProvider(t, "/dav")
	ctx := context.Background()
	content := []byte("uploaded through webdav")

	sess, err := p.BeginUpload(ctx, RootID, "new.txt", int64(len(content)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.RapidDone {
		t.Fatal("webdav has no rapid upload")
	}
	tok, err := p.UploadPart(ctx, sess, 0, strings.NewReader(string(content)), int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if tok.ETag == "" {
		t.Fatal("ETag not captured from the PUT response")
	}
	e, err := p.CompleteUpload(ctx, sess, []provider.PartToken{tok})
	if err != nil {
		t.Fatal(err)
	}
	if e.Size != int64(len(content)) || e.ID != "/new.txt" {
		t.Fatalf("completed entry = %+v", e)
	}
	if string(dav.files["/new.txt"]) != string(content) {
		t.Fatalf("server content = %q", dav.files["/new.txt"])
	}
	// A second part is refused rather than silently overwriting the file.
	if _, err := p.UploadPart(ctx, sess, 1, strings.NewReader("x"), 1); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("second part = %v", err)
	}
}

func TestMkdirRenameMoveCopyDelete(t *testing.T) {
	p, dav, _ := newProvider(t, "/dav")
	ctx := context.Background()

	d, err := p.Mkdir(ctx, RootID, "project")
	if err != nil || d.Kind != provider.KindDir || d.ID != "/project" {
		t.Fatalf("mkdir = %+v, %v", d, err)
	}
	if _, err := p.Mkdir(ctx, RootID, "project"); !errors.Is(err, provider.ErrExists) {
		t.Fatalf("duplicate mkdir = %v", err)
	}

	dav.files["/project/a.txt"] = []byte("a")
	e, err := p.Rename(ctx, "/project/a.txt", "b.txt")
	if err != nil || e.ID != "/project/b.txt" {
		t.Fatalf("rename = %+v, %v", e, err)
	}
	if _, ok := dav.files["/project/a.txt"]; ok {
		t.Fatal("old name still present after rename")
	}

	if _, err := p.Mkdir(ctx, RootID, "archive"); err != nil {
		t.Fatal(err)
	}
	e, err = p.Move(ctx, "/project/b.txt", "/archive")
	if err != nil || e.ID != "/archive/b.txt" {
		t.Fatalf("move = %+v, %v", e, err)
	}

	e, err = p.Copy(ctx, "/archive/b.txt", "/project", "copy.txt")
	if err != nil || e.ID != "/project/copy.txt" {
		t.Fatalf("copy = %+v, %v", e, err)
	}
	if string(dav.files["/project/copy.txt"]) != "a" {
		t.Fatalf("copied content = %q", dav.files["/project/copy.txt"])
	}
	// Moving onto an existing name is refused, not silently overwritten: the
	// VFS decides how to resolve a conflict, the driver must not clobber.
	dav.files["/project/b.txt"] = []byte("already here")
	if _, err := p.Move(ctx, "/archive/b.txt", "/project"); !errors.Is(err, provider.ErrExists) {
		t.Fatalf("move onto an existing name = %v, want ErrExists", err)
	}
	if string(dav.files["/project/b.txt"]) != "already here" {
		t.Fatal("refused move must leave the target untouched")
	}

	if err := p.Delete(ctx, "/project"); err != nil {
		t.Fatal(err)
	}
	if dav.dirs["/project"] {
		t.Fatal("directory not deleted")
	}
	if _, ok := dav.files["/project/copy.txt"]; ok {
		t.Fatal("subtree not deleted")
	}
	if err := p.Delete(ctx, "/gone"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("delete missing = %v", err)
	}
}

func TestFingerprintWithoutETag(t *testing.T) {
	// A server with no ETag must still produce a version that changes when the
	// file changes, otherwise the cache would serve stale blocks.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">
		  <d:response><d:href>/f.txt</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status>
		  <d:prop><d:displayname>f.txt</d:displayname><d:getcontentlength>42</d:getcontentlength>
		  <d:getlastmodified>Wed, 02 Sep 2026 10:00:00 GMT</d:getlastmodified><d:resourcetype/></d:prop>
		  </d:propstat></d:response></d:multistatus>`)
	}))
	defer srv.Close()
	p, _ := New(Options{Name: "n", BaseURL: srv.URL, Client: httpx.New(httpx.Options{Remote: "n"})})
	e, err := p.Stat(context.Background(), "/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if e.Version == "" || !strings.HasPrefix(e.Version, "42-") {
		t.Fatalf("fallback fingerprint = %q, want it to include the size", e.Version)
	}
}

func TestCapabilities(t *testing.T) {
	p, _, _ := newProvider(t, "/dav")
	c := p.Capabilities()
	if len(c.HashTypes) != 0 {
		t.Error("plain WebDAV exposes no content hash")
	}
	if !c.RangeRead || !c.ServerMove || !c.ServerRename || !c.ServerCopy {
		t.Errorf("caps = %+v", c)
	}
	if c.Delta {
		t.Error("WebDAV has no change feed")
	}
	if !c.PathIDs {
		t.Error("a WebDAV id is a path, so renaming a collection changes every id beneath it; the VFS needs to be told")
	}
	if c.LinkShareable {
		t.Error("WebDAV URLs need this process's credentials and must not be advertised as shareable")
	}
}

func TestFactoryValidatesConfig(t *testing.T) {
	if _, err := Factory("nas", map[string]any{}); err == nil {
		t.Fatal("missing url should fail")
	} else if !strings.Contains(err.Error(), "url") {
		t.Fatalf("error should name the missing key: %v", err)
	}
	p, err := Factory("nas", map[string]any{"url": "https://example.com/dav", "user": "u", "pass": "p"})
	if err != nil || p.Name() != "nas" {
		t.Fatalf("factory = %v, %v", p, err)
	}
	// Both names resolve to this driver.
	for _, typ := range []string{"webdav", "openlist"} {
		if _, err := provider.New(typ, "x", map[string]any{"url": "https://example.com"}); err != nil {
			t.Errorf("provider.New(%q) = %v", typ, err)
		}
	}
}

func TestBasePathPrefixHandled(t *testing.T) {
	// The base URL may include a path prefix; ids must stay relative to it.
	p, dav, _ := newProvider(t, "/remote.php/dav/files/alice")
	dav.files["/deep.txt"] = []byte("x")
	entries, _, err := p.List(context.Background(), RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "/deep.txt" {
		t.Fatalf("entries with a base prefix = %+v", entries)
	}
}

func TestInsufficientStorageIsClassifiedAsQuota(t *testing.T) {
	err := mapErr(&httpx.StatusError{Code: http.StatusInsufficientStorage, Status: "507 Insufficient Storage", URL: "https://dav.example.com/big.bin"})
	if !errors.Is(err, provider.ErrQuotaExceeded) {
		t.Fatalf("507 mapped to %v, want ErrQuotaExceeded", err)
	}
	if got := retry.Classify(err); got != retry.ClassQuota {
		t.Fatalf("Classify = %v, want quota: a full server must not be retried as a 5xx", got)
	}
	// Every other status keeps the mapping httpx already gives it.
	if got := mapErr(&httpx.StatusError{Code: http.StatusServiceUnavailable}); errors.Is(got, provider.ErrQuotaExceeded) {
		t.Fatal("a plain 503 must not be read as a full server")
	}
}
