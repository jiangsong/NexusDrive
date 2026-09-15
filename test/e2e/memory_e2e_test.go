package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/daemon"
	"cloudfs/internal/embed"
	"cloudfs/internal/index"
	"cloudfs/internal/mcpsrv"
)

// memoryYAML is the configuration a person who wants agent memory writes:
// the index on (memory_search runs over it, through the built-in rule
// that follows memory.root, so no index rule is needed) and a root for the
// memory tree.
const memoryYAML = "index:\n  enabled: true\nmemory:\n  root: /agent-memory\n"

// withMemory hands the daemon's index and memory store to the MCP server
// the way cmd/cloudfs does for both transports.
func withMemory(d *daemon.Daemon, o *mcpsrv.Options) {
	withIndex(d, o)
	o.Memory = d.Memory
}

// memoryFact mirrors the structured result of memory_get and memory_put.
type memoryFact struct {
	Name      string   `json:"name"`
	Agent     string   `json:"agent"`
	Path      string   `json:"path"`
	Size      int64    `json:"size"`
	Content   string   `json:"content"`
	Version   string   `json:"version"`
	Conflicts []string `json:"conflicts"`
}

// linesPointingAt counts the MEMORY.md lines that link to facts/<name>.md.
func linesPointingAt(text, name string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "](facts/"+name+".md)") {
			n++
		}
	}
	return n
}

