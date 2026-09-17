package mcpsrv

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type env struct {
	server  *Server
	fs      *vfs.FS
	fake    *fakeprovider.Fake
	up      *upload.Uploader
	j       *journal.Journal
	session *mcp.ClientSession
}

func newEnv(t *testing.T, opt Options) *env {
	t.Helper()
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })

	fake := fakeprovider.New("ali")
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca,
		AttrTTL: time.Minute, DefaultDirTTL: time.Minute, NegativeTTL: time.Second,
		Mounts: []vfs.Mount{{
			Prefix: "/", Remote: "ali", RootID: fakeprovider.RootID,
			Provider: fake, Mode: config.ModeWriteback, DirTTL: time.Minute,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })

	up, err := upload.New(upload.Options{
		Journal: j,
		Providers: func(remote string) (provider.Provider, bool) {
			if remote == "ali" {
				return fake, true
			}
			return nil, false
		},
		Policy: retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: time.Millisecond}, MaxAttempts: 2},
		Hooks:  fsys.UploadHooks(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fsys.SetWriteBackend(j, up)

	opt.FS = fsys
	srv, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	// Wire a real client to a real server over an in-memory transport, so the
	t.Cleanup(func() { srv.Close() })
	// tests exercise schema validation and JSON encoding, not just the funcs.
	clientT, serverT := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Run(ctx, serverT) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(); cancel() })

	return &env{server: srv, fs: fsys, fake: fake, up: up, j: j, session: session}
}

// call invokes a tool and decodes its structured output into out.
func (e *env) call(t *testing.T, name string, args any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := e.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: transport error: %v", name, err)
	}
	if out != nil && !res.IsError && res.StructuredContent != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("%s: marshal structured content: %v", name, err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: decode output: %v", name, err)
		}
	}
	return res
}

func errText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func (e *env) drain(t *testing.T) {
	t.Helper()
	if _, err := e.up.DrainAll(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestToolsAreRegistered(t *testing.T) {
	e := newEnv(t, Options{})
	res, err := e.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
	}
	want := []string{
		"list_directory", "directory_tree", "stat", "stat_many", "read_text", "read_range",
		"search", "cache_status", "list_roots", "get_download_url",
		"write_file", "edit_file", "create_directory", "move", "copy", "delete", "pin", "unpin",
		"list_copy_jobs", "get_copy_job", "retry_copy_job", "cancel_copy_job", "forget_copy_job",
		"list_uploads", "get_upload", "retry_upload", "cancel_upload", "resume_upload", "discard_upload", "flush_uploads",
		"share",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing tool %s", w)
		}
	}
	if len(res.Tools) != len(want) {
		t.Errorf("tool count = %d, want %d", len(res.Tools), len(want))
	}
}

