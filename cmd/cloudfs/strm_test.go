package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"cloudfs/internal/config"
)

func TestCmdSTRMEndToEnd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" || r.URL.Path != "/dav/Films/" || r.Header.Get("Depth") != "1" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = fmt.Fprint(w, `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">
<D:response><D:href>/dav/Films/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
<D:response><D:href>/dav/Films/Movie%20One.mkv</D:href><D:propstat><D:prop><D:resourcetype/></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
</D:multistatus>`)
	}))
	defer server.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("webdav:\n  prefix: /dav\n  root: /media\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "strm")
	err := cmdSTRM(context.Background(), []string{
		"/media/Films", "--out", out, "--base-url", server.URL + "/dav", "--config", configPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "Movie One.strm"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), server.URL+"/dav/Films/Movie%20One.mkv\n"; got != want {
		t.Fatalf("generated content = %q, want %q", got, want)
	}
}

func TestSTRMStartURL(t *testing.T) {
	cfg := &config.Config{}
	cfg.WebDAV.HTTP = "0.0.0.0:8080"
	cfg.WebDAV.Prefix = "/dav"
	cfg.WebDAV.Root = "/media"
	got, err := strmStartURL(cfg, "/media/Films/One", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:8080/dav/Films/One" {
		t.Fatalf("got %q", got)
	}
	if _, err := strmStartURL(cfg, "/private", ""); err == nil {
		t.Fatal("expected path outside WebDAV root to fail")
	}
}

func TestSTRMStartURLOverrideEscapesPath(t *testing.T) {
	cfg := &config.Config{}
	cfg.WebDAV.Root = "/"
	got, err := strmStartURL(cfg, "/电影/A B", "https://media.example/dav")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://media.example/dav/%E7%94%B5%E5%BD%B1/A%20B" {
		t.Fatalf("got %q", got)
	}
}
