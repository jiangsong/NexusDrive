package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	"cloudfs/internal/mcpsrv"
)

// indexYAML turns the content index on over the whole mount, in rules mode,
// which is what a person who wants semantic_search to see their notes
// writes.
const indexYAML = "index:\n  enabled: true\n  rules:\n    - path: /\n"

// withIndex hands the daemon's indexer to the MCP server the way
// cmd/cloudfs does: a nil indexer stays a nil interface, so the tools are
// registered only when there is an index behind them.
func withIndex(d *daemon.Daemon, o *mcpsrv.Options) {
	if d.Index != nil {
		o.Index = d.Index
	}
}

// TestContentSearchFindsAFreshMarkdownFile needs a kernel mount: run on
// Linux with /dev/fuse. A Markdown file written at the mount point is found
// by semantic_search within three seconds of the write: the FLUSH commit
// feeds the VFS change feed, the indexer extracts the file and the hit
// carries a file offset read_text can use.
func TestContentSearchFindsAFreshMarkdownFile(t *testing.T) {
	s := newStackWith(t, "writeback", stackOptions{extraYAML: indexYAML, mcp: withIndex})
	body := "# Plan\n\nthe e2e needle 51c9 sits here\n"
	if err := os.WriteFile(filepath.Join(s.dir, "plan.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	type hit struct {
		Path       string `json:"path"`
		StartOff   int64  `json:"start_off"`
		OffsetKind string `json:"offset_kind"`
	}
	var out struct {
		Hits []hit `json:"hits"`
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		res := s.callTool(t, "semantic_search", map[string]any{"query": "needle 51c9"}, &out)
		if res.IsError {
			t.Fatalf("semantic_search: %s", toolText(res))
		}
		if len(out.Hits) == 1 && out.Hits[0].Path == "/plan.md" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a file written through the mount was not searchable within 3s: %+v", out.Hits)
		}
		time.Sleep(100 * time.Millisecond)
	}
	h := out.Hits[0]
	if h.OffsetKind != "file" {
		t.Fatalf("a Markdown hit should carry file offsets: %+v", h)
	}
	// The offset addresses the file itself, so read_text from there gives
	// the chunk's text.
	var read struct {
		Content string `json:"content"`
	}
	if res := s.callTool(t, "read_text", map[string]any{"path": "/plan.md", "offset": h.StartOff}, &read); res.IsError {
		t.Fatalf("read_text: %s", toolText(res))
	}
	if !strings.HasPrefix(body[h.StartOff:], read.Content) || !strings.Contains(read.Content, "needle 51c9") {
		t.Fatalf("read_text at the hit offset %d gave %q", h.StartOff, read.Content)
	}
}

// TestContentSearchInTheBrowser renders the main window's content search
// and the index screen in headless Chromium: the same file the API found
// is a hit row with the query marked, and the index screen offers its
// rebuild action. The browser half runs only with CLOUDFS_BROWSER=1.
func TestContentSearchInTheBrowser(t *testing.T) {
	s := newUnmountedStack(t, stackOptions{extraYAML: indexYAML, mcp: withIndex})
	ctx := context.Background()
	root, err := s.d.FS.StatPath(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.d.FS.Mkdir(ctx, root.Ino, "notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.d.FS.WriteFile(ctx, "/notes/plan.md", []byte("browser smoke marker 7f3a"), false); err != nil {
		t.Fatal(err)
	}
	h := control.NewServer(s.d.Collector()).Handler()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w := uiCall(t, h, "GET", "/index/search?q=marker%207f3a", "")
		if w.Code == 200 && strings.Contains(w.Body.String(), `"/notes/plan.md"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not indexed within 5s: %d %s", w.Code, w.Body.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	var st struct {
		Index *struct {
			Enabled bool `json:"enabled"`
		} `json:"index"`
	}
	if w := uiCall(t, h, "GET", "/status", ""); json.Unmarshal(w.Body.Bytes(), &st) != nil || st.Index == nil || !st.Index.Enabled {
		t.Fatalf("/status does not announce the index, so the content segment would never appear: %s", w.Body.String())
	}

	chrome := requireBrowser(t)
	base := startControlUI(t, s.d.Collector())
	wants := []string{`data-hit="/notes/plan.md"`, "<mark>marker</mark>", "<mark>7f3a</mark>"}
	dom := renderedDOM(t, chrome, base+"/?lang=en#/connections?q=marker%207f3a&mode=content", wants...)
	for _, want := range wants {
		if !strings.Contains(dom, want) {
			t.Fatalf("the file browser's content search lacks %s:\n%s", want, dom)
		}
	}
	// The navigation carries both phase-one screens, in the order
	// docs/ui-plan.md gives them: agents, then index.
	agents, index := strings.Index(dom, `href="#/agents"`), strings.Index(dom, `href="#/index"`)
	if agents < 0 || index < 0 || agents > index {
		t.Fatalf("the nav does not list #/agents before #/index:\n%s", dom)
	}
	wants = []string{"Rebuild index", `data-source="config"`}
	dom = renderedDOM(t, chrome, base+"/?lang=en#/index", wants...)
	for _, want := range wants {
		if !strings.Contains(dom, want) {
			t.Fatalf("the index screen lacks %s:\n%s", want, dom)
		}
	}
}