func TestPinAndUnpinUseVFSAndRespectAllowlist(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	e.fake.Seed("work/a", []byte("local"))
	if res := e.call(t, "pin", pinInput{Path: "/work"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if len(e.fs.PinPolicies()) != 1 || e.fs.Cache().Stats().PinnedBlocks != 1 {
		t.Fatal("pin did not persist and protect")
	}
	for _, name := range []string{"pin", "unpin"} {
		if res := e.call(t, name, pinInput{Path: "/work/../secret"}, nil); !res.IsError {
			t.Fatal("allowlist bypass:", name)
		}
	}
	if res := e.call(t, "unpin", pinInput{Path: "/work"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if len(e.fs.PinPolicies()) != 0 || e.fs.Cache().Stats().PinnedBlocks != 0 || e.fs.Cache().Stats().Blocks != 1 {
		t.Fatal("unpin deleted data or kept the policy")
	}
}

func TestListDirectoryPaginates(t *testing.T) {
	e := newEnv(t, Options{Limits: Limits{MaxEntries: 3}})
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		e.fake.Seed("docs/"+n+".txt", []byte(n))
	}
	var out listOutput
	e.call(t, "list_directory", listInput{Path: "/docs"}, &out)
	if len(out.Entries) != 3 || !out.Truncated || out.Total != 7 {
		t.Fatalf("first page = %+v", out)
	}
	if out.Entries[0].Name != "a.txt" {
		t.Fatalf("entries = %+v", out.Entries)
	}
	seen := len(out.Entries)
	cursor := out.NextCursor
	for cursor != "" {
		var page listOutput
		e.call(t, "list_directory", listInput{Path: "/docs", Cursor: cursor}, &page)
		seen += len(page.Entries)
		cursor = page.NextCursor
	}
	if seen != 7 {
		t.Fatalf("paged through %d entries, want 7", seen)
	}
	// A bad cursor is reported, not ignored.
	res := e.call(t, "list_directory", listInput{Path: "/docs", Cursor: "junk"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "invalid cursor") {
		t.Fatalf("bad cursor = %v %q", res.IsError, errText(res))
	}
}

func TestReadTextHeadTailAndTruncation(t *testing.T) {
	e := newEnv(t, Options{Limits: Limits{MaxBytes: 20}})
	e.fake.Seed("poem.txt", []byte("one\ntwo\nthree\nfour\nfive\nsix\n"))

	var out readTextOutput
	e.call(t, "read_text", readTextInput{Path: "/poem.txt", Head: 2}, &out)
	if out.Content != "one\ntwo\n" {
		t.Fatalf("head = %q", out.Content)
	}
	e.call(t, "read_text", readTextInput{Path: "/poem.txt", Tail: 2}, &out)
	if !strings.HasSuffix(out.Content, "six\n") {
		t.Fatalf("tail = %q", out.Content)
	}
	// The byte cap truncates and reports where to continue.
	e.call(t, "read_text", readTextInput{Path: "/poem.txt"}, &out)
	if !out.Truncated || out.Bytes != 20 || out.NextOffset != 20 {
		t.Fatalf("truncated read = %+v", out)
	}
	var rest readTextOutput
	e.call(t, "read_text", readTextInput{Path: "/poem.txt", Offset: out.NextOffset}, &rest)
	if out.Content+rest.Content != "one\ntwo\nthree\nfour\nfive\nsix\n" {
		t.Fatalf("continuation = %q + %q", out.Content, rest.Content)
	}
}

func TestReadTextRejectsBinary(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("image.bin", []byte{0xff, 0xfe, 0x00, 0x01})
	res := e.call(t, "read_text", readTextInput{Path: "/image.bin"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "read_range") {
		t.Fatalf("binary read should point at read_range: %v %q", res.IsError, errText(res))
	}
}

func TestReadRange(t *testing.T) {
	e := newEnv(t, Options{})
	content := []byte("0123456789abcdef")
	e.fake.Seed("data.bin", content)
	var out readRangeOutput
	e.call(t, "read_range", readRangeInput{Path: "/data.bin", Offset: 4, Length: 6}, &out)
	got, err := base64.StdEncoding.DecodeString(out.Base64)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "456789" {
		t.Fatalf("range = %q", got)
	}
	if !out.Truncated || out.Size != 16 {
		t.Fatalf("out = %+v", out)
	}
	// The length cap applies.
	e2 := newEnv(t, Options{Limits: Limits{MaxRangeBytes: 4}})
	e2.fake.Seed("data.bin", content)
	e2.call(t, "read_range", readRangeInput{Path: "/data.bin", Offset: 0, Length: 100}, &out)
	if out.Bytes != 4 {
		t.Fatalf("range cap not applied: %d bytes", out.Bytes)
	}
}

func TestWriteAndReadBack(t *testing.T) {
	e := newEnv(t, Options{})
	var w writeOutput
	res := e.call(t, "write_file", writeInput{Path: "/notes.md", Content: "hello agent\n"}, &w)
	if res.IsError {
		t.Fatalf("write failed: %s", errText(res))
	}
	if w.State != "local" || w.Size != 12 {
		t.Fatalf("write output = %+v", w)
	}
	// Readable immediately, before the upload.
	var r readTextOutput
	e.call(t, "read_text", readTextInput{Path: "/notes.md"}, &r)
	if r.Content != "hello agent\n" {
		t.Fatalf("read back = %q", r.Content)
	}
	e.drain(t)
	var st statOutput
	e.call(t, "stat", statInput{Path: "/notes.md"}, &st)
	if st.State != "synced" || st.Size != 12 || st.Kind != "file" {
		t.Fatalf("stat after upload = %+v", st)
	}

	// Append.
	e.call(t, "write_file", writeInput{Path: "/notes.md", Content: "second\n", Mode: "append"}, &w)
	e.drain(t)
	e.call(t, "read_text", readTextInput{Path: "/notes.md"}, &r)
	if r.Content != "hello agent\nsecond\n" {
		t.Fatalf("after append = %q", r.Content)
	}
	// create mode refuses to clobber.
	res = e.call(t, "write_file", writeInput{Path: "/notes.md", Content: "x", Mode: "create"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "already exists") {
		t.Fatalf("create over existing = %v %q", res.IsError, errText(res))
	}
	// An unknown mode is rejected.
	res = e.call(t, "write_file", writeInput{Path: "/x.txt", Content: "x", Mode: "sideways"}, nil)
	if !res.IsError {
		t.Fatal("unknown mode should fail")
	}
}

func TestEditFile(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("code.go", []byte("package main\n\nfunc main() {\n\tprintln(\"old\")\n}\n"))
	// Seed before the first listing: a file added remotely later stays
	// invisible until the directory TTL expires, which is the intended
	// close-to-open behaviour and not what this test is about.
	e.fake.Seed("dup.txt", []byte("x\nx\n"))

	var out editOutput
	res := e.call(t, "edit_file", editInput{
		Path:   "/code.go",
		Edits:  []editSpec{{OldText: `println("old")`, NewText: `println("new")`}},
		DryRun: true,
	}, &out)
	if res.IsError {
		t.Fatalf("dry run failed: %s", errText(res))
	}
	if !out.DryRun || out.Applied != 1 || !strings.Contains(out.Diff, "old") {
		t.Fatalf("dry run output = %+v", out)
	}
	// Nothing changed yet.
	var r readTextOutput
	e.call(t, "read_text", readTextInput{Path: "/code.go"}, &r)
	if !strings.Contains(r.Content, `println("old")`) {
		t.Fatal("dry run must not modify the file")
	}

	e.call(t, "edit_file", editInput{
		Path:  "/code.go",
		Edits: []editSpec{{OldText: `println("old")`, NewText: `println("new")`}},
	}, &out)
	e.drain(t)
	e.call(t, "read_text", readTextInput{Path: "/code.go"}, &r)
	if !strings.Contains(r.Content, `println("new")`) {
		t.Fatalf("after edit = %q", r.Content)
	}

	// A non-unique target is refused rather than guessed at.
	res = e.call(t, "edit_file", editInput{Path: "/dup.txt", Edits: []editSpec{{OldText: "x", NewText: "y"}}}, nil)
	if !res.IsError || !strings.Contains(errText(res), "appears 2 times") {
		t.Fatalf("ambiguous edit = %v %q", res.IsError, errText(res))
	}
	// Missing text is reported.
	res = e.call(t, "edit_file", editInput{Path: "/dup.txt", Edits: []editSpec{{OldText: "zzz", NewText: "y"}}}, nil)
	if !res.IsError || !strings.Contains(errText(res), "not found") {
		t.Fatalf("missing text = %v %q", res.IsError, errText(res))
	}
}

func TestDeleteRequiresConfirm(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("trash.txt", []byte("bye"))
	res := e.call(t, "delete", deleteInput{Path: "/trash.txt"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "confirm=true") {
		t.Fatalf("delete without confirm = %v %q", res.IsError, errText(res))
	}
	// Still there.
	if res := e.call(t, "stat", statInput{Path: "/trash.txt"}, nil); res.IsError {
		t.Fatal("file should not have been deleted")
	}
	var ok okOutput
	res = e.call(t, "delete", deleteInput{Path: "/trash.txt", Confirm: true}, &ok)
	if res.IsError || !ok.OK {
		t.Fatalf("confirmed delete failed: %s", errText(res))
	}
	if res := e.call(t, "stat", statInput{Path: "/trash.txt"}, nil); !res.IsError {
		t.Fatal("file should be gone")
	}
	// The mount root is protected.
	res = e.call(t, "delete", deleteInput{Path: "/", Confirm: true}, nil)
	if !res.IsError {
		t.Fatal("deleting the root should be refused")
	}
}

func TestNonEmptyDirectoryDeleteGuidance(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("dir/file.txt", []byte("x"))
	res := e.call(t, "delete", deleteInput{Path: "/dir", Confirm: true}, nil)
	if !res.IsError || !strings.Contains(errText(res), "recursive=true") {
		t.Fatalf("non-empty delete should suggest recursive: %q", errText(res))
	}
	var ok okOutput
	res = e.call(t, "delete", deleteInput{Path: "/dir", Confirm: true, Recursive: true}, &ok)
	if res.IsError || !ok.OK {
		t.Fatalf("recursive delete failed: %s", errText(res))
	}
}

func TestCreateDirectoryAndMove(t *testing.T) {
	e := newEnv(t, Options{})
	var ok okOutput
	// Nested creation makes every missing parent.
	res := e.call(t, "create_directory", mkdirInput{Path: "/a/b/c"}, &ok)
	if res.IsError {
		t.Fatalf("mkdir failed: %s", errText(res))
	}
	var st statOutput
	e.call(t, "stat", statInput{Path: "/a/b/c"}, &st)
	if st.Kind != "directory" {
		t.Fatalf("stat = %+v", st)
	}
	// Creating again is not an error.
	if res := e.call(t, "create_directory", mkdirInput{Path: "/a/b/c"}, &ok); res.IsError {
		t.Fatalf("idempotent mkdir failed: %s", errText(res))
	}

	e.call(t, "write_file", writeInput{Path: "/a/b/c/f.txt", Content: "data"}, nil)
	e.drain(t)
	res = e.call(t, "move", moveInput{From: "/a/b/c/f.txt", To: "/a/b/c/g.txt"}, &ok)
	if res.IsError {
		t.Fatalf("move failed: %s", errText(res))
	}
	if res := e.call(t, "stat", statInput{Path: "/a/b/c/g.txt"}, nil); res.IsError {
		t.Fatal("moved file missing")
	}
	if res := e.call(t, "stat", statInput{Path: "/a/b/c/f.txt"}, nil); !res.IsError {
		t.Fatal("old path should be gone")
	}
}

func TestSearchByName(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("src/handler.go", []byte("package main\n// the needle is here\n"))
	e.fake.Seed("src/other.go", []byte("package main\n"))
	e.fake.Seed("docs/handler.md", []byte("docs\n"))
	// Warm the tree so the name index is populated.
	if _, err := e.fs.Warm(context.Background(), "/", -1); err != nil {
		t.Fatal(err)
	}

	var out searchOutput
	e.call(t, "search", searchInput{Query: "handler"}, &out)
	if len(out.Hits) != 2 {
		t.Fatalf("search hits = %+v", out.Hits)
	}
	// Scoping to a subtree filters.
	e.call(t, "search", searchInput{Path: "/src", Query: "handler"}, &out)
	if len(out.Hits) != 1 || !strings.HasPrefix(out.Hits[0].Path, "/src/") {
		t.Fatalf("scoped search = %+v", out.Hits)
	}
	// An empty query is refused.
	if res := e.call(t, "search", searchInput{Query: "  "}, nil); !res.IsError {
		t.Fatal("empty query should fail")
	}
}

func TestSearchContentNeedsCachedFiles(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("src/handler.go", []byte("package main\n// the needle is here\n"))
	if _, err := e.fs.Warm(context.Background(), "/", -1); err != nil {
		t.Fatal(err)
	}
	var out searchOutput
	e.call(t, "search", searchInput{Query: "handler", Content: "needle"}, &out)
	if len(out.Hits) != 0 || out.Note == "" {
		t.Fatalf("uncached content search should explain itself: %+v", out)
	}
	if !strings.Contains(out.Note, "pin") {
		t.Fatalf("note should point at pin: %q", out.Note)
	}
	// After pinning, the content search works and reports the line.
	var ok okOutput
	if res := e.call(t, "pin", pinInput{Path: "/src"}, &ok); res.IsError {
		t.Fatalf("pin failed: %s", errText(res))
	}
	e.call(t, "search", searchInput{Query: "handler", Content: "needle"}, &out)
	if len(out.Hits) != 1 || !strings.Contains(out.Hits[0].Line, "needle") {
		t.Fatalf("content search after pin = %+v", out)
	}
}

func TestAllowlistDeniesOutsidePaths(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	e.fake.Seed("work/inside.txt", []byte("ok"))
	e.fake.Seed("secret/outside.txt", []byte("no"))

	if res := e.call(t, "stat", statInput{Path: "/work/inside.txt"}, nil); res.IsError {
		t.Fatalf("allowed path rejected: %s", errText(res))
	}
	for _, p := range []string{"/secret/outside.txt", "/", "/work/../secret/outside.txt", "/workshop/x"} {
		res := e.call(t, "stat", statInput{Path: p}, nil)
		if !res.IsError || !strings.Contains(errText(res), "outside the allowed") {
			t.Errorf("path %q should be denied, got error=%v %q", p, res.IsError, errText(res))
		}
	}
	// Writes are denied too.
	if res := e.call(t, "write_file", writeInput{Path: "/secret/x.txt", Content: "x"}, nil); !res.IsError {
		t.Error("write outside the allowlist should be denied")
	}
	// list_roots only advertises roots the caller may use.
	var roots rootsOutput
	e.call(t, "list_roots", struct{}{}, &roots)
	if len(roots.Roots) != 0 {
		t.Fatalf("root / should not be advertised under an allowlist of /work: %+v", roots.Roots)
	}
}

func TestReadOnlyServerRefusesMutations(t *testing.T) {
	e := newEnv(t, Options{ReadOnly: true})
	e.fake.Seed("file.txt", []byte("content"))
	if res := e.call(t, "read_text", readTextInput{Path: "/file.txt"}, nil); res.IsError {
		t.Fatalf("read should work: %s", errText(res))
	}
	for name, args := range map[string]any{
		"write_file":       writeInput{Path: "/new.txt", Content: "x"},
		"edit_file":        editInput{Path: "/file.txt", Edits: []editSpec{{OldText: "content", NewText: "x"}}},
		"create_directory": mkdirInput{Path: "/d"},
		"move":             moveInput{From: "/file.txt", To: "/g.txt"},
		"copy":             moveInput{From: "/file.txt", To: "/g.txt"},
		"delete":           deleteInput{Path: "/file.txt", Confirm: true},
	} {
		res := e.call(t, name, args, nil)
		if !res.IsError || !strings.Contains(errText(res), "read-only") {
			t.Errorf("%s on a read-only server = %v %q", name, res.IsError, errText(res))
		}
	}
}

func TestStatManyReportsPerPathErrors(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("a.txt", []byte("a"))
	var out statManyOutput
	e.call(t, "stat_many", statManyInput{Paths: []string{"/a.txt", "/missing.txt"}}, &out)
	if len(out.Results) != 2 {
		t.Fatalf("results = %+v", out.Results)
	}
	if !out.Results[0].Exists || out.Results[0].Error != "" {
		t.Fatalf("existing path = %+v", out.Results[0])
	}
	if out.Results[1].Exists || !strings.Contains(out.Results[1].Error, "does not exist") {
		t.Fatalf("missing path = %+v", out.Results[1])
	}
}

func TestCacheStatusAndDownloadURL(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("media.bin", make([]byte, 8192))
	var cs cacheStatusOutput
	e.call(t, "cache_status", statInput{Path: "/media.bin"}, &cs)
	if cs.Cached != 0 {
		t.Fatalf("uncached file = %+v", cs)
	}
	var ok okOutput
	e.call(t, "pin", pinInput{Path: "/media.bin"}, &ok)
	e.call(t, "cache_status", statInput{Path: "/media.bin"}, &cs)
	if cs.Cached != 1 {
		t.Fatalf("after pin = %+v", cs)
	}

	var dl downloadURLOutput
	res := e.call(t, "get_download_url", statInput{Path: "/media.bin"}, &dl)
	if res.IsError || dl.URL == "" || dl.ExpiresAt == "" {
		t.Fatalf("download url = %+v (%s)", dl, errText(res))
	}
	// A file that has not been uploaded has no shareable link.
	e.call(t, "write_file", writeInput{Path: "/fresh.txt", Content: "x"}, nil)
	res = e.call(t, "get_download_url", statInput{Path: "/fresh.txt"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "not been uploaded") {
		t.Fatalf("local-only download url = %v %q", res.IsError, errText(res))
	}
}

func TestErrorsAreActionable(t *testing.T) {
	e := newEnv(t, Options{})
	e.fake.Seed("dir/f.txt", []byte("x"))
	cases := []struct {
		tool string
		args any
		want string
	}{
		{"read_text", readTextInput{Path: "/nope.txt"}, "does not exist"},
		{"read_text", readTextInput{Path: "/dir"}, "is a directory"},
		{"list_directory", listInput{Path: "/dir/f.txt"}, "not a directory"},
	}
	for _, c := range cases {
		res := e.call(t, c.tool, c.args, nil)
		if !res.IsError || !strings.Contains(errText(res), c.want) {
			t.Errorf("%s: got %q, want it to mention %q", c.tool, errText(res), c.want)
		}
	}
}
