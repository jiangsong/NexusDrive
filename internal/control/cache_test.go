package control

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

func cacheControl(t *testing.T) (*fixture, *fakeprovider.Fake) {
	t.Helper()
	f := newFixture(t)
	p := fakeprovider.New("ali")
	p.Seed("/docs/a", []byte("cached content"))
	fs, err := vfs.New(vfs.Options{Meta: f.meta, Cache: f.cache, Mounts: []vfs.Mount{{Prefix: "/", Remote: "ali", RootID: fakeprovider.RootID, Provider: p}}})
	if err != nil {
		t.Fatal(err)
	}
	f.coll.FS = fs
	t.Cleanup(func() { fs.Close(); f.cache.Close() })
	return f, p
}

func TestCacheControlManagesTheLiveCache(t *testing.T) {
	f, p := cacheControl(t)
	ctx := context.Background()
	socket := socketPath(t)
	srv, err := NewServer(f.coll).Start(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	for _, q := range []CacheRequest{{Action: "warm", Path: "/docs", Depth: 0}, {Action: "pin", Path: "/docs"}, {Action: "stats"}, {Action: "pins"}} {
		out, online, err := CallCache(ctx, socket, "", q)
		if err != nil || !online {
			t.Fatalf("%s: %+v %v %v", q.Action, out, online, err)
		}
		if q.Action == "warm" && (out.Directories != 1 || p.ReadBytes() != 0) {
			t.Fatal("warm downloaded content or did not list directory")
		}
		if q.Action == "stats" && (out.Stats.PinnedBlocks != 1 || len(out.Pins) != 1) {
			t.Fatalf("live pin missing: %+v", out)
		}
	}
	before := p.TotalCalls()
	out, online, err := CallCache(ctx, socket, "", CacheRequest{Action: "unpin", Path: "/docs"})
	if err != nil || !online || len(out.Pins) != 0 || out.Stats.PinnedBlocks != 0 || out.Stats.Blocks != 1 {
		t.Fatalf("unpin: %+v %v %v", out, online, err)
	}
	if p.TotalCalls() != before {
		t.Fatal("unpin called provider")
	}
	if _, _, err := CallCache(ctx, socket, "", CacheRequest{Action: "gc"}); err != nil {
		t.Fatal(err)
	}
}

func TestCacheControlRejectsBrowserAndActionOverride(t *testing.T) {
	f, _ := cacheControl(t)
	s := NewServer(f.coll)
	for _, tc := range []struct {
		name, method, route, body, origin, header string
		status                                    int
	}{
		{"origin", "POST", "/cache/pin", `{"path":"/docs"}`, "https://bad.invalid", "1", 403},
		{"header", "POST", "/cache/pin", `{"path":"/docs"}`, "", "", 403},
		{"get", "GET", "/cache/pin", "", "", "", 405},
		{"override", "POST", "/cache/unpin", `{"path":"/docs","action":"pin"}`, "", "1", 400},
		{"trailing", "POST", "/cache/pin", `{"path":"/docs"}{}`, "", "1", 400},
		{"relative", "POST", "/cache/pin", `{"path":"docs"}`, "", "1", 400},
		{"depth", "POST", "/cache/pin", `{"path":"/docs","depth":-1}`, "", "1", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://cloudfs"+tc.route, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-CloudFS-Control", tc.header)
			r.Header.Set("Origin", tc.origin)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
		})
	}
	if len(f.coll.FS.PinPolicies()) != 0 {
		t.Fatal("rejected request changed pins")
	}
}

func TestUnboundedWarmRequiresConfirmAndAllUsesTheCrawler(t *testing.T) {
	f, p := cacheControl(t)
	p.Seed("/docs/deep/x", []byte("x"))
	s := NewServer(f.coll)
	if w := call(t, s, "POST", "/cache/warm", `{"path":"/","depth":-1}`); w.Code != 400 || !strings.Contains(w.Body.String(), "confirm=true") {
		t.Fatalf("unconfirmed whole-tree warm: %d %s", w.Code, w.Body)
	}
	if w := call(t, s, "POST", "/cache/warm", `{"path":"/docs","depth":-1,"all":true,"confirm":true}`); w.Code != 400 {
		t.Fatalf("all=true below the root: %d %s", w.Code, w.Body)
	}
	out := decode[CacheResponse](t, call(t, s, "POST", "/cache/warm", `{"path":"/","depth":-1,"all":true,"confirm":true}`))
	if out.Crawl == nil || out.Queued || out.Crawl.Listed != 3 || out.Directories != 3 || p.Calls("List") != 3 {
		t.Fatalf("all=true did not run the crawler over /, /docs and /docs/deep: %+v, %d List calls", out, p.Calls("List"))
	}
	cov, _ := f.coll.FS.Meta().Coverage(context.Background())
	if cov.Listed != cov.Known {
		t.Fatalf("crawler left directories unlisted: %+v", cov)
	}
	if w := call(t, s, "POST", "/cache/warm", `{"path":"/docs","depth":1}`); w.Code != 200 {
		t.Fatalf("a bounded warm needs no confirmation: %d %s", w.Code, w.Body)
	}
	st := f.coll.Collect(context.Background(), "en")
	if st.Coverage != cov || st.Meta.LastCrawl.IsZero() || st.Crawl.Listed != out.Crawl.Listed {
		t.Fatalf("status does not carry the crawl: coverage %+v, meta %+v, crawl %+v", st.Coverage, st.Meta, st.Crawl)
	}
	// With a manager running, the pass is handed over and the request
	// answers with the running state instead of holding the connection.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.coll.FS.StartCrawl(ctx, vfs.CrawlOptions{})
	defer f.coll.FS.StopCrawl()
	out = decode[CacheResponse](t, call(t, s, "POST", "/cache/warm", `{"path":"/","depth":-1,"all":true,"confirm":true}`))
	if out.Crawl == nil || !out.Queued || out.Directories != 0 {
		t.Fatalf("a kicked pass must answer with progress only: %+v", out)
	}
}
