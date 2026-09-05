package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
	"gopkg.in/yaml.v3"
)

func TestCacheCLIUsesOwnerInsteadOfBuildingAnotherStack(t *testing.T) {
	cfg, configPath := uploadCLIConfig(t) // deliberately unregistered provider
	dir := t.TempDir()
	s, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p := fakeprovider.New("ali")
	p.Seed("/a", []byte("live content"))
	f, err := vfs.New(vfs.Options{Meta: s, Cache: c, Mounts: []vfs.Mount{{Prefix: "/ali", Remote: "ali", RootID: fakeprovider.RootID, Provider: p}}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx := context.Background()
	srv, err := control.NewServer(&control.Collector{FS: f, Cache: c}).Start(ctx, cfg.Control.Socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	for _, tc := range []struct {
		action string
		args   []string
	}{
		{"warm", []string{"/ali", "0"}}, {"pin", []string{"/ali"}},
		{"cache", []string{"stats"}}, {"cache", []string{"pins"}},
		{"unpin", []string{"/ali"}}, {"cache", []string{"gc"}},
	} {
		var out bytes.Buffer
		args := append(tc.args, "--json", "--config", configPath)
		if err := runCache(ctx, tc.action, args, &out); err != nil {
			t.Fatalf("%s: %v", tc.action, err)
		}
		if !strings.Contains(out.String(), `"stats"`) {
			t.Fatal(out.String())
		}
	}
	if _, err := os.Stat(cfg.Cache.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("online CLI touched another cache: %v", err)
	}
	if len(f.PinPolicies()) != 0 || c.Stats().Blocks != 1 {
		t.Fatal("commands did not manage live policy/cache")
	}
}

func TestCacheCLIRefusesOfflineFallbackWhenStorageHasAnOwner(t *testing.T) {
	cfg, p := uploadCLIConfig(t)
	j, _ := uploadCLIJournal(t, cfg)
	defer j.Close()
	for _, action := range []string{"pin", "unpin", "warm", "cache"} {
		args := []string{"/", "--config", p}
		if action == "cache" {
			args[0] = "gc"
		}
		err := runCache(context.Background(), action, args, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "owned by another process") {
			t.Fatalf("%s: %v", action, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.Cache.Dir, "meta.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("opened metadata before ownership check")
	}
}

func TestOfflineCacheCLIPersistsPinsWithoutStartingUploads(t *testing.T) {
	var downloads, unexpected atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "PROPFIND":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			w.Write([]byte(`<d:multistatus xmlns:d="DAV:"><d:response><d:href>/a</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>5</d:getcontentlength><d:getetag>"v1"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`))
		case "GET":
			downloads.Add(1)
			w.Header().Set("ETag", `"v1"`)
			http.ServeContent(w, r, "a", time.Time{}, bytes.NewReader([]byte("hello")))
		default:
			unexpected.Add(1)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	cfg, p := uploadCLIConfig(t)
	cfg.Remotes["ali"] = config.Remote{Type: "webdav", Extra: map[string]any{"url": srv.URL}}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	j, u := uploadCLIJournal(t, cfg)
	j.Close()
	ctx := context.Background()
	for _, tc := range []struct {
		action string
		args   []string
	}{{"pin", []string{"/a"}}, {"cache", []string{"pins"}}, {"unpin", []string{"/a"}}} {
		var out bytes.Buffer
		if err := runCache(ctx, tc.action, append(tc.args, "--config", p), &out); err != nil {
			t.Fatalf("%s: %v", tc.action, err)
		}
		if tc.action == "cache" && !strings.Contains(out.String(), "/a") {
			t.Fatal("pin not restored across commands")
		}
	}
	if downloads.Load() != 1 || unexpected.Load() != 0 {
		t.Fatalf("downloads=%d unexpected/upload=%d", downloads.Load(), unexpected.Load())
	}
	ro, err := journal.OpenReadOnly(filepath.Join(cfg.Cache.Dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	row, err := ro.Get(ctx, u.ID)
	if err != nil || row.State != journal.StatePending {
		t.Fatalf("offline cache command changed upload: %+v %v", row, err)
	}
}