// TestMemoryPutIsVisibleInTheMountAndSearchable needs a kernel mount. It is
// the promise of the memory store: what an agent writes with memory_put is
// a plain file a person can cat at the mount point, with its frontmatter
// and exactly one MEMORY.md line; memory_search finds it within three
// seconds; a second put keeps the one line; and an edit made from the
// shell is what memory_get returns next, under a new version.
func TestMemoryPutIsVisibleInTheMountAndSearchable(t *testing.T) {
	s := newStackWith(t, "writeback", stackOptions{extraYAML: memoryYAML, mcp: withMemory})
	const body = "Prefer tabs over spaces in Go, needle 9c2e.\n"

	var put memoryFact
	res := s.callTool(t, "memory_put", map[string]any{
		"name": "style", "content": body, "description": "coding style",
	}, &put)
	if res.IsError {
		t.Fatalf("memory_put: %s", toolText(res))
	}
	// The client introduced itself as "e2e", so that is the agent.
	if put.Agent != "e2e" || put.Path != "/agent-memory/memory/e2e/facts/style.md" || put.Version == "" {
		t.Fatalf("memory_put returned %+v", put)
	}

	// The shell sees the fact file with its frontmatter and the body.
	agentDir := filepath.Join(s.dir, "agent-memory", "memory", "e2e")
	factPath := filepath.Join(agentDir, "facts", "style.md")
	raw, err := os.ReadFile(factPath)
	if err != nil {
		t.Fatalf("cat %s: %v", factPath, err)
	}
	file := string(raw)
	for _, want := range []string{"---\nname: style\n", "description: coding style\n", "updated_at: ", "\n---\n" + body} {
		if !strings.Contains(file, want) {
			t.Fatalf("facts/style.md at the mount lacks %q:\n%s", want, file)
		}
	}
	if !strings.HasSuffix(file, body) {
		t.Fatalf("the body is not the tail of the file:\n%s", file)
	}
	memoryMD, err := os.ReadFile(filepath.Join(agentDir, "MEMORY.md"))
	if err != nil {
		t.Fatalf("cat MEMORY.md: %v", err)
	}
	if n := linesPointingAt(string(memoryMD), "style"); n != 1 {
		t.Fatalf("MEMORY.md has %d lines for style, want 1:\n%s", n, memoryMD)
	}
	if !strings.Contains(string(memoryMD), "coding style") {
		t.Fatalf("MEMORY.md line lacks the description:\n%s", memoryMD)
	}

	// The fact is searchable within three seconds of the put.
	var found struct {
		Hits []struct {
			Path  string `json:"path"`
			Agent string `json:"agent"`
			Name  string `json:"name"`
		} `json:"hits"`
		ModeUsed string `json:"mode_used"`
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		res := s.callTool(t, "memory_search", map[string]any{"query": "needle 9c2e"}, &found)
		if res.IsError {
			t.Fatalf("memory_search: %s", toolText(res))
		}
		if len(found.Hits) == 1 && found.Hits[0].Name == "style" && found.Hits[0].Agent == "e2e" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a fact written with memory_put was not searchable within 3s: %+v", found.Hits)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if found.Hits[0].Path != put.Path || found.ModeUsed != "keyword" {
		t.Fatalf("memory_search hit %+v mode %q", found.Hits[0], found.ModeUsed)
	}

	// A second put with the version the first returned replaces the file
	// and keeps MEMORY.md at one line for the fact.
	var again memoryFact
	res = s.callTool(t, "memory_put", map[string]any{
		"name": "style", "content": body + "Also gofmt everything.\n", "expected_version": put.Version,
	}, &again)
	if res.IsError {
		t.Fatalf("second memory_put: %s", toolText(res))
	}
	if again.Version == put.Version {
		t.Fatalf("a changed fact kept version %s", put.Version)
	}
	memoryMD, err = os.ReadFile(filepath.Join(agentDir, "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if n := linesPointingAt(string(memoryMD), "style"); n != 1 {
		t.Fatalf("after a second put MEMORY.md has %d lines for style, want 1:\n%s", n, memoryMD)
	}

	// The shell edits the file the way `echo >> file` does; memory_get
	// returns the new content under a new version. The kernel learns of a
	// change made past it (the put went through MCP, not the mount) from
	// an inode notification the VFS sends asynchronously, and an O_APPEND
	// write positions itself at the size the kernel holds, so the edit
	// waits until `stat` at the mount reports the second put's size — what
	// a person who just looked at the file has already done.
	deadline = time.Now().Add(3 * time.Second)
	for {
		st, err := os.Stat(factPath)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() == again.Size {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stat at the mount still says %d bytes, the put wrote %d", st.Size(), again.Size)
		}
		time.Sleep(10 * time.Millisecond)
	}
	f, err := os.OpenFile(factPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("Edited from the terminal.\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	var got memoryFact
	if res := s.callTool(t, "memory_get", map[string]any{"name": "style"}, &got); res.IsError {
		t.Fatalf("memory_get: %s", toolText(res))
	}
	if !strings.HasSuffix(got.Content, "Also gofmt everything.\nEdited from the terminal.\n") || !strings.HasPrefix(got.Content, body) {
		t.Fatalf("memory_get after a shell edit returned %q", got.Content)
	}
	if got.Version == again.Version || got.Version == put.Version {
		t.Fatalf("the shell edit did not change the version (%s)", got.Version)
	}
	if len(got.Conflicts) != 0 {
		t.Fatalf("no conflict copy was made, yet memory_get lists %v", got.Conflicts)
	}
	s.settle(t)
}

// TestHybridSearchWithAFakeEmbedder needs a kernel mount. With an embedder
// behind the index, a Markdown file written at the mount point is
// extracted, embedded and then found by semantic_search in hybrid mode as
// hybrid — mode_used says so and nothing is degraded — and in vector
// mode; index_status reports the embedder's model and the stored vector.
// The fake embedder stands in for an endpoint: it is what test/perf
// counts calls against, and it reports its health the way the real client
// does.
func TestHybridSearchWithAFakeEmbedder(t *testing.T) {
	// The daemon builds its embedder from index.embedding, which only
	// names real endpoints, so the index here is the test's own over the
	// daemon's VFS: the same Indexer cmd/cloudfs hands to the MCP server,
	// with embed.Fake in Options.Embedder. index.enabled stays false so the
	// daemon opens no second index over the same files.
	fake := embed.NewFake(8)
	var x *index.Indexer
	var store *index.Store
	indexDir := filepath.Join(t.TempDir(), "index")
	s := newStackWith(t, "writeback", stackOptions{
		extraYAML: "index:\n  enabled: false\n  rules:\n    - path: /\n",
		mcp: func(d *daemon.Daemon, o *mcpsrv.Options) {
			var err error
			store, err = index.OpenStore(indexDir)
			if err != nil {
				t.Fatal(err)
			}
			cfg := d.Config.Index
			cfg.Enabled = true
			x, err = index.New(index.Options{FS: d.FS, Store: store, Config: cfg, Embedder: fake})
			if err != nil {
				t.Fatal(err)
			}
			x.Start(context.Background())
			o.Index = x
		},
	})
	// Registered after the stack's own cleanup, so it runs before it: the
	// indexer stops before the VFS it watches closes.
	t.Cleanup(func() { x.Close(); store.Close() })

	body := "# Notes\n\nthe hybrid needle 4d7b lives in this paragraph\n"
	if err := os.WriteFile(filepath.Join(s.dir, "notes.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	type hit struct {
		Path string `json:"path"`
	}
	var out struct {
		Hits     []hit  `json:"hits"`
		ModeUsed string `json:"mode_used"`
		Degraded string `json:"degraded"`
	}
	search := func(mode string) {
		t.Helper()
		out.Hits, out.ModeUsed, out.Degraded = nil, "", ""
		res := s.callTool(t, "semantic_search", map[string]any{"query": "needle 4d7b", "mode": mode}, &out)
		if res.IsError {
			t.Fatalf("semantic_search %s: %s", mode, toolText(res))
		}
	}
	// Extraction first: until the chunk exists there is nothing to embed.
	deadline := time.Now().Add(3 * time.Second)
	for {
		search("keyword")
		if len(out.Hits) == 1 && out.Hits[0].Path == "/notes.md" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not extracted within 3s: %+v", out.Hits)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Embedding: the worker Start launched may already have taken the
	// chunk; EmbedNow drains whatever is left on this goroutine and waits
	// for a turn in progress, so afterwards the vector exists and the one
	// chunk cost exactly one call whoever made it (ceil(1 / batch)).
	rep, err := x.EmbedNow(context.Background())
	if err != nil {
		t.Fatalf("EmbedNow: %v", err)
	}
	if rep.Paused != "" {
		t.Fatalf("EmbedNow paused: %+v", rep)
	}
	if fake.Calls() != 1 {
		t.Fatalf("the fake embedder took %d calls for one chunk", fake.Calls())
	}

	search("hybrid")
	if out.ModeUsed != "hybrid" || out.Degraded != "" {
		t.Fatalf("hybrid with vectors present ran as %q (degraded %q)", out.ModeUsed, out.Degraded)
	}
	if len(out.Hits) != 1 || out.Hits[0].Path != "/notes.md" {
		t.Fatalf("hybrid hits %+v", out.Hits)
	}
	// The query itself was embedded: one more call.
	if fake.Calls() != 2 {
		t.Fatalf("a hybrid query should embed once, calls=%d", fake.Calls())
	}
	search("vector")
	if out.ModeUsed != "vector" || out.Degraded != "" || len(out.Hits) != 1 || out.Hits[0].Path != "/notes.md" {
		t.Fatalf("vector mode: mode_used=%q degraded=%q hits=%+v", out.ModeUsed, out.Degraded, out.Hits)
	}

	var st struct {
		Vectors   int `json:"vectors"`
		Embedding struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Dim      int    `json:"dim"`
			Healthy  bool   `json:"healthy"`
			Embedded int    `json:"embedded"`
			Pending  int    `json:"pending"`
		} `json:"embedding"`
	}
	if res := s.callTool(t, "index_status", map[string]any{}, &st); res.IsError {
		t.Fatalf("index_status: %s", toolText(res))
	}
	// The configuration names no provider (the fake is injected), so the
	// provider stays none; the model, dimension and counts are the fake's.
	if st.Embedding.Provider != "none" || st.Embedding.Model != "fake" || st.Embedding.Dim != 8 || !st.Embedding.Healthy {
		t.Fatalf("index_status.embedding %+v", st.Embedding)
	}
	if st.Vectors != 1 || st.Embedding.Embedded != 1 || st.Embedding.Pending != 0 {
		t.Fatalf("index_status vectors=%d embedded=%d pending=%d", st.Vectors, st.Embedding.Embedded, st.Embedding.Pending)
	}
	s.settle(t)
}

// TestMemoryTabInTheBrowser renders the agents screen's memory tab in
// headless Chromium: the agent and the fact created over MCP are listed,
// and the editor deep link (#/agents?tab=memory&agent=<a>&memory=<name>)
// opens the fact with its body in the textarea. Runs only with
// CLOUDFS_BROWSER=1.
func TestMemoryTabInTheBrowser(t *testing.T) {
	s := newUnmountedStack(t, stackOptions{extraYAML: memoryYAML, mcp: withMemory})
	const body = "Browser memory marker 3e8f.\nSecond line.\n"
	var put memoryFact
	if res := s.callTool(t, "memory_put", map[string]any{
		"name": "workflow", "content": body, "description": "how we work", "type": "preference",
	}, &put); res.IsError {
		t.Fatalf("memory_put: %s", toolText(res))
	}

	chrome := requireBrowser(t)
	base := startControlUI(t, s.d.Collector())
	wants := []string{`data-agent="e2e"`, `data-fact="workflow"`, "how we work", "preference"}
	dom := renderedDOM(t, chrome, base+"/?lang=en#/agents?tab=memory&agent=e2e", wants...)
	for _, want := range wants {
		if !strings.Contains(dom, want) {
			t.Fatalf("the memory tab lacks %s:\n%s", want, dom)
		}
	}
	if strings.Contains(dom, `data-conflicts=`) {
		t.Fatalf("a fact without copies shows a conflict marker:\n%s", dom)
	}

	// The deep link opens the editor: the file path is in the sheet and the
	// textarea holds the body (filled through .value, so it is read back
	// from the live page rather than the serialized DOM).
	dom, value := renderedDOMAndValue(t, chrome, base+"/?lang=en#/agents?tab=memory&agent=e2e&memory=workflow",
		"document.querySelector('textarea').value", put.Path)
	if !strings.Contains(dom, put.Path) || !strings.Contains(dom, "Memory e2e / workflow") {
		t.Fatalf("the editor did not open from the deep link:\n%s", dom)
	}
	if value != body {
		t.Fatalf("the editor textarea holds %q, want %q", value, body)
	}
}

// renderedDOMAndValue is renderedDOM followed by evaluating expr in the
// same page, for state the DOM serialization cannot show (a textarea's
// value). It attaches a second session to the tab render opened, found by
// its URL.
func renderedDOMAndValue(t *testing.T, chrome, url, expr string, want ...string) (dom, value string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	b, err := startBrowser(ctx, chrome)
	if err != nil {
		t.Fatalf("headless chrome: %v", err)
	}
	defer b.close()
	dom, err = b.render(ctx, url, want, 30*time.Second)
	if err != nil {
		t.Fatalf("headless chrome %s: %v", url, err)
	}
	res, err := b.call(ctx, "", "Target.getTargets", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			URL      string `json:"url"`
		} `json:"targetInfos"`
	}
	if err := json.Unmarshal(res, &targets); err != nil {
		t.Fatal(err)
	}
	targetID := ""
	for _, ti := range targets.TargetInfos {
		if ti.Type == "page" && ti.URL == url {
			targetID = ti.TargetID
		}
	}
	if targetID == "" {
		t.Fatalf("no page target at %s among %+v", url, targets.TargetInfos)
	}
	res, err = b.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &attached); err != nil {
		t.Fatal(err)
	}
	res, err = b.call(ctx, attached.SessionID, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatal(err)
	}
	return dom, fmt.Sprint(out.Result.Value)
}
