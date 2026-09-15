package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/index"
	"cloudfs/internal/memory"
)

// memoryControl is a server over a real memory.Store on the fixture's VFS:
// the routes are thin, so what the tests prove is that a fact written
// through the API is the same file the MCP tools would read, with the
// same names, limits and version check.
func memoryControl(t *testing.T, cfg config.Memory, idx memory.Searcher) (*fixture, *Server, *memory.Store) {
	t.Helper()
	f, _ := cacheControl(t)
	f.coll.FS.SetWriteBackend(f.j, nil)
	store := memory.New(memory.Options{FS: f.coll.FS, Config: cfg, Searcher: idx})
	f.coll.Memory = store
	return f, NewServer(f.coll), store
}

func memoryConfig() config.Memory {
	return config.Memory{Root: "/work/.agent", MaxFactBytes: 4096, MaxAgentBytes: 1 << 20}
}

func TestMemoryPutRequiresExpectedVersionWhenGiven(t *testing.T) {
	_, s, store := memoryControl(t, memoryConfig(), nil)
	ctx := context.Background()

	first := decode[memory.Fact](t, call(t, s, "PUT", "/memory/claude-code/style", `{"content":"tabs, not spaces\n","description":"code style","type":"preference"}`))
	if first.Name != "style" || first.Agent != "claude-code" || first.Version == "" || first.Content != "tabs, not spaces\n" || first.Meta.Description != "code style" || first.Meta.Type != "preference" {
		t.Fatalf("first put: %+v", first)
	}
	if first.Path != "/work/.agent/memory/claude-code/facts/style.md" {
		t.Fatalf("path = %q", first.Path)
	}

	// A stale expected_version is refused with the version the caller
	// must re-read, and the content stays what it was.
	w := call(t, s, "PUT", "/memory/claude-code/style", `{"content":"spaces\n","expected_version":"0123456789abcdef01234567"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale put: got %d %s, want 409", w.Code, w.Body.String())
	}
	var conflict MemoryConflictResponse
	if err := json.Unmarshal(w.Body.Bytes(), &conflict); err != nil || conflict.CurrentVersion != first.Version || conflict.Error == "" {
		t.Fatalf("stale put body: %s (%v), want current_version %q", w.Body.String(), err, first.Version)
	}
	if got, err := store.Get(ctx, "claude-code", "style"); err != nil || got.Content != "tabs, not spaces\n" || got.Version != first.Version {
		t.Fatalf("content changed after a refused put: %+v %v", got, err)
	}

	// The matching version goes through and moves the version on.
	second := decode[memory.Fact](t, call(t, s, "PUT", "/memory/claude-code/style", `{"content":"spaces\n","expected_version":"`+first.Version+`"}`))
	if second.Version == first.Version || second.Content != "spaces\n" || second.Meta.Description != "code style" {
		t.Fatalf("matching put: %+v", second)
	}

	// No expected_version is a plain overwrite, as memory_put allows.
	third := decode[memory.Fact](t, call(t, s, "PUT", "/memory/claude-code/style", `{"content":"either\n","mode":"replace"}`))
	if third.Version == second.Version || third.Content != "either\n" {
		t.Fatalf("unconditional put: %+v", third)
	}

	// The fact is what GET returns, and the listing and the agent table
	// count it.
	got := decode[memory.Fact](t, call(t, s, "GET", "/memory/claude-code/style", ""))
	if got.Version != third.Version || got.Content != "either\n" || got.Conflicts == nil {
		t.Fatalf("get: %+v", got)
	}
	list := decode[MemoryListResponse](t, call(t, s, "GET", "/memory/claude-code?limit=10", ""))
	if list.Agent != "claude-code" || len(list.Facts) != 1 || list.Facts[0].Name != "style" || list.Facts[0].Meta.Description != "code style" || list.NextCursor != "" {
		t.Fatalf("list: %+v", list)
	}
	agents := decode[MemoryAgentsResponse](t, call(t, s, "GET", "/memory/agents", ""))
	if !agents.Enabled || agents.Root != "/work/.agent" || agents.MaxFactBytes != 4096 || agents.MaxAgentBytes != 1<<20 || len(agents.Agents) != 1 || agents.Agents[0].Name != "claude-code" || agents.Agents[0].Facts != 1 || agents.Agents[0].Bytes == 0 {
		t.Fatalf("agents: %+v", agents)
	}

	// Over the fact limit: 413 with the usage in the text.
	w = call(t, s, "PUT", "/memory/claude-code/big", `{"content":"`+strings.Repeat("x", 5000)+`"}`)
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "max_fact_bytes") {
		t.Fatalf("too large: got %d %s", w.Code, w.Body.String())
	}
	// A bad mode and an unknown fact keep their own codes.
	if w := call(t, s, "PUT", "/memory/claude-code/style", `{"content":"x","mode":"prepend"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad mode: got %d %s", w.Code, w.Body.String())
	}
	if w := call(t, s, "GET", "/memory/claude-code/missing", ""); w.Code != http.StatusNotFound {
		t.Fatalf("missing fact: got %d %s", w.Code, w.Body.String())
	}
	if w := call(t, s, "POST", "/memory/claude-code/style", `{}`); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post: got %d", w.Code)
	}
}

