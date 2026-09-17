package webdavsrv

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/cache"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

// TestWebDAVPutIsAudited (docs/agent-first-design.md §6.1): a PUT and a
// DELETE through the WebDAV endpoint reach the change record with origin
// webdav — the adapter tags every request with tagOrigin, the real VFS
// reports the change under that name, the recorder writes the row — so
// the inspector's "last modified" names the WebDAV client rather than
// nobody.
func TestWebDAVPutIsAudited(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4096, MaxBytes: 1 << 20, FreeSpace: func(string) (int64, error) { return 1 << 30, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	p := fakeprovider.New("ali")
	p.Seed("/share/old.txt", []byte("old"))
	fsys, err := vfs.New(vfs.Options{Meta: store, Cache: ca, Mounts: []vfs.Mount{{Prefix: "/", Remote: "ali", RootID: fakeprovider.RootID, Provider: p}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	up, err := upload.New(upload.Options{
		Journal:   j,
		Providers: func(string) (provider.Provider, bool) { return p, true },
		Limiters:  ratelimit.NewRegistry(func(ratelimit.Key) ratelimit.Options { return ratelimit.Options{Rate: 16, MinRate: 1} }, ratelimit.BreakerOptions{Threshold: 3, Window: time.Minute, Cooldown: time.Hour}),
		Hooks:     fsys.UploadHooks(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fsys.SetWriteBackend(j, up)
	st, err := agent.Open(filepath.Join(dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go st.RunChangeRecorder(rctx, fsys)

	sctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	running, err := Start(sctx, Options{FS: fsys, Addr: "127.0.0.1:0", Prefix: "/dav", Root: "/share", Writable: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { running.Close() })
	c := &davClient{t: t, base: "http://" + running.Addr() + "/dav"}
	if code := c.status(http.MethodPut, "/new.txt", "hello from dav", nil); code != http.StatusCreated && code != http.StatusNoContent {
		t.Fatalf("PUT: %d", code)
	}
	if code := c.status(http.MethodDelete, "/old.txt", "", nil); code != http.StatusNoContent {
		t.Fatalf("DELETE: %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	byPath := map[string]agent.Change{}
	for time.Now().Before(deadline) {
		rows, _, _ := st.Changes(ctx, agent.ChangesQuery{Prefix: "/share"})
		byPath = map[string]agent.Change{}
		for _, r := range rows {
			byPath[r.Path] = r
		}
		if _, ok := byPath["/share/new.txt"]; ok {
			if _, ok := byPath["/share/old.txt"]; ok {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c, ok := byPath["/share/new.txt"]; !ok || c.Origin != "webdav" || !c.Reliable {
		t.Fatalf("the PUT is not on record as webdav: %+v", byPath)
	}
	if c, ok := byPath["/share/old.txt"]; !ok || c.Origin != "webdav" || c.Kind != "remove" {
		t.Fatalf("the DELETE is not on record as webdav: %+v", byPath)
	}
	last, ok, err := st.LastWriter(ctx, "/share/new.txt")
	if err != nil || !ok || last.Origin != "webdav" {
		t.Fatalf("last_writer: %+v %v %v", last, ok, err)
	}
}
