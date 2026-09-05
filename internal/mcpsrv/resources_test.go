package mcpsrv

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/vfs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func readResourceTest(t *testing.T, e *env, uri string) *mcp.ResourceContents {
	t.Helper()
	r, err := e.session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if err != nil || len(r.Contents) != 1 {
		t.Fatalf("read %q: %+v %v", uri, r, err)
	}
	if r.Contents[0].URI != uri {
		t.Fatalf("resource identity changed: %+v", r.Contents[0])
	}
	return r.Contents[0]
}

func TestResourcesRegisterOnlyAllowedRootsAndPaginate(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work", "/docs", "/work"}, Limits: Limits{MaxEntries: 1}})
	ctx := context.Background()
	page, err := e.session.ListResources(ctx, nil)
	if err != nil || len(page.Resources) != 1 || page.NextCursor == "" {
		t.Fatalf("first resource page: %+v %v", page, err)
	}
	seen := map[string]bool{page.Resources[0].URI: true}
	page, err = e.session.ListResources(ctx, &mcp.ListResourcesParams{Cursor: page.NextCursor})
	if err != nil || len(page.Resources) != 1 || page.NextCursor != "" || seen[page.Resources[0].URI] {
		t.Fatalf("second resource page: %+v %v", page, err)
	}
	seen[page.Resources[0].URI] = true
	if !seen["cloudfs://ali/work"] || !seen["cloudfs://ali/docs"] || e.fake.TotalCalls() != 0 {
		t.Fatalf("leaked or fetched roots: %v calls=%d", seen, e.fake.TotalCalls())
	}
	templates, err := e.session.ListResourceTemplates(ctx, nil)
	if err != nil || len(templates.ResourceTemplates) != 1 {
		t.Fatalf("templates: %+v %v", templates, err)
	}
	caps := e.session.InitializeResult().Capabilities.Resources
	if caps == nil || !caps.Subscribe {
		t.Fatalf("wrong capability advertisement: %+v", caps)
	}
}

func TestResourceResponsesArePrivateAndImmediatelyRevalidated(t *testing.T) {
	e := newEnv(t, Options{})
	ctx := context.Background()
	e.fake.Seed("file", []byte("old"))
	list, err := e.session.ListResources(ctx, nil)
	if err != nil || list.CacheScope != "private" || list.TTLMs != 0 {
		t.Fatalf("resource roots may be shared or cached: %+v %v", list, err)
	}
	templates, err := e.session.ListResourceTemplates(ctx, nil)
	if err != nil || templates.CacheScope != "private" || templates.TTLMs != 0 {
		t.Fatalf("templates may be shared or cached: %+v %v", templates, err)
	}
	for _, body := range []string{"old", "new"} {
		if body == "new" {
			if _, err := e.fs.WriteFile(ctx, "/file", []byte(body), false); err != nil {
				t.Fatal(err)
			}
		}
		r, err := e.session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "cloudfs://ali/file"})
		if err != nil || len(r.Contents) != 1 || r.Contents[0].Text != body || r.CacheScope != "private" || r.TTLMs != 0 {
			t.Fatalf("resource privacy or freshness: %+v %v", r, err)
		}
	}
}

