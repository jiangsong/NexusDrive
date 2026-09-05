package e2e

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/daemon"
	"cloudfs/internal/provider"
	"cloudfs/internal/webdavsrv"
	"cloudfs/test/fakeprovider"
)

// This path deliberately does not mount FUSE: it proves that a NAS/container
// can serve WebDAV through the same assembled VFS when /dev/fuse is absent.
func TestWebDAVOutputUsesAssembledVFSWithoutFUSE(t *testing.T) {
	base := t.TempDir()
	cfg := config.Default()
	cfg.SourcePath = filepath.Join(base, "config.yaml")
	cfg.Cache.Dir = filepath.Join(base, "cache")
	cfg.Cache.MaxSize = 64 << 20
	cfg.Cache.MinFree = 0
	cfg.Cache.BlockSize = 64 << 10
	cfg.Cache.SubBlockSize = 16 << 10
	cfg.Control.Socket = ""
	cfg.Remotes = map[string]config.Remote{"demo": {Type: "fake"}}
	cfg.Mounts = []config.Mount{{Path: filepath.Join(base, "unused-mount"), Layout: map[string]config.Layout{
		"/": {Remote: "demo", Root: fakeprovider.RootID, Mode: config.ModeReadonly, DirTTL: time.Hour},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := daemon.Open(ctx, daemon.Options{Config: &cfg, Version: "webdav-e2e"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	fake, _ := provider.Unwrap(d.Providers["demo"]).(*fakeprovider.Fake)
	if fake == nil {
		t.Fatal("fake provider was not assembled")
	}
	fake.Seed("movies/demo.txt", []byte("served through the VFS"))

	running, err := webdavsrv.Start(ctx, webdavsrv.Options{
		FS: d.FS, Addr: "127.0.0.1:0", Prefix: "/dav", Root: "/movies", Token: "e2e-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()

	req, _ := http.NewRequest(http.MethodGet, "http://"+running.Addr()+"/dav/demo.txt", nil)
	req.Header.Set("Authorization", "Bearer e2e-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK || string(body) != "served through the VFS" {
		t.Fatalf("WebDAV/VFS response = %d %q, %v", resp.StatusCode, body, readErr)
	}
	if fake.Calls("ReadRange") == 0 {
		t.Fatal("WebDAV response bypassed the assembled VFS/provider read path")
	}

	before := fake.TotalCalls()
	req, _ = http.NewRequest(http.MethodGet, "http://"+running.Addr()+"/dav/demo.txt", nil)
	req.Header.Set("Authorization", "Bearer e2e-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if after := fake.TotalCalls(); after != before {
		t.Fatalf("warm WebDAV read made %d extra provider calls", after-before)
	}
}

// TestWritableWebDAVGoesThroughTheRealWritePath proves the writable endpoint is
// the same write path FUSE uses: a PUT lands in the journal, the upload queue
// drains it to the backend, and a COPY is served by the provider rather than by
// moving the bytes through this process.
//
// It runs without FUSE on purpose — a container or NAS without /dev/fuse is the
// case this endpoint exists for.
func TestWritableWebDAVGoesThroughTheRealWritePath(t *testing.T) {
	base := t.TempDir()
	cfg := config.Default()
	cfg.SourcePath = filepath.Join(base, "config.yaml")
	cfg.Cache.Dir = filepath.Join(base, "cache")
	cfg.Cache.MaxSize = 64 << 20
	cfg.Cache.MinFree = 0
	cfg.Cache.BlockSize = 64 << 10
	cfg.Cache.SubBlockSize = 16 << 10
	cfg.Control.Socket = ""
	cfg.Remotes = map[string]config.Remote{"demo": {Type: "fake"}}
	cfg.Mounts = []config.Mount{{Path: filepath.Join(base, "unused-mount"), Layout: map[string]config.Layout{
		"/": {Remote: "demo", Root: fakeprovider.RootID, Mode: config.ModeWriteback, DirTTL: time.Hour},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := daemon.Open(ctx, daemon.Options{Config: &cfg, Version: "webdav-write-e2e"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	fake, _ := provider.Unwrap(d.Providers["demo"]).(*fakeprovider.Fake)
	if fake == nil {
		t.Fatal("fake provider was not assembled")
	}
	fake.Seed("share/keep.txt", []byte("already there"))

	running, err := webdavsrv.Start(ctx, webdavsrv.Options{
		FS: d.FS, Addr: "127.0.0.1:0", Prefix: "/dav", Root: "/share",
		Token: "e2e-token", Writable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()
	dav := "http://" + running.Addr() + "/dav"

	do := func(method, target, body string, headers map[string]string) *http.Response {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, dav+target, reader)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer e2e-token")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	status := func(method, target, body string, headers map[string]string) int {
		t.Helper()
		resp := do(method, target, body, headers)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	payload := "uploaded over WebDAV without a FUSE mount"
	if got := status(http.MethodPut, "/uploaded.txt", payload, nil); got != http.StatusCreated {
		t.Fatalf("PUT returned %d, want 201", got)
	}
	// Writeback: the write is committed locally and the queue carries it out.
	waitDrain(t, d.Journal, 30*time.Second)
	if content, ok := fake.Content("share/uploaded.txt"); !ok || string(content) != payload {
		t.Fatalf("backend holds %q (present=%v); the PUT did not reach the provider", content, ok)
	}

	// The file is readable back through the same endpoint.
	resp := do(http.MethodGet, "/uploaded.txt", "", nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != payload {
		t.Fatalf("GET after PUT = %d %q", resp.StatusCode, got)
	}

	if got := status("MKCOL", "/season-1", "", nil); got != http.StatusCreated {
		t.Fatalf("MKCOL returned %d, want 201", got)
	}
	if got := status("MOVE", "/uploaded.txt", "", map[string]string{
		"Destination": dav + "/season-1/moved.txt",
	}); got != http.StatusCreated {
		t.Fatalf("MOVE returned %d, want 201", got)
	}

	// COPY of a file whose bytes are already cached must not download them
	// again. Letting the DAV handler do the copy would read the source back
	// through this process, which is exactly what vfs.Copy exists to avoid.
	beforeReads := fake.Calls("ReadRange")
	if got := status("COPY", "/season-1/moved.txt", "", map[string]string{
		"Destination": dav + "/season-1/copied.txt",
	}); got != http.StatusCreated {
		t.Fatalf("COPY returned %d, want 201", got)
	}
	waitDrain(t, d.Journal, 30*time.Second)
	if extra := fake.Calls("ReadRange") - beforeReads; extra != 0 {
		t.Fatalf("COPY of a cached file pulled %d byte ranges from the backend", extra)
	}
	if content, ok := fake.Content("share/season-1/copied.txt"); !ok || string(content) != payload {
		t.Fatalf("copied file on the backend holds %q (present=%v)", content, ok)
	}

	if got := status(http.MethodDelete, "/season-1", "", nil); got != http.StatusNoContent {
		t.Fatalf("DELETE returned %d, want 204", got)
	}
	waitDrain(t, d.Journal, 30*time.Second)
	if got := status(http.MethodGet, "/season-1/moved.txt", "", nil); got != http.StatusNotFound {
		t.Fatalf("GET after DELETE returned %d, want 404", got)
	}
}
