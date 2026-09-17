package control

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
)

// TestFsRenderStripsScripts: a Markdown file renders to HTML in which no
// tag from the source survives; a text file comes back escaped as text;
// an image points the page at /fs/raw, which streams it inline with a
// sandboxing policy and never as HTML; a directory and an escape are
// refused; the console link names the file.
func TestFsRenderStripsScripts(t *testing.T) {
	f, fake := fsControl(t)
	fake.Seed("/docs/report.md", []byte("# Q3 <script>alert(1)</script>\n\nSee [the plan](https://plan.example) and `x`.\n"))
	fake.Seed("/docs/notes.txt", []byte("<b>plain</b>\n"))
	fake.Seed("/docs/chart.png", []byte("\x89PNG\r\n\x1a\nfake"))
	fake.Seed("/docs/page.svg", []byte("<svg onload=alert(1)></svg>"))
	cfg := config.Default()
	cfg.Control.Metrics = "0.0.0.0:9101"
	cfg.Control.UI = true
	f.coll.PublishConfigView(&cfg)
	if _, err := f.coll.FS.ReadDirPath(context.Background(), "/docs"); err != nil {
		t.Fatal(err)
	}
	s := NewServer(f.coll)
	md := decode[RenderResponse](t, call(t, s, "GET", "/fs/render?path=/docs/report.md", ""))
	if md.Kind != "markdown" || !strings.Contains(md.HTML, "&lt;script&gt;") || strings.Contains(md.HTML, "<script>") || !strings.Contains(md.HTML, `href="https://plan.example"`) {
		t.Fatalf("markdown: %+v", md)
	}
	if md.ConsoleURL != "http://127.0.0.1:9101/#/fs/docs/report.md" || !md.Share {
		t.Fatalf("console link / share: %+v", md)
	}
	txt := decode[RenderResponse](t, call(t, s, "GET", "/fs/render?path=/docs/notes.txt", ""))
	if txt.Kind != "text" || txt.Text != "<b>plain</b>\n" || txt.HTML != "" {
		t.Fatalf("text: %+v", txt)
	}
	img := decode[RenderResponse](t, call(t, s, "GET", "/fs/render?path=/docs/chart.png", ""))
	if img.Kind != "image" || img.Raw != "/fs/raw?path=/docs/chart.png" || img.Mime != "image/png" {
		t.Fatalf("image: %+v", img)
	}
	raw := call(t, s, "GET", "/fs/raw?path=/docs/chart.png", "")
	if raw.Code != 200 || raw.Header().Get("Content-Type") != "image/png" || raw.Header().Get("Content-Disposition") != "inline" || !strings.Contains(raw.Header().Get("Content-Security-Policy"), "sandbox") || raw.Body.Len() != 12 {
		t.Fatalf("raw png: %d %v", raw.Code, raw.Header())
	}
	svg := call(t, s, "GET", "/fs/raw?path=/docs/page.svg", "")
	if svg.Code != 200 || svg.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("an svg was served as a scriptable type: %v", svg.Header())
	}
	if w := call(t, s, "GET", "/fs/render?path=/docs", ""); w.Code != 400 {
		t.Fatalf("directory: %d", w.Code)
	}
	if w := call(t, s, "GET", "/fs/render?path=..%2F..", ""); w.Code == 200 {
		t.Fatal("an unclean path was accepted")
	}
	for _, target := range []string{"/fs/render", "/fs/raw"} {
		if w := call(t, s, "POST", target, `{}`); w.Code != 405 {
			t.Fatalf("POST %s: %d", target, w.Code)
		}
	}
}