func TestResourcesReadTextBinaryEmptyAndRangesThroughVFS(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}, ReadOnly: true, Limits: Limits{MaxBytes: 8, MaxRangeBytes: 4}})
	e.fake.Seed("work/text", []byte("hello"))
	e.fake.Seed("work/binary", []byte{0, 255, 128, 1})
	e.fake.Seed("work/empty", []byte{})
	e.fake.Seed("work/large", []byte("0123456789abcdef"))
	text := readResourceTest(t, e, "cloudfs://ali/work/text")
	if text.Text != "hello" || text.MIMEType != "text/plain; charset=utf-8" || text.Meta["cloudfs/truncated"] != false {
		t.Fatalf("text response: %+v", text)
	}
	before := e.fake.TotalCalls()
	readResourceTest(t, e, "cloudfs://ali/work/text")
	if e.fake.TotalCalls() != before {
		t.Fatal("resource bypassed VFS cache")
	}
	binary := readResourceTest(t, e, "cloudfs://ali/work/binary")
	if !bytes.Equal(binary.Blob, []byte{0, 255, 128, 1}) || binary.Text != "" {
		t.Fatalf("binary encoding changed bytes: %+v", binary)
	}
	empty := readResourceTest(t, e, "cloudfs://ali/work/empty")
	if empty.Blob == nil || len(empty.Blob) != 0 || empty.Meta["cloudfs/truncated"] != false {
		t.Fatalf("empty resource did not include blob on wire: %+v", empty)
	}
	uri := "cloudfs://ali/work/large"
	var all []byte
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("resource continuation made no progress")
		}
		c := readResourceTest(t, e, uri)
		body := c.Blob
		if c.Text != "" {
			body = []byte(c.Text)
		}
		cap := 8
		if pages > 0 {
			cap = 4
		}
		if len(body) > cap {
			t.Fatalf("response exceeded byte limit: %d", len(body))
		}
		all = append(all, body...)
		if c.Meta["cloudfs/truncated"] == false {
			break
		}
		uri, _ = c.Meta["cloudfs/next_uri"].(string)
		if uri == "" {
			t.Fatal("truncated response has no continuation")
		}
	}
	if string(all) != "0123456789abcdef" {
		t.Fatalf("paging changed bytes: %q", all)
	}
	for _, query := range []string{"offset=9223372036854775807&length=9223372036854775807", "offset=16", "offset=200"} {
		c := readResourceTest(t, e, "cloudfs://ali/work/large?"+query)
		if len(c.Blob) != 0 || c.Meta["cloudfs/truncated"] != false {
			t.Fatalf("EOF or range overflow: %+v", c)
		}
	}
	if res := e.call(t, "write_file", writeInput{Path: "/work/text", Content: "denied"}, nil); !res.IsError {
		t.Fatal("resource support weakened read-only tools")
	}
}

func TestResourcesValidateBeforeProviderAccess(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	for _, uri := range []string{
		"file:///work/a", "cloudfs://wrong/work/a", "cloudfs://ali/secret/a", "cloudfs://ali/work/../secret/a",
		"cloudfs://ali/work/%2e%2e/secret/a", "cloudfs://ali/work//a", "cloudfs://ali/work/a/", "cloudfs://ali/work/%00",
		"cloudfs://ali/work/%5ca", "cloudfs://ali/work/%ff", "cloudfs://u:p@ali/work/a", "cloudfs://ali:9/work/a",
		"cloudfs://ali/work/a#fragment", "cloudfs://ali/work/a?", "cloudfs://ali/work/a?offset=-1", "cloudfs://ali/work/a?offset=01",
		"cloudfs://ali/work/a?offset=9223372036854775808", "cloudfs://ali/work/a?length=0", "cloudfs://ali/work/a?length=1&length=2",
		"cloudfs://ali/work/a?offset=", "cloudfs://ali/work/a?unknown=1", "cloudfs://ali/work/a?cursor=Li4", "cloudfs://ali/work/a?cursor=YQ&offset=0",
		"cloudfs://ali/work/a?cursor=%", "cloudfs://ali/work/a?cursor=YQ==", "cloudfs://ali/work/a?offset=1;length=2",
	} {
		t.Run(uri, func(t *testing.T) {
			if _, err := e.session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri}); err == nil {
				t.Fatal("accepted invalid or forbidden URI")
			}
			if e.fake.TotalCalls() != 0 {
				t.Fatal("rejected resource contacted provider")
			}
		})
	}
}

