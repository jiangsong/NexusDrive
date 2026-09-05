package strmgen

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type davEntry struct {
	href string
	dir  bool
	ok   bool
}

type fakeDAV struct {
	token    string
	listings map[string][]davEntry
	mu       sync.Mutex
	requests []string
}

func (f *fakeDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PROPFIND" || r.Header.Get("Depth") != "1" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if f.token != "" {
		user, password, ok := r.BasicAuth()
		if !ok || user != "cloudfs" || password != f.token {
			w.Header().Set("WWW-Authenticate", `Basic realm="cloudfs"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path)
	f.mu.Unlock()
	entries, ok := f.listings[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = fmt.Fprint(w, `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">`)
	for _, entry := range entries {
		status := "HTTP/1.1 404 Not Found"
		if entry.ok {
			status = "HTTP/1.1 200 OK"
		}
		kind := ""
		if entry.dir {
			kind = "<D:collection/>"
		}
		_, _ = fmt.Fprintf(w, "<D:response><D:href>%s</D:href><D:propstat><D:prop><D:resourcetype>%s</D:resourcetype></D:prop><D:status>%s</D:status></D:propstat></D:response>", xmlText(entry.href), kind, status)
	}
	_, _ = fmt.Fprint(w, `</D:multistatus>`)
}

func xmlText(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func TestGenerateRecursiveAndIncremental(t *testing.T) {
	fake := &fakeDAV{token: "secret", listings: map[string][]davEntry{
		"/dav/Movies/": {
			{href: "/dav/Movies/", dir: true, ok: true},
			{href: "/dav/Movies/Film%20One.mkv", ok: true},
			{href: "/dav/Movies/cover.jpg", ok: true},
			{href: "/dav/Movies/Season%201/", dir: true, ok: true},
		},
		"/dav/Movies/Season 1/": {
			{href: "/dav/Movies/Season%201/", dir: true, ok: true},
			{href: "/dav/Movies/Season%201/Episode%2001.mp4", ok: true},
		},
	}}
	server := httptest.NewServer(fake)
	defer server.Close()
	out := t.TempDir()
	opt := Options{StartURL: server.URL + "/dav/Movies", OutputDir: out, Token: "secret"}

	stats, err := Generate(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (Stats{Directories: 2, Media: 2, Written: 2, Skipped: 1}) {
		t.Fatalf("unexpected first stats: %+v", stats)
	}
	assertFile(t, filepath.Join(out, "Film One.strm"), server.URL+"/dav/Movies/Film%20One.mkv\n")
	assertFile(t, filepath.Join(out, "Season 1", "Episode 01.strm"), server.URL+"/dav/Movies/Season%201/Episode%2001.mp4\n")

	stats, err = Generate(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Written != 0 || stats.Unchanged != 2 {
		t.Fatalf("expected unchanged second run, got %+v", stats)
	}
}

func TestGenerateFiltersUnsafeResponses(t *testing.T) {
	fake := &fakeDAV{listings: map[string][]davEntry{
		"/dav/root/": {
			{href: "/dav/root/", dir: true, ok: true},
			{href: "/dav/root/good.mkv", ok: true},
			{href: "/dav/root/deep/hidden.mp4", ok: true},
			{href: "/dav/outside.mp4", ok: true},
			{href: "https://example.invalid/evil.mp4", ok: true},
			{href: "/dav/root/ignored.mp4", ok: false},
		},
	}}
	server := httptest.NewServer(fake)
	defer server.Close()
	out := t.TempDir()
	stats, err := Generate(context.Background(), Options{StartURL: server.URL + "/dav/root", OutputDir: out})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Media != 1 || stats.Written != 1 {
		t.Fatalf("unsafe responses were accepted: %+v", stats)
	}
	assertFile(t, filepath.Join(out, "good.strm"), server.URL+"/dav/root/good.mkv\n")
}

func TestGenerateLimitsAndCollisions(t *testing.T) {
	tests := []struct {
		name     string
		listing  map[string][]davEntry
		opt      Options
		wantText string
	}{
		{
			name: "collision",
			listing: map[string][]davEntry{"/root/": {
				{href: "/root/", dir: true, ok: true},
				{href: "/root/Movie.mkv", ok: true},
				{href: "/root/movie.mp4", ok: true},
			}},
			wantText: "map to the same output",
		},
		{
			name: "file limit",
			listing: map[string][]davEntry{"/root/": {
				{href: "/root/", dir: true, ok: true},
				{href: "/root/a.mp4", ok: true},
				{href: "/root/b.mp4", ok: true},
			}},
			opt:      Options{MaxFiles: 1},
			wantText: "file count exceeds 1",
		},
		{
			name: "depth limit",
			listing: map[string][]davEntry{
				"/root/":     {{href: "/root/", dir: true, ok: true}, {href: "/root/sub/", dir: true, ok: true}},
				"/root/sub/": {{href: "/root/sub/", dir: true, ok: true}},
			},
			opt:      Options{MaxDepth: -1},
			wantText: "out of range",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeDAV{listings: tt.listing}
			server := httptest.NewServer(fake)
			defer server.Close()
			tt.opt.StartURL = server.URL + "/root"
			tt.opt.OutputDir = t.TempDir()
			_, err := Generate(context.Background(), tt.opt)
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("got error %v, want substring %q", err, tt.wantText)
			}
		})
	}
}

func TestGenerateEmbedsAuthOnlyWhenRequested(t *testing.T) {
	fake := &fakeDAV{token: "p@ss:word", listings: map[string][]davEntry{
		"/root/": {{href: "/root/", dir: true, ok: true}, {href: "/root/movie.mp4", ok: true}},
	}}
	server := httptest.NewServer(fake)
	defer server.Close()
	out := t.TempDir()
	_, err := Generate(context.Background(), Options{
		StartURL: server.URL + "/root", OutputDir: out, Token: fake.token, EmbedBasicAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimSpace(readFile(t, filepath.Join(out, "movie.strm")))
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	user := u.User.Username()
	password, ok := u.User.Password()
	if !ok || user != "cloudfs" || password != fake.token {
		t.Fatalf("embedded credentials did not round trip: %q", raw)
	}
	info, err := os.Stat(filepath.Join(out, "movie.strm"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential-bearing file mode = %o, want 600", info.Mode().Perm())
	}
	if err := os.Chmod(filepath.Join(out, "movie.strm"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(context.Background(), Options{
		StartURL: server.URL + "/root", OutputDir: out, Token: fake.token, EmbedBasicAuth: true,
	}); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(filepath.Join(out, "movie.strm"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("unchanged credential file did not recover mode 600: %v, %v", info, err)
	}
	_, err = Generate(context.Background(), Options{
		StartURL: server.URL + "/root", OutputDir: t.TempDir(), EmbedBasicAuth: true,
	})
	if err == nil || !strings.Contains(err.Error(), "requires CLOUDFS_WEBDAV_TOKEN") {
		t.Fatalf("expected missing-token error, got %v", err)
	}
}

func TestGenerateRejectsSymlinkOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	fake := &fakeDAV{listings: map[string][]davEntry{
		"/root/":     {{href: "/root/", dir: true, ok: true}, {href: "/root/sub/", dir: true, ok: true}},
		"/root/sub/": {{href: "/root/sub/", dir: true, ok: true}, {href: "/root/sub/movie.mp4", ok: true}},
	}}
	server := httptest.NewServer(fake)
	defer server.Close()
	out := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(out, "sub")); err != nil {
		t.Fatal(err)
	}
	_, err := Generate(context.Background(), Options{StartURL: server.URL + "/root", OutputDir: out})
	if err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "movie.strm")); !os.IsNotExist(err) {
		t.Fatalf("generator escaped through symlink: %v", err)
	}
}

func TestPruneDeletesOnlyUnmodifiedManifestOutputs(t *testing.T) {
	fake := &fakeDAV{listings: map[string][]davEntry{
		"/root/": {
			{href: "/root/", dir: true, ok: true},
			{href: "/root/a.mp4", ok: true},
			{href: "/root/b.mp4", ok: true},
			{href: "/root/c.mp4", ok: true},
		},
	}}
	server := httptest.NewServer(fake)
	defer server.Close()
	out := t.TempDir()
	opt := Options{StartURL: server.URL + "/root", OutputDir: out, Prune: true}
	stats, err := Generate(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Written != 3 || stats.Pruned != 0 {
		t.Fatalf("first prune run should establish a baseline: %+v", stats)
	}
	manifestInfo, err := os.Stat(filepath.Join(out, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	if manifestInfo.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %o, want 600", manifestInfo.Mode().Perm())
	}
	if err := os.WriteFile(filepath.Join(out, "c.strm"), []byte("user replacement\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake.listings["/root/"] = []davEntry{
		{href: "/root/", dir: true, ok: true},
		{href: "/root/a.mp4", ok: true},
	}
	stats, err = Generate(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pruned != 1 || stats.Retained != 1 || stats.Unchanged != 1 {
		t.Fatalf("unexpected prune stats: %+v", stats)
	}
	if _, err := os.Stat(filepath.Join(out, "b.strm")); !os.IsNotExist(err) {
		t.Fatalf("unchanged stale output was not removed: %v", err)
	}
	assertFile(t, filepath.Join(out, "c.strm"), "user replacement\n")
	// Modified stale files leave the manifest and become user-owned; a later
	// prune must not keep attempting to delete them.
	stats, err = Generate(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Retained != 0 || stats.Pruned != 0 {
		t.Fatalf("modified file remained tracked after handoff: %+v", stats)
	}
}

func TestPruneRejectsChangedSourceAndSymlink(t *testing.T) {
	fake := &fakeDAV{listings: map[string][]davEntry{
		"/root/": {{href: "/root/", dir: true, ok: true}, {href: "/root/movie.mp4", ok: true}},
	}}
	server := httptest.NewServer(fake)
	defer server.Close()
	out := t.TempDir()
	opt := Options{StartURL: server.URL + "/root", OutputDir: out, Prune: true}
	if _, err := Generate(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(context.Background(), Options{
		StartURL: server.URL + "/other", OutputDir: out, Prune: true,
	}); err == nil || !strings.Contains(err.Error(), "source does not match") {
		t.Fatalf("expected source mismatch, got %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	target := filepath.Join(out, "movie.strm")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	fake.listings["/root/"] = []davEntry{{href: "/root/", dir: true, ok: true}}
	if _, err := Generate(context.Background(), opt); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("expected stale symlink rejection, got %v", err)
	}
	assertFile(t, outside, "keep")
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	if got := readFile(t, path); got != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