// TestShareRouteFollowsThePolicy: POST /share needs the typed confirmation,
// answers 409 with a code for a file the cache does not hold (and costs
// no download), and shares a cached file with one CreateShare; the
// response carries the console link beside the public one.
func TestShareRouteFollowsThePolicy(t *testing.T) {
	f, fake := fsControl(t)
	cfg := config.Default()
	cfg.Control.Metrics = "127.0.0.1:9101"
	cfg.Control.UI = true
	f.coll.PublishConfigView(&cfg)
	ctx := context.Background()
	if _, err := f.coll.FS.ReadDirPath(ctx, "/docs"); err != nil {
		t.Fatal(err)
	}
	s := NewServer(f.coll)
	if w := call(t, s, "POST", "/share", `{"path":"/docs/b"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "confirm=true") {
		t.Fatalf("without confirm: %d %s", w.Code, w.Body.String())
	}
	reads := fake.Calls("ReadRange")
	w := call(t, s, "POST", "/share", `{"path":"/docs/b","confirm":true}`)
	var refusal map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &refusal)
	if w.Code != 409 || refusal["code"] != "not_cached" {
		t.Fatalf("uncached: %d %s", w.Code, w.Body.String())
	}
	if fake.Calls("ReadRange") != reads || fake.Calls("CreateShare") != 0 {
		t.Fatal("a refused share downloaded or shared")
	}
	if err := f.coll.FS.Prefetch(ctx, "/docs/b"); err != nil {
		t.Fatal(err)
	}
	res := decode[ShareResponse](t, call(t, s, "POST", "/share", `{"path":"/docs/b","confirm":true,"expires_hours":24}`))
	if res.URL == "" || res.ConsoleURL != "http://127.0.0.1:9101/#/fs/docs/b" || fake.Calls("CreateShare") != 1 {
		t.Fatalf("share: %+v (CreateShare %d)", res, fake.Calls("CreateShare"))
	}
	if w := call(t, s, "GET", "/share", ""); w.Code != 405 {
		t.Fatalf("GET: %d", w.Code)
	}
}

// TestRenderTokenIsNotProviderCredential: a render link's token is
// random, unrelated to any credential the daemon holds, good for one
// open and gone after its TTL; the LAN page renders the same safe HTML,
// serves nothing without a token, and the console refuses to mint links
// while the service is off.
func TestRenderTokenIsNotProviderCredential(t *testing.T) {
	f, fake := fsControl(t)
	fake.Seed("/docs/report.md", []byte("# Q3 <script>x</script>\n\nfine\n"))
	ctx := context.Background()
	if _, err := f.coll.FS.ReadDirPath(ctx, "/docs"); err != nil {
		t.Fatal(err)
	}
	s := NewServer(f.coll)
	if w := call(t, s, "POST", "/share/render-link", `{"path":"/docs/report.md"}`); w.Code != 409 {
		t.Fatalf("mint with the service off: %d %s", w.Code, w.Body.String())
	}
	links := NewRenderLinks(time.Hour)
	now := time.Now()
	links.now = func() time.Time { return now }
	f.coll.RenderLinks, f.coll.RenderBase = links, "http://192.168.1.10:9102"
	s = NewServer(f.coll)
	link := decode[RenderLinkResponse](t, call(t, s, "POST", "/share/render-link", `{"path":"/docs/report.md"}`))
	if !strings.HasPrefix(link.URL, "http://192.168.1.10:9102/r/") || !link.Once {
		t.Fatalf("link: %+v", link)
	}
	tok := strings.TrimPrefix(link.URL, "http://192.168.1.10:9102/r/")
	if len(tok) != 32 || strings.HasPrefix(tok, "cfs") || strings.Contains(tok, "token") {
		t.Fatalf("token shape: %q", tok)
	}
	if w := call(t, s, "POST", "/share/render-link", `{"path":"/docs"}`); w.Code != 400 {
		t.Fatalf("directory: %d", w.Code)
	}
	svc := NewRenderService(links, f.coll.FS).Handler()
	get := func(target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		svc.ServeHTTP(w, httptest.NewRequest("GET", "http://192.168.1.10:9102"+target, nil))
		return w
	}
	page := get("/r/" + tok)
	if page.Code != 200 || !strings.Contains(page.Header().Get("Content-Type"), "text/html") || !strings.Contains(page.Body.String(), "&lt;script&gt;") || strings.Contains(page.Body.String(), "<script>") {
		t.Fatalf("page: %d %v %s", page.Code, page.Header(), page.Body.String())
	}
	if again := get("/r/" + tok); again.Code != 410 {
		t.Fatalf("a second open: %d", again.Code)
	}
	if w := get("/r/"); w.Code != 404 {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := get("/"); w.Code != 404 {
		t.Fatalf("root: %d", w.Code)
	}
	expired, _ := links.Mint("/docs/report.md")
	now = now.Add(2 * time.Hour)
	if w := get("/r/" + expired); w.Code != 410 {
		t.Fatalf("expired: %d", w.Code)
	}
}