func TestResourcesDirectoryPagesContainUsableEscapedURIs(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}, Limits: Limits{MaxEntries: 2, MaxBytes: 1024}})
	names := []string{"a space", "b#hash", "c?query", "d%literal", "中文"}
	for _, name := range names {
		e.fake.Seed("work/"+name, []byte(name))
	}
	e.fake.Seed("secret/hidden", []byte("secret"))
	uri := "cloudfs://ali/work"
	seen := map[string]bool{}
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("directory cursor did not advance")
		}
		c := readResourceTest(t, e, uri)
		var dir directoryResource
		if err := json.Unmarshal([]byte(c.Text), &dir); err != nil || len(dir.Entries) > 2 || len(c.Text) > 1024 {
			t.Fatalf("directory page: %+v %v", dir, err)
		}
		for _, entry := range dir.Entries {
			if seen[entry.Name] {
				t.Fatalf("duplicate directory entry: %s", entry.Name)
			}
			seen[entry.Name] = true
			if got := readResourceTest(t, e, entry.URI); got.Text != entry.Name {
				t.Fatalf("entry URI lost escaping: %+v %+v", entry, got)
			}
		}
		if !dir.Truncated {
			break
		}
		uri = dir.NextURI
	}
	if len(seen) != len(names) {
		t.Fatalf("missing entries: %v", seen)
	}
	if _, err := e.session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "cloudfs://ali/work?offset=0"}); err == nil {
		t.Fatal("directory accepted file range")
	}
	if _, err := e.session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "cloudfs://ali/work/a%20space?cursor=YQ"}); err == nil {
		t.Fatal("file accepted directory cursor")
	}
}

func TestResourcesByteBudgetAndSplitUTF8RemainExact(t *testing.T) {
	e := newEnv(t, Options{Limits: Limits{MaxBytes: 2, MaxRangeBytes: 2}})
	e.fake.Seed("unicode", []byte("中文"))
	var got []byte
	uri := "cloudfs://ali/unicode"
	for range 3 {
		c := readResourceTest(t, e, uri)
		if len(c.Blob) != 2 {
			t.Fatalf("split UTF-8 must be a byte-exact blob: %+v", c)
		}
		got = append(got, c.Blob...)
		uri, _ = c.Meta["cloudfs/next_uri"].(string)
	}
	if string(got) != "中文" || uri != "" {
		t.Fatalf("split UTF-8 pagination: %q next=%q", got, uri)
	}
	if _, err := e.session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "cloudfs://ali/"}); err == nil {
		t.Fatal("tiny directory budget was ignored")
	}
}

