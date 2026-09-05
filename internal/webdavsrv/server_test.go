package webdavsrv

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
)

type fakeBackend struct {
	mu       sync.Mutex
	paths    []string
	released int
	body     []byte
	link     provider.Link
	linkErr  error
}

func (f *fakeBackend) record(p string) { f.mu.Lock(); f.paths = append(f.paths, p); f.mu.Unlock() }

func (f *fakeBackend) StatPath(_ context.Context, p string) (vfs.Attr, error) {
	f.record(p)
	switch p {
	case "/share":
		return vfs.Attr{Ino: 1, Name: "share", IsDir: true, MTime: time.Unix(100, 0)}, nil
	case "/share/movie.txt":
		return vfs.Attr{Ino: 2, Name: "movie.txt", Size: int64(len(f.body)), MTime: time.Unix(200, 0), Remote: "media", Version: "provider-private-version"}, nil
	default:
		return vfs.Attr{}, vfs.ErrNotFound
	}
}

func (f *fakeBackend) ReadDirPath(_ context.Context, p string) ([]vfs.Attr, error) {
	f.record(p)
	if p != "/share" {
		return nil, vfs.ErrNotFound
	}
	return []vfs.Attr{{Ino: 2, Name: "movie.txt", Size: int64(len(f.body)), MTime: time.Unix(200, 0), Remote: "media", Version: "provider-private-version"}}, nil
}

func (f *fakeBackend) Open(_ context.Context, ino uint64, write bool) (*vfs.Handle, error) {
	if write || ino != 2 {
		return nil, vfs.ErrNotFound
	}
	return &vfs.Handle{Ino: ino}, nil
}

func (f *fakeBackend) Read(_ context.Context, h *vfs.Handle, p []byte, off int64) (int, error) {
	if h == nil || h.Ino != 2 {
		return 0, vfs.ErrNotFound
	}
	if off >= int64(len(f.body)) {
		return 0, io.EOF
	}
	return copy(p, f.body[off:]), nil
}

func (f *fakeBackend) Release(context.Context, *vfs.Handle) error {
	f.mu.Lock()
	f.released++
	f.mu.Unlock()
	return nil
}

func (f *fakeBackend) DownloadURL(_ context.Context, p string) (provider.Link, error) {
	f.record(p)
	return f.link, f.linkErr
}

func TestReadOnlyWebDAVHTTP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := &fakeBackend{body: []byte("0123456789")}
	running, err := Start(ctx, Options{FS: backend, Addr: "127.0.0.1:0", Prefix: "/dav", Root: "/share", Token: "private-token"})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()
	base := "http://" + running.Addr() + "/dav"

	resp, err := http.Get(base + "/movie.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("unauthenticated response = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	req, _ := http.NewRequest(http.MethodOptions, base+"/", nil)
	req.Header.Set("Authorization", "Bearer private-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(resp.Header.Get("Allow"), "PUT") || resp.Header.Get("DAV") != "1" {
		t.Fatalf("read-only OPTIONS = %d allow=%q dav=%q", resp.StatusCode, resp.Header.Get("Allow"), resp.Header.Get("DAV"))
	}

	req, _ = http.NewRequest(http.MethodGet, base+"/movie.txt", nil)
	req.Header.Set("Authorization", "Bearer private-token")
	req.Header.Set("Range", "bytes=2-5")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusPartialContent || string(body) != "2345" {
		t.Fatalf("range response = %d %q, %v", resp.StatusCode, body, readErr)
	}
	if etag := resp.Header.Get("ETag"); etag == "" || strings.Contains(etag, "provider-private-version") {
		t.Fatalf("unsafe or missing ETag %q", etag)
	}

	req, _ = http.NewRequest("PROPFIND", base+"/", nil)
	req.SetBasicAuth("cloudfs", "private-token")
	req.Header.Set("Depth", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr = io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusMultiStatus || !bytes.Contains(body, []byte("movie.txt")) {
		t.Fatalf("PROPFIND response = %d %q, %v", resp.StatusCode, body, readErr)
	}

	req, _ = http.NewRequest(http.MethodPut, base+"/new.txt", strings.NewReader("no"))
	req.Header.Set("Authorization", "Bearer private-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("read-only PUT response = %d", resp.StatusCode)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.released == 0 {
		t.Fatal("WebDAV did not release its VFS read handles")
	}
	for _, p := range backend.paths {
		if p != "/share" && p != "/share/movie.txt" {
			t.Fatalf("request escaped configured root: %q", p)
		}
	}
}

func TestWebDAVRejectsUnboundedPropfind(t *testing.T) {
	backend := &fakeBackend{body: []byte("x")}
	running, err := Start(t.Context(), Options{FS: backend, Addr: "127.0.0.1:0", Prefix: "/dav", Root: "/share"})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()
	req, _ := http.NewRequest("PROPFIND", "http://"+running.Addr()+"/dav/", nil)
	req.Header.Set("Depth", "infinity")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("infinite PROPFIND status = %d", resp.StatusCode)
	}
}

