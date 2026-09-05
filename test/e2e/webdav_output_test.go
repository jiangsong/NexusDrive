package e2e

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
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