func TestResourceRoutingHandlesAliasesShadowingAndNonHostRemoteNames(t *testing.T) {
	e := newEnv(t, Options{})
	fs, err := vfs.New(vfs.Options{Meta: e.fs.Meta(), Cache: e.fs.Cache(), Mounts: []vfs.Mount{
		{Prefix: "/one", Remote: "ali", RootID: "root", Provider: e.fake},
		{Prefix: "/two", Remote: "ali", RootID: "root", Provider: e.fake},
		{Prefix: "/one/private", Remote: "other", RootID: "root", Provider: e.fake},
		{Prefix: "/unicode", Remote: "云盘 name:@", RootID: "root", Provider: e.fake},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	s, err := New(Options{FS: fs, Allow: []string{"/one", "/two", "/unicode"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ remote, path string }{{"ali", "/one/a"}, {"ali", "/two/a"}, {"other", "/one/private/a"}, {"云盘 name:@", "/unicode/a"}} {
		uri := resourceURI(tc.remote, tc.path)
		q, err := s.parseResource(uri)
		if err != nil || q.remote != tc.remote || q.path != tc.path {
			t.Fatalf("ambiguous resource mapping: %+v %+v %v", tc, q, err)
		}
		if _, err := url.Parse(uri); err != nil {
			t.Fatalf("SDK-incompatible URI: %q %v", uri, err)
		}
	}
	if _, err := s.parseResource("cloudfs://ali/one/private/a"); err == nil {
		t.Fatal("outer remote bypassed shadowing mount")
	}
	if resourceAuthority("r~616c69") == resourceAuthority("ali") || resourceAuthority("name with space") == resourceAuthority("r~6e616d652077697468207370616365") {
		t.Fatal("authority encoding collision")
	}
}

func TestResourceErrorsDoNotExposeBackendDetails(t *testing.T) {
	secret := "https://private.invalid/?token=secret /private/cache/path"
	if got := resourceReadError("cloudfs://ali/a", errors.New(secret)).Error(); strings.Contains(got, "secret") || strings.Contains(got, "private") {
		t.Fatalf("resource leaked underlying error: %s", got)
	}
	if !errors.Is(resourceReadError("cloudfs://ali/a", context.Canceled), context.Canceled) {
		t.Fatal("resource swallowed cancellation")
	}
	// Verify literal %2e%2e is decoded once, not interpreted a second time.
	e := newEnv(t, Options{Allow: []string{"/work"}})
	e.fake.Seed("work/%2e%2e", []byte("literal filename"))
	c := readResourceTest(t, e, "cloudfs://ali/work/%252e%252e")
	if c.Text != "literal filename" {
		t.Fatal("resource double-decoded path")
	}
	cursor := base64.RawURLEncoding.EncodeToString([]byte("%2e%2e"))
	readResourceTest(t, e, "cloudfs://ali/work?cursor="+cursor)
}

func TestResourceDirectoryByteBudgetPaginatesWithoutLoss(t *testing.T) {
	e := newEnv(t, Options{Limits: Limits{MaxEntries: 100, MaxBytes: 300}})
	for i := range 10 {
		e.fake.Seed(fmt.Sprintf("directory/file-%02d", i), []byte("x"))
	}
	uri := "cloudfs://ali/directory"
	seen := map[string]bool{}
	for pages := 0; ; pages++ {
		if pages >= 10 {
			t.Fatal("directory byte-budget cursor stalled")
		}
		c := readResourceTest(t, e, uri)
		var dir directoryResource
		if err := json.Unmarshal([]byte(c.Text), &dir); err != nil || len(c.Text) > 300 || len(dir.Entries) == 0 {
			t.Fatalf("invalid bounded directory page: %q %v", c.Text, err)
		}
		if pages == 0 && !dir.Truncated {
			t.Fatal("byte budget did not force pagination")
		}
		for _, entry := range dir.Entries {
			if seen[entry.Name] {
				t.Fatal("repeated directory entry")
			}
			seen[entry.Name] = true
		}
		if !dir.Truncated {
			break
		}
		uri = dir.NextURI
	}
	if len(seen) != 10 {
		t.Fatalf("byte budget lost entries: %v", seen)
	}
}

type resourceAuthTransport struct{ base http.RoundTripper }

func (r resourceAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer resource-test-token")
	return r.base.RoundTrip(clone)
}

func TestResourceStreamableHTTPUsesExistingAuthenticationAndAllowlist(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("work/file", []byte("through HTTP"))
	s, err := New(Options{FS: e.fs, Allow: []string{"/work"}, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	handler := newMCPHTTPHandler(s)
	server := httptest.NewServer(requireBearer(handler, "resource-test-token"))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "http-resource-test", Version: "1"}, nil)
	if session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL, DisableStandaloneSSE: true, MaxRetries: -1}, nil); err == nil {
		session.Close()
		t.Fatal("unauthenticated resource client connected")
	}
	if e.fake.TotalCalls() != 0 {
		t.Fatal("unauthenticated client reached provider")
	}
	httpClient := &http.Client{Transport: resourceAuthTransport{base: server.Client().Transport}, Timeout: 5 * time.Second}
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: httpClient, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	r, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "cloudfs://ali/work/file"})
	if err != nil || len(r.Contents) != 1 || r.Contents[0].Text != "through HTTP" || r.CacheScope != "private" || r.TTLMs != 0 {
		t.Fatalf("HTTP read: %+v %v", r, err)
	}
	for _, uri := range []string{"cloudfs://ali/secret/file", "cloudfs://ali/work/../secret/file"} {
		// v1.7.0's client marks HTTP 4xx responses as connection failures.
		// Use a fresh connection for EACH refusal so a previously closed
		// connection cannot produce a false-positive authorization test.
		deniedSession, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: httpClient, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		before := e.fake.TotalCalls()
		_, err = deniedSession.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		deniedSession.Close()
		if err == nil {
			t.Fatal("HTTP resource bypassed allowlist")
		}
		if e.fake.TotalCalls() != before {
			t.Fatal("HTTP resource refusal accessed provider")
		}
	}
}