func TestWebDAVNonLoopbackRequiresToken(t *testing.T) {
	backend := &fakeBackend{}
	if _, err := Start(t.Context(), Options{FS: backend, Addr: "0.0.0.0:0", Prefix: "/dav", Root: "/"}); err == nil || !strings.Contains(err.Error(), "CLOUDFS_WEBDAV_TOKEN") {
		t.Fatalf("non-loopback without token = %v", err)
	}
	if _, err := Start(t.Context(), Options{FS: backend, Addr: "0.0.0.0:0", Prefix: "/dav", Root: "/", Token: "short"}); err == nil || !strings.Contains(err.Error(), "at least 16 bytes") {
		t.Fatalf("non-loopback with weak token = %v", err)
	}
	if _, err := Start(t.Context(), Options{FS: backend, Addr: "127.0.0.1:0", Prefix: "/dav", Root: "/", Token: "bad\nheader"}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("newline token = %v", err)
	}
}

func TestWebDAVRootMappingCannotEscape(t *testing.T) {
	f := &readOnlyFS{backend: &fakeBackend{}, root: "/share"}
	for _, name := range []string{"/", "/../", "/../../outside", "//movie.txt"} {
		got, err := f.resolve(name)
		if err != nil || (got != "/share" && !strings.HasPrefix(got, "/share/")) {
			t.Errorf("resolve(%q) = %q, %v", name, got, err)
		}
	}
	if _, err := f.resolve("/bad\\name"); err == nil {
		t.Fatal("backslash path accepted")
	}
}

func TestWebDAVContextStopsListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	running, err := Start(ctx, Options{FS: &fakeBackend{}, Addr: "127.0.0.1:0", Prefix: "/dav", Root: "/"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-running.done:
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not stop after context cancellation")
	}
	if err := running.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWebDAVDirectDownloadStrategies(t *testing.T) {
	start := func(t *testing.T, strategy string, link provider.Link) (*Running, *fakeBackend) {
		t.Helper()
		backend := &fakeBackend{body: []byte("proxied"), link: link}
		running, err := Start(t.Context(), Options{FS: backend, Addr: "127.0.0.1:0", Prefix: "/dav", Root: "/share", Strategy: strategy})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = running.Close() })
		return running, backend
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	running, _ := start(t, "auto", provider.Link{URL: "https://cdn.example/download?signature=private"})
	resp, err := client.Get("http://" + running.Addr() + "/dav/movie.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://cdn.example/download?signature=private" || resp.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("auto redirect = %d %q %q", resp.StatusCode, resp.Header.Get("Location"), resp.Header.Get("Cache-Control"))
	}

	running, _ = start(t, "auto", provider.Link{URL: "https://cdn.example/requires-ua", Headers: map[string]string{"User-Agent": "provider-client"}})
	resp, err = client.Get("http://" + running.Addr() + "/dav/movie.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK || string(body) != "proxied" {
		t.Fatalf("header-bound auto fallback = %d %q, %v", resp.StatusCode, body, readErr)
	}

	running, _ = start(t, "redirect", provider.Link{URL: "https://cdn.example/requires-ua", Headers: map[string]string{"Referer": "https://provider.example/"}})
	resp, err = client.Get("http://" + running.Addr() + "/dav/movie.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Location") != "" {
		t.Fatalf("forced redirect with headers = %d location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestSafeRedirect(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://cdn.example/file?sig=x", true},
		{"http://127.0.0.1/file", true},
		{"javascript:alert(1)", false},
		{"https://user:pass@cdn.example/file", false},
		{"//cdn.example/file", false},
		{"https://cdn.example/file\r\nX-Evil: yes", false},
	} {
		if got := safeRedirect(tc.url); got != tc.want {
			t.Errorf("safeRedirect(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
