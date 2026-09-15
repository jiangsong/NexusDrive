package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

// fsControl is cacheControl with a small tree to browse.
func fsControl(t *testing.T) (*fixture, *fakeprovider.Fake) {
	t.Helper()
	f, p := cacheControl(t)
	p.Seed("/docs/b", []byte("second"))
	p.Seed("/docs/sub/c", []byte("deeper"))
	p.Seed("/other", []byte("o"))
	return f, p
}

// call sends a same-origin request the way the embedded page does.
func call(t *testing.T, s *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "http://cloudfs"+target, nil)
	} else {
		r = httptest.NewRequest(method, "http://cloudfs"+target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		r.Header.Set("X-CloudFS-Control", "1")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	var out T
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	return out
}

// TestFSRoutesBrowseAndMutateTheSameTreeTheMountShows: the browser's view and
// the VFS's view are one view. Everything the page changes is visible through
// the FS afterwards, and every entry carries the local facts FUSE cannot show.
func TestFSRoutesBrowseAndMutateTheSameTreeTheMountShows(t *testing.T) {
	f, p := fsControl(t)
	s := NewServer(f.coll)
	ctx := context.Background()

	list := decode[FSListResponse](t, call(t, s, "GET", "/fs/list?path=/docs", ""))
	names := map[string]FSEntry{}
	for _, e := range list.Entries {
		names[e.Name] = e
	}
	if len(names) != 3 || !names["sub"].IsDir || names["a"].IsDir || names["a"].Path != "/docs/a" || list.Total != 3 {
		t.Fatalf("listing: %+v", list)
	}
	if names["a"].Cached != 0 {
		t.Fatalf("a file nobody read reports cached=%v", names["a"].Cached)
	}

	// Reading through the FS is what makes it cached; the listing says so.
	if _, err := f.coll.FS.ReadFileRange(ctx, "/docs/a", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.coll.FS.Pin(ctx, "/docs/sub"); err != nil {
		t.Fatal(err)
	}
	list = decode[FSListResponse](t, call(t, s, "GET", "/fs/list?path=/docs", ""))
	for _, e := range list.Entries {
		switch e.Name {
		case "a":
			if e.Cached != 1 {
				t.Fatalf("a read file reports cached=%v", e.Cached)
			}
		case "sub":
			if !e.Pinned {
				t.Fatal("a pinned directory is not reported pinned")
			}
		}
	}
	st := decode[FSEntry](t, call(t, s, "GET", "/fs/stat?path=/docs/sub/c", ""))
	if !st.Pinned || st.Path != "/docs/sub/c" || st.Size != 6 {
		t.Fatalf("stat under a pin: %+v", st)
	}

	// Paging: two entries per page, the cursor from one page continues it.
	page1 := decode[FSListResponse](t, call(t, s, "GET", "/fs/list?path=/docs&limit=2", ""))
	if len(page1.Entries) != 2 || page1.NextCursor == "" {
		t.Fatalf("first page: %+v", page1)
	}
	page2 := decode[FSListResponse](t, call(t, s, "GET", "/fs/list?path=/docs&limit=2&cursor="+page1.NextCursor, ""))
	if len(page2.Entries) != 1 || page2.NextCursor != "" || page2.Entries[0].Name == page1.Entries[1].Name {
		t.Fatalf("second page: %+v", page2)
	}

	// Preview hands back the bytes, capped, with a range.
	w := call(t, s, "GET", "/fs/preview?path=/docs/b&offset=1&length=3", "")
	if w.Code != 200 || w.Body.String() != "eco" || w.Header().Get("Content-Range") != "bytes 1-3/6" {
		t.Fatalf("preview: %d %q %q", w.Code, w.Body.String(), w.Header().Get("Content-Range"))
	}
	if w := call(t, s, "GET", "/fs/preview?path=/docs/b&length=9999999", ""); w.Code != 400 {
		t.Fatalf("an oversize preview was accepted: %d", w.Code)
	}

	// Mutations: mkdir is one level, rename moves, delete needs confirm.
	made := decode[FSEntry](t, call(t, s, "POST", "/fs/mkdir", `{"path":"/docs/new"}`))
	if !made.IsDir || made.Path != "/docs/new" {
		t.Fatalf("mkdir: %+v", made)
	}
	if w := call(t, s, "POST", "/fs/mkdir", `{"path":"/docs/missing/deep"}`); w.Code != 404 {
		t.Fatalf("mkdir through a missing parent: %d (must not create the chain)", w.Code)
	}
	moved := decode[FSEntry](t, call(t, s, "POST", "/fs/rename", `{"from":"/docs/b","to":"/docs/new/b2"}`))
	if moved.Path != "/docs/new/b2" || moved.Size != 6 {
		t.Fatalf("rename: %+v", moved)
	}
	if _, err := f.coll.FS.StatPath(ctx, "/docs/b"); err == nil {
		t.Fatal("renamed file still at its old path")
	}
	if w := call(t, s, "POST", "/fs/rename", `{"from":"/docs/new","to":"/docs/new/inside"}`); w.Code != 400 {
		t.Fatalf("moving a directory into itself: %d", w.Code)
	}
	if w := call(t, s, "POST", "/fs/delete", `{"path":"/docs/new/b2"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "confirm") {
		t.Fatalf("delete without confirm: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.coll.FS.StatPath(ctx, "/docs/new/b2"); err != nil {
		t.Fatal("a refused delete removed the file")
	}
	if w := call(t, s, "POST", "/fs/delete", `{"path":"/docs/new","confirm":true}`); w.Code != 409 {
		t.Fatalf("deleting a non-empty directory without recursive: %d %s", w.Code, w.Body.String())
	}
	if w := call(t, s, "POST", "/fs/delete", `{"path":"/docs/new","recursive":true,"confirm":true}`); w.Code != 200 {
		t.Fatalf("recursive delete: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.coll.FS.StatPath(ctx, "/docs/new"); err == nil {
		t.Fatal("deleted directory still resolves")
	}
	if w := call(t, s, "POST", "/fs/delete", `{"path":"/","confirm":true}`); w.Code != 400 {
		t.Fatalf("deleting the root: %d", w.Code)
	}
	_ = p
}

// TestFSRoutesRefuseUnsafePaths: every path parameter is canonicalised by one
// function, and these are its rejections. The vectors match what the MCP
// tools refuse, so the two adapters agree on what a path may look like.
func TestFSRoutesRefuseUnsafePaths(t *testing.T) {
	f, _ := fsControl(t)
	s := NewServer(f.coll)
	for _, tc := range []struct{ name, target string }{
		{"relative", "/fs/list?path=docs"},
		{"empty", "/fs/stat?path="},
		{"nul", "/fs/stat?path=/docs%00"},
		{"control char", "/fs/stat?path=/docs%0a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := call(t, s, "GET", tc.target, ""); w.Code != 400 {
				t.Fatalf("got %d %s", w.Code, strings.TrimSpace(w.Body.String()))
			}
		})
	}
	// Traversal is cleaned, not rejected: the cleaned path is what is served,
	// and it cannot climb above the root.
	w := call(t, s, "GET", "/fs/list?path=/docs/../docs/../", "")
	if w.Code != 200 {
		t.Fatalf("cleaned traversal: %d %s", w.Code, w.Body.String())
	}
	if out := decode[FSListResponse](t, w); out.Path != "/" {
		t.Fatalf("cleaned to %q, want /", out.Path)
	}
	if w := call(t, s, "GET", "/fs/list?path=/docs&cursor=garbage", ""); w.Code != 400 {
		t.Fatalf("garbage cursor: %d", w.Code)
	}
	if w := call(t, s, "POST", "/fs/mkdir", `{"path":"/x","extra":1}`); w.Code != 400 {
		t.Fatalf("unknown field: %d", w.Code)
	}
	if w := call(t, s, "GET", "/fs/list?path=/nowhere", ""); w.Code != 404 {
		t.Fatalf("missing directory: %d", w.Code)
	}
}

// TestFSRoutesWithoutAFilesystemSaySo: an MCP-only daemon that has no VFS
// answers 503, not a panic.
func TestFSRoutesWithoutAFilesystemSaySo(t *testing.T) {
	f := newFixture(t)
	s := NewServer(f.coll)
	if w := call(t, s, "GET", "/fs/list?path=/", ""); w.Code != 503 {
		t.Fatalf("got %d", w.Code)
	}
	if w := call(t, s, "GET", "/search?q=a", ""); w.Code != 503 {
		t.Fatalf("search got %d", w.Code)
	}
}

// TestSearchReportsCompleteness: the index has a work budget, and the page has
// to be told when it was hit rather than take a partial list for the whole.
func TestSearchReportsCompleteness(t *testing.T) {
	f, _ := fsControl(t)
	s := NewServer(f.coll)
	if _, err := f.coll.FS.ReadDirPath(context.Background(), "/docs/sub"); err != nil {
		t.Fatal(err)
	}
	if err := f.coll.FS.Meta().FlushIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := decode[SearchResponse](t, call(t, s, "GET", "/search?q=c&path=/docs", ""))
	found := false
	for _, hit := range out.Results {
		if hit.Path == "/docs/sub/c" {
			found = true
		}
		if strings.Contains(hit.Path, fakeprovider.RootID) {
			t.Fatalf("a search hit names a provider id: %+v", hit)
		}
	}
	if !found || !out.Complete {
		t.Fatalf("search: %+v", out)
	}
	if w := call(t, s, "GET", "/search?q=", ""); w.Code != 400 {
		t.Fatalf("empty query: %d", w.Code)
	}
	if w := call(t, s, "GET", "/search?q=a&limit=100000", ""); w.Code != 400 {
		t.Fatalf("unbounded limit: %d", w.Code)
	}
}

// TestDownloadURLIsTheOneDeliberateSignedURL: the link endpoint returns what
// the provider signs, no-store, and nothing else on the server does.
func TestDownloadURLIsTheOneDeliberateSignedURL(t *testing.T) {
	f, _ := fsControl(t)
	s := NewServer(f.coll)
	w := call(t, s, "GET", "/fs/download-url?path=/docs/a", "")
	if w.Code == 200 {
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("a signed link was served cacheable")
		}
		out := decode[FSLinkResponse](t, w)
		if out.URL == "" {
			t.Fatalf("empty link: %+v", out)
		}
	} else if w.Code != 501 && w.Code != 500 {
		t.Fatalf("download-url: %d %s", w.Code, w.Body.String())
	}
	if w := call(t, s, "GET", "/fs/download-url?path=/docs", ""); w.Code == 200 {
		t.Fatal("a directory has no download link")
	}
	// The listing and stat replies never carry one.
	list := call(t, s, "GET", "/fs/list?path=/docs", "")
	if strings.Contains(list.Body.String(), "http://") || strings.Contains(list.Body.String(), "\"url\"") {
		t.Fatalf("listing carries a link: %s", list.Body.String())
	}
	_ = vfs.ErrNotFound
}

// TestSearchCarriesCoverageAndRowFacts: every row carries what a results
// table shows, the answer says how much of the tree it could see, and the
// filters are validated at the door.
func TestSearchCarriesCoverageAndRowFacts(t *testing.T) {
	f, _ := fsControl(t)
	s := NewServer(f.coll)
	ctx := context.Background()
	if _, err := f.coll.FS.ReadDirPath(ctx, "/docs"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.coll.FS.ReadFileRange(ctx, "/docs/b", 0, 6); err != nil { // one block: fully cached
		t.Fatal(err)
	}
	if err := f.coll.FS.Meta().FlushIndex(ctx); err != nil {
		t.Fatal(err)
	}
	out := decode[SearchResponse](t, call(t, s, "GET", "/search?q=b&sort=size", ""))
	if out.Coverage.Known == 0 || out.Coverage.Listed == 0 || out.Coverage.Listed > out.Coverage.Known || out.Coverage.Crawling {
		t.Fatalf("coverage: %+v", out.Coverage)
	}
	var hit *SearchHit
	for i := range out.Results {
		if out.Results[i].Path == "/docs/b" {
			hit = &out.Results[i]
		}
	}
	if hit == nil || hit.Size != 6 || hit.Kind != "file" || hit.MTime.IsZero() || !hit.Cached {
		t.Fatalf("thin hit: %+v", out.Results)
	}
	dirs := decode[SearchResponse](t, call(t, s, "GET", "/search?kind=dir", ""))
	if len(dirs.Results) < 2 {
		t.Fatalf("filter-only search: %+v", dirs)
	}
	for _, r := range dirs.Results {
		if r.Kind != "dir" || r.Cached {
			t.Fatalf("kind=dir returned %+v", r)
		}
	}
	cold := decode[SearchResponse](t, call(t, s, "GET", "/search?path=/docs&ext=&glob=*&min_size=6&max_size=6&after=2000-01-01", ""))
	if len(cold.Results) != 1 || cold.Results[0].Path != "/docs/b" {
		t.Fatalf("structured filters: %+v", cold.Results)
	}
	for _, bad := range []string{"/search", "/search?q=", "/search?q=a&sort=bogus", "/search?q=a&min_size=x", "/search?q=a&max_size=-1", "/search?ext=go&kind=link", "/search?q=size:lots", "/search?q=a&after=yesterday"} {
		if w := call(t, s, "GET", bad, ""); w.Code != 400 {
			t.Fatalf("%s: %d %s", bad, w.Code, w.Body.String())
		}
	}
}