func TestMemoryDeleteNeedsConfirm(t *testing.T) {
	_, s, store := memoryControl(t, memoryConfig(), nil)
	ctx := context.Background()
	decode[memory.Fact](t, call(t, s, "PUT", "/memory/codex/todo", `{"content":"ship it\n"}`))

	w := call(t, s, "DELETE", "/memory/codex/todo", `{}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "confirm=true") || !strings.Contains(w.Body.String(), "codex") || !strings.Contains(w.Body.String(), "todo") {
		t.Fatalf("delete without confirm: got %d %s", w.Code, w.Body.String())
	}
	if _, err := store.Get(ctx, "codex", "todo"); err != nil {
		t.Fatalf("fact gone after a refused delete: %v", err)
	}
	if w := call(t, s, "DELETE", "/memory/codex/todo", `{"confirm":false}`); w.Code != http.StatusBadRequest {
		t.Fatalf("delete with confirm=false: got %d", w.Code)
	}

	done := decode[MemoryDeleteResponse](t, call(t, s, "DELETE", "/memory/codex/todo", `{"confirm":true}`))
	if !done.OK || done.Name != "todo" || done.Agent != "codex" || done.Path != "/work/.agent/memory/codex/facts/todo.md" {
		t.Fatalf("delete: %+v", done)
	}
	if w := call(t, s, "GET", "/memory/codex/todo", ""); w.Code != http.StatusNotFound {
		t.Fatalf("after delete: got %d %s", w.Code, w.Body.String())
	}
	if w := call(t, s, "DELETE", "/memory/codex/todo", `{"confirm":true}`); w.Code != http.StatusNotFound {
		t.Fatalf("delete twice: got %d %s", w.Code, w.Body.String())
	}
}

func TestMemoryRoutesRefuseBadNames(t *testing.T) {
	_, s, store := memoryControl(t, memoryConfig(), nil)
	ctx := context.Background()
	long := strings.Repeat("a", 65)
	// %2e%2e reaches the handler as ".."; a literal ".." is cleaned away
	// by the mux before any handler runs.
	for _, name := range []string{"%2e%2e", "a/b", "Upper", long, "-dash", "sp%20ace"} {
		for _, tc := range []struct{ method, body string }{{"GET", ""}, {"PUT", `{"content":"x"}`}, {"DELETE", `{"confirm":true}`}} {
			w := call(t, s, tc.method, "/memory/claude-code/"+name, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s name %q: got %d %s, want 400", tc.method, name, w.Code, w.Body.String())
			}
		}
	}
	for _, ag := range []string{"%2e%2e", "Upper", long, "-x"} {
		if w := call(t, s, "GET", "/memory/"+ag, ""); w.Code != http.StatusBadRequest {
			t.Fatalf("agent %q list: got %d %s, want 400", ag, w.Code, w.Body.String())
		}
		if w := call(t, s, "PUT", "/memory/"+ag+"/ok", `{"content":"x"}`); w.Code != http.StatusBadRequest {
			t.Fatalf("agent %q put: got %d %s, want 400", ag, w.Code, w.Body.String())
		}
		if w := call(t, s, "GET", "/memory/search?q=x&agent="+ag, ""); w.Code != http.StatusBadRequest {
			t.Fatalf("agent %q search: got %d %s, want 400", ag, w.Code, w.Body.String())
		}
	}
	// Nothing was written under any of them.
	agents, err := store.Agents(ctx)
	if err != nil || len(agents) != 0 {
		t.Fatalf("agents after refused writes: %+v %v", agents, err)
	}
	// Extra path segments and an empty tail are not routes.
	for _, p := range []string{"/memory/", "/memory/claude-code/style/extra"} {
		if w := call(t, s, "GET", p, ""); w.Code != http.StatusNotFound && w.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d", p, w.Code)
		}
	}
}

func TestMemoryRoutesWhenNoStore(t *testing.T) {
	f := newFixture(t)
	s := NewServer(f.coll)
	agents := decode[MemoryAgentsResponse](t, call(t, s, "GET", "/memory/agents", ""))
	if agents.Enabled || agents.Reason != "unavailable" {
		t.Fatalf("agents without a store: %+v", agents)
	}
	for _, tc := range []struct{ method, target, body string }{
		{"GET", "/memory/claude-code", ""},
		{"GET", "/memory/claude-code/style", ""},
		{"PUT", "/memory/claude-code/style", `{"content":"x"}`},
		{"DELETE", "/memory/claude-code/style", `{"confirm":true}`},
		{"GET", "/memory/search?q=x", ""},
	} {
		w := call(t, s, tc.method, tc.target, tc.body)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s %s without a store: got %d %s, want 404", tc.method, tc.target, w.Code, w.Body.String())
		}
	}
}

func TestMemoryAgentsExplainsAMissingRoot(t *testing.T) {
	_, s, _ := memoryControl(t, config.Memory{}, nil)
	agents := decode[MemoryAgentsResponse](t, call(t, s, "GET", "/memory/agents", ""))
	if agents.Enabled || agents.Reason != "no_root" || !strings.Contains(agents.Example, "memory:\n  root:") {
		t.Fatalf("agents without a root: %+v", agents)
	}
	// The other routes say what to configure rather than pretending.
	w := call(t, s, "PUT", "/memory/claude-code/style", `{"content":"x"}`)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "memory.root") {
		t.Fatalf("put without a root: got %d %s", w.Code, w.Body.String())
	}
	if w := call(t, s, "GET", "/memory/claude-code", ""); w.Code != http.StatusNotFound {
		t.Fatalf("list without a root: got %d", w.Code)
	}
}

func TestMemorySearchWithoutIndexIs409(t *testing.T) {
	_, s, _ := memoryControl(t, memoryConfig(), nil)
	w := call(t, s, "GET", "/memory/search?q=tabs&agent=claude-code", "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "index.enabled") {
		t.Fatalf("search without an index: got %d %s, want 409", w.Code, w.Body.String())
	}
}

func TestMemorySearchScopesTheIndexToTheAgent(t *testing.T) {
	idx := newFakeIndex()
	_, s, _ := memoryControl(t, memoryConfig(), idx)
	decode[memory.Fact](t, call(t, s, "PUT", "/memory/claude-code/style", `{"content":"tabs\n"}`))

	res := decode[memory.SearchResult](t, call(t, s, "GET", "/memory/search?q=tabs&agent=claude-code&top_k=5&mode=keyword", ""))
	if res.Hits == nil || res.ModeUsed != "keyword" {
		t.Fatalf("search: %+v", res)
	}
	idx.mu.Lock()
	queries := append([]index.SearchQuery(nil), idx.searches...)
	idx.mu.Unlock()
	if len(queries) != 1 || queries[0].Query != "tabs" || queries[0].TopK != 5 || queries[0].Mode != "keyword" {
		t.Fatalf("query: %+v", queries)
	}
	if roots := strings.Join(queries[0].Roots, ","); roots != "/work/.agent/memory/claude-code,/work/.agent/memory/shared" {
		t.Fatalf("roots = %q", roots)
	}
	// include_shared=0 narrows to the agent alone.
	decode[memory.SearchResult](t, call(t, s, "GET", "/memory/search?q=tabs&agent=claude-code&include_shared=0", ""))
	idx.mu.Lock()
	last := idx.searches[len(idx.searches)-1]
	idx.mu.Unlock()
	if strings.Join(last.Roots, ",") != "/work/.agent/memory/claude-code" {
		t.Fatalf("roots without shared = %q", last.Roots)
	}
	for _, bad := range []string{"/memory/search?agent=claude-code", "/memory/search?q=x&mode=magic", "/memory/search?q=x&top_k=0", "/memory/search?q=x&top_k=101"} {
		if w := call(t, s, "GET", bad, ""); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d %s", bad, w.Code, w.Body.String())
		}
	}
}
