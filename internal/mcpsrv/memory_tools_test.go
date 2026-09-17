package mcpsrv

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/index"
	"cloudfs/internal/memory"
	"cloudfs/internal/testx"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var memoryToolNames = []string{"memory_list", "memory_get", "memory_put", "memory_delete", "memory_search"}

func TestMemoryToolsAbsentWithoutAStore(t *testing.T) {
	names := toolNames(t, newEnv(t, Options{}))
	for _, n := range memoryToolNames {
		if names[n] {
			t.Fatalf("%s registered without a memory store", n)
		}
	}
	e, _, _ := newMemoryEnv(t, Options{}, nil, memoryConfig("/work/.agent"), nil)
	names = toolNames(t, e.env)
	for _, n := range memoryToolNames {
		if !names[n] {
			t.Fatalf("%s missing with a store", n)
		}
	}
}

func TestPutCreatesTheFactAndOneIndexLine(t *testing.T) {
	e, _, _ := newMemoryEnv(t, Options{}, nil, memoryConfig("/work/.agent"), nil)
	var put memoryPutOutput
	if res := e.call(t, "memory_put", map[string]any{"name": "style", "content": "Prefer tabs.\n", "description": "coding style", "type": "preference"}, &put); res.IsError {
		t.Fatal(errText(res))
	}
	// The client of the in-memory transport is named "test", so the
	// default agent is "test".
	if put.Agent != "test" || put.Path != "/work/.agent/memory/test/facts/style.md" || put.Version == "" {
		t.Fatalf("%+v", put)
	}
	body := readText(t, e, put.Path)
	if !strings.HasPrefix(body, "---\nname: style\ndescription: coding style\ntype: preference\n") || !strings.HasSuffix(body, "\n---\nPrefer tabs.\n") {
		t.Fatalf("fact file:\n%s", body)
	}
	if idx := readText(t, e, "/work/.agent/memory/test/MEMORY.md"); idx != "- [style](facts/style.md) — coding style\n" {
		t.Fatalf("MEMORY.md: %q", idx)
	}
	// A repeated put replaces the line rather than adding one.
	var put2 memoryPutOutput
	if res := e.call(t, "memory_put", map[string]any{"name": "style", "content": "Prefer spaces.\n"}, &put2); res.IsError {
		t.Fatal(errText(res))
	}
	if idx := readText(t, e, "/work/.agent/memory/test/MEMORY.md"); idx != "- [style](facts/style.md) — coding style\n" {
		t.Fatalf("MEMORY.md after the second put: %q", idx)
	}
	var got memory.Fact
	if res := e.call(t, "memory_get", map[string]any{"name": "style"}, &got); res.IsError {
		t.Fatal(errText(res))
	}
	if got.Content != "Prefer spaces.\n" || got.Version != put2.Version || got.Meta.Description != "coding style" || len(got.Conflicts) != 0 {
		t.Fatalf("%+v", got)
	}
	var list memoryListOutput
	if res := e.call(t, "memory_list", map[string]any{}, &list); res.IsError {
		t.Fatal(errText(res))
	}
	if list.Agent != "test" || len(list.Facts) != 1 || list.Facts[0].Name != "style" || list.Facts[0].Meta.Type != "preference" {
		t.Fatalf("%+v", list)
	}
	// Another agent's memory is reachable by name; the shared area too.
	if res := e.call(t, "memory_put", map[string]any{"name": "team", "content": "We use Go.\n", "agent": "shared"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if res := e.call(t, "memory_get", map[string]any{"name": "team", "agent": "shared"}, &got); res.IsError || got.Agent != "shared" {
		t.Fatalf("%+v %s", got, errText(res))
	}
}

func TestStaleExpectedVersionIsRefused(t *testing.T) {
	e, _, _ := newMemoryEnv(t, Options{}, nil, memoryConfig("/work/.agent"), nil)
	var put memoryPutOutput
	if res := e.call(t, "memory_put", map[string]any{"name": "style", "content": "one\n"}, &put); res.IsError {
		t.Fatal(errText(res))
	}
	if res := e.call(t, "memory_put", map[string]any{"name": "style", "content": "two\n", "expected_version": put.Version}, nil); res.IsError {
		t.Fatalf("matching version refused: %s", errText(res))
	}
	res := e.call(t, "memory_put", map[string]any{"name": "style", "content": "three\n", "expected_version": put.Version}, nil)
	if !res.IsError || !strings.Contains(errText(res), "memory changed elsewhere; re-read") {
		t.Fatalf("stale version: %v %s", res.IsError, errText(res))
	}
	var got memory.Fact
	e.call(t, "memory_get", map[string]any{"name": "style"}, &got)
	if got.Content != "two\n" {
		t.Fatalf("content changed by a refused put: %q", got.Content)
	}
}

func TestMemoryDeleteNeedsConfirm(t *testing.T) {
	e, _, _ := newMemoryEnv(t, Options{}, nil, memoryConfig("/work/.agent"), nil)
	if res := e.call(t, "memory_put", map[string]any{"name": "style", "content": "x\n"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	res := e.call(t, "memory_delete", map[string]any{"name": "style", "confirm": false}, nil)
	if !res.IsError || !strings.Contains(errText(res), "confirm=true") {
		t.Fatalf("delete without confirm: %v %s", res.IsError, errText(res))
	}
	if res := e.call(t, "memory_get", map[string]any{"name": "style"}, nil); res.IsError {
		t.Fatal("the fact was deleted without confirm")
	}
	if res := e.call(t, "memory_delete", map[string]any{"name": "style", "confirm": true}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	res = e.call(t, "memory_get", map[string]any{"name": "style"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "no such memory") {
		t.Fatalf("after delete: %v %s", res.IsError, errText(res))
	}
	if idx := readText(t, e, "/work/.agent/memory/test/MEMORY.md"); idx != "" {
		t.Fatalf("MEMORY.md after delete: %q", idx)
	}
	// Deleting again says so.
	res = e.call(t, "memory_delete", map[string]any{"name": "style", "confirm": true}, nil)
	if !res.IsError || !strings.Contains(errText(res), "no such memory") {
		t.Fatalf("second delete: %v %s", res.IsError, errText(res))
	}
}

// memoryCalls is every memory tool with valid arguments, for the tests
// that expect one answer from all five.
var memoryCalls = map[string]map[string]any{
	"memory_list":   {},
	"memory_get":    {"name": "style"},
	"memory_put":    {"name": "style", "content": "x"},
	"memory_delete": {"name": "style", "confirm": true},
	"memory_search": {"query": "style"},
}

func TestRootOutsideScopeFailsEveryToolTheSameWay(t *testing.T) {
	// No index: memory_search must refuse on the root before it looks for
	// one, and without an index worker nothing else touches the provider.
	e, _, _ := newMemoryEnv(t, Options{}, &agent.Scope{Read: []string{"/work"}}, memoryConfig("/gd/.agent"), nil)
	e.fake.Seed("gd/.keep", []byte(""))
	want := "memory root /gd/.agent is outside the allowed directories of this session; set memory.root inside mcp.allow"
	for name, args := range memoryCalls {
		res := e.call(t, name, args, nil)
		if !res.IsError || errText(res) != want {
			t.Errorf("%s: IsError=%v %q", name, res.IsError, errText(res))
		}
	}
	if e.fake.TotalCalls() != 0 {
		t.Fatalf("a refused memory call reached the provider: %d calls", e.fake.TotalCalls())
	}
}

func TestMemoryToolsRefuseWithoutARoot(t *testing.T) {
	e, _, _ := newMemoryEnv(t, Options{}, nil, config.Memory{}, nil)
	for name, args := range memoryCalls {
		res := e.call(t, name, args, nil)
		if !res.IsError || !strings.Contains(errText(res), "memory.root is not configured") {
			t.Errorf("%s: IsError=%v %q", name, res.IsError, errText(res))
		}
	}
}

func TestReadOnlyAllowsReadsOnly(t *testing.T) {
	// Seed a fact with a writable server, then serve the same files
	// read-only: the way `cloudfs mcp --read-only` runs.
	e, _, _ := newMemoryEnv(t, Options{}, nil, memoryConfig("/work/.agent"), nil)
	if res := e.call(t, "memory_put", map[string]any{"name": "style", "content": "x\n"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	e.drain(t)
	ro, _, _ := newMemoryEnv(t, Options{ReadOnly: true}, nil, memoryConfig("/work/.agent"), &config.Index{Enabled: true})
	ro.fake.Seed("work/.agent/memory/test/MEMORY.md", []byte("- [style](facts/style.md)\n"))
	ro.fake.Seed("work/.agent/memory/test/facts/style.md", []byte("---\nname: style\n---\nx\n"))
	for _, name := range []string{"memory_list", "memory_get", "memory_search"} {
		if res := ro.call(t, name, memoryCalls[name], nil); res.IsError {
			t.Errorf("%s refused on a read-only server: %s", name, errText(res))
		}
	}
	for _, name := range []string{"memory_put", "memory_delete"} {
		res := ro.call(t, name, memoryCalls[name], nil)
		if !res.IsError || !strings.Contains(errText(res), "read-only") {
			t.Errorf("%s on a read-only server: IsError=%v %q", name, res.IsError, errText(res))
		}
	}
	var got memory.Fact
	if res := ro.call(t, "memory_get", map[string]any{"name": "style"}, &got); res.IsError || got.Content != "x\n" {
		t.Fatalf("%+v %s", got, errText(res))
	}
}

func TestMemoryPutAndDeleteNeedTheOwner(t *testing.T) {
	e, _, _ := newMemoryEnv(t, Options{NonOwner: true}, nil, memoryConfig("/work/.agent"), nil)
	for _, name := range []string{"memory_put", "memory_delete"} {
		res := e.call(t, name, memoryCalls[name], nil)
		if !res.IsError || !strings.Contains(errText(res), "requires the storage owner") {
			t.Errorf("%s on a non-owner: IsError=%v %q", name, res.IsError, errText(res))
		}
	}
	if e.fake.TotalCalls() != 0 {
		t.Fatalf("a refused write reached the provider: %d calls", e.fake.TotalCalls())
	}
	if res := e.call(t, "memory_list", map[string]any{}, nil); res.IsError {
		t.Fatalf("memory_list on a non-owner: %s", errText(res))
	}
}

func TestMemorySearchWithoutAnIndexSaysSo(t *testing.T) {
	e, _, _ := newMemoryEnv(t, Options{}, nil, memoryConfig("/work/.agent"), nil)
	res := e.call(t, "memory_search", map[string]any{"query": "x"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "index.enabled: false") {
		t.Fatalf("search without an index: %v %s", res.IsError, errText(res))
	}
}

func TestMemorySearchFindsAFreshFact(t *testing.T) {
	if testx.RaceEnabled {
		t.Skip("the three-second bound is a timing assertion")
	}
	e, _, x := newMemoryEnv(t, Options{}, nil, memoryConfig("/work/.agent"), &config.Index{Enabled: true})
	// The built-in rule is listed as such, and unindex refuses it.
	rules, err := x.Rules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Path != "/work/.agent/memory" || rules[0].Source != "builtin" {
		t.Fatalf("rules: %+v", rules)
	}
	res := e.call(t, "unindex", map[string]any{"path": "/work/.agent/memory"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "built in") {
		t.Fatalf("unindex of the builtin rule: %v %s", res.IsError, errText(res))
	}
	if res := e.call(t, "memory_put", map[string]any{"name": "style", "content": "the needle 7f3a sits in this fact\n", "description": "coding style"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if res := e.call(t, "memory_put", map[string]any{"name": "team", "content": "the needle 7f3a is shared too\n", "agent": "shared"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if res := e.call(t, "memory_put", map[string]any{"name": "other", "content": "the needle 7f3a belongs to codex\n", "agent": "codex"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	var out memory.SearchResult
	deadline := time.Now().Add(3 * time.Second)
	for {
		if res := e.call(t, "memory_search", map[string]any{"query": "needle 7f3a"}, &out); res.IsError {
			t.Fatal(errText(res))
		}
		if len(out.Hits) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a fresh fact was not searchable within 3s: %+v", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	seen := map[string]string{}
	for _, h := range out.Hits {
		seen[h.Agent] = h.Name
	}
	if seen["test"] != "style" || seen["shared"] != "team" || out.ModeUsed != "keyword" {
		t.Fatalf("hits: %+v", out)
	}
	// Without the shared area only the agent's own fact is found; another
	// agent's memory is never searched by default.
	e.call(t, "memory_search", map[string]any{"query": "needle 7f3a", "include_shared": false}, &out)
	if len(out.Hits) != 1 || out.Hits[0].Agent != "test" {
		t.Fatalf("own facts only: %+v", out)
	}
	e.call(t, "memory_search", map[string]any{"query": "needle 7f3a", "agent": "codex", "include_shared": false}, &out)
	if len(out.Hits) != 1 || out.Hits[0].Name != "other" {
		t.Fatalf("another agent by name: %+v", out)
	}
	// index_status names the rule as builtin.
	var st index.Status
	if res := e.call(t, "index_status", map[string]any{"path": "/work/.agent/memory/test/facts/style.md"}, &st); res.IsError {
		t.Fatal(errText(res))
	}
	if st.RuleSource != "builtin" {
		t.Fatalf("%+v", st)
	}
}

func TestAgentNameIsNormalised(t *testing.T) {
	e, _, _ := newMemoryEnv(t, Options{}, nil, memoryConfig("/work/.agent"), nil)
	// A client that introduces itself as "Claude Code" gets claude-code.
	clientT, serverT := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = e.server.Run(ctx, serverT) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "Claude Code", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	cc := &env{server: e.server, fs: e.fs, fake: e.fake, up: e.up, j: e.j, session: session}
	var put memoryPutOutput
	if res := cc.call(t, "memory_put", map[string]any{"name": "style", "content": "x\n"}, &put); res.IsError {
		t.Fatal(errText(res))
	}
	if put.Agent != "claude-code" || put.Path != "/work/.agent/memory/claude-code/facts/style.md" {
		t.Fatalf("%+v", put)
	}
	// An explicit agent is validated like a fact name; an owner-qualified
	// one is refused in layout v1 with a pointer at the migration.
	for _, bad := range []string{"Codex", "..", "a b"} {
		res := cc.call(t, "memory_get", map[string]any{"name": "style", "agent": bad}, nil)
		if !res.IsError || !strings.Contains(errText(res), "lower-case") {
			t.Errorf("agent %q: IsError=%v %q", bad, res.IsError, errText(res))
		}
	}
	if res := cc.call(t, "memory_get", map[string]any{"name": "style", "agent": "a/b"}, nil); !res.IsError || !strings.Contains(errText(res), "memory migrate") {
		t.Errorf("agent a/b in v1: IsError=%v %q", res.IsError, errText(res))
	}
	res := cc.call(t, "memory_put", map[string]any{"name": "Bad Name", "content": "x"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "lower-case") {
		t.Fatalf("bad fact name: %v %s", res.IsError, errText(res))
	}
	// A configured override wins over the client name.
	o, _, _ := newMemoryEnv(t, Options{Agent: "openclaw"}, nil, memoryConfig("/work/.agent"), nil)
	if res := o.call(t, "memory_put", map[string]any{"name": "style", "content": "x\n"}, &put); res.IsError || put.Agent != "openclaw" {
		t.Fatalf("%+v %s", put, errText(res))
	}
}

func TestMemoryAgentFollowsTheTokenPrincipal(t *testing.T) {
	e, st, _ := newMemoryEnv(t, Options{}, &agent.Scope{}, memoryConfig("/work/.agent"), nil)
	var put memoryPutOutput
	if res := e.call(t, "memory_put", map[string]any{"name": "style", "content": "x\n"}, &put); res.IsError {
		t.Fatal(errText(res))
	}
	// Over stdio the client name decides.
	if put.Agent != "test" {
		t.Fatalf("%+v", put)
	}
	// Over HTTP with a token the token's own name decides, whatever SDK the
	// holder used to connect.
	tok, _ := mustCreateToken(t, e.agents, agent.TokenSpec{Name: "my-bot", Read: []string{"/work"}})
	ts := tokenServer(t, e.env, HTTPAuth{Verify: e.agents.VerifyToken})
	s, err := connectWith(t, ts.URL, tok)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "memory_put", Arguments: map[string]any{"name": "style", "content": "y\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatal(errText(res))
	}
	if !strings.Contains(errText(res), "/work/.agent/memory/my-bot/facts/style.md") {
		t.Fatalf("token principal name must pick the agent: %s", errText(res))
	}
	_ = st
}

// readText reads a whole file through the read_text tool.
func readText(t *testing.T, e *memEnv, p string) string {
	t.Helper()
	var out readTextOutput
	res := e.call(t, "read_text", map[string]any{"path": p}, &out)
	if res.IsError {
		if strings.Contains(errText(res), "does not exist") {
			return ""
		}
		t.Fatalf("read_text %s: %s", p, errText(res))
	}
	return out.Content
}

// TestMemoryToolsInLayoutV2KeyByOwner (T-56): in layout v2 the default
// agent is <owner>/<client>, an explicit owner/agent reads another
// person's memory, shared stays unowned, memory_get carries the drive's
// version once the fact is uploaded, and memory_put's
// expected_remote_version refuses a put after another device's write
// landed while accepting one behind this device's own pending upload.
func TestMemoryToolsInLayoutV2KeyByOwner(t *testing.T) {
	cfg := memoryConfig("/work/.agent")
	cfg.Layout = memory.LayoutV2
	e, _, _ := newMemoryEnv(t, Options{}, nil, cfg, nil)
	owner := agent.DefaultOwner()
	var put memoryPutOutput
	if res := e.call(t, "memory_put", map[string]any{"name": "style", "content": "mine\n"}, &put); res.IsError {
		t.Fatal(errText(res))
	}
	if put.Agent != owner+"/test" || put.Path != "/work/.agent/memory/"+owner+"/test/facts/style.md" || put.RemoteVersion != "" {
		t.Fatalf("v2 default agent: %+v", put)
	}
	if res := e.call(t, "memory_put", map[string]any{"name": "team", "content": "ours\n", "agent": "shared"}, &put); res.IsError || put.Path != "/work/.agent/memory/shared/facts/team.md" {
		t.Fatalf("shared in v2: %+v %s", put, errText(res))
	}
	if res := e.call(t, "memory_put", map[string]any{"name": "style", "content": "bob's\n", "agent": "bob/codex"}, &put); res.IsError || put.Agent != "bob/codex" {
		t.Fatalf("another owner's agent: %+v %s", put, errText(res))
	}
	e.drain(t)
	var got memory.Fact
	if res := e.call(t, "memory_get", map[string]any{"name": "style"}, &got); res.IsError {
		t.Fatal(errText(res))
	}
	if got.Agent != owner+"/test" || got.Content != "mine\n" || got.RemoteVersion == "" {
		t.Fatalf("memory_get after upload: %+v", got)
	}
	var list memoryListOutput
	if res := e.call(t, "memory_list", map[string]any{"agent": "bob/codex"}, &list); res.IsError || len(list.Facts) != 1 || list.Agent != "bob/codex" {
		t.Fatalf("list another owner: %+v %s", list, errText(res))
	}
	// Own pending write: remote version unchanged, accepted.
	e.up.Stop()
	if res := e.call(t, "memory_put", map[string]any{"name": "style", "content": "mine again\n", "expected_remote_version": got.RemoteVersion}, &put); res.IsError {
		t.Fatalf("own pending write refused: %s", errText(res))
	}
	// Another device's write lands: refused.
	e.fake.Seed(strings.TrimPrefix(got.Path, "/"), []byte("theirs\n"))
	dir, _ := e.fs.StatPath(context.Background(), "/work/.agent/memory/"+owner+"/test/facts")
	if err := e.fs.Refresh(context.Background(), dir.Ino); err != nil {
		t.Fatal(err)
	}
	res := e.call(t, "memory_put", map[string]any{"name": "style", "content": "third\n", "expected_remote_version": got.RemoteVersion}, nil)
	if !res.IsError || !strings.Contains(errText(res), "on the drive") {
		t.Fatalf("moved remote version accepted: %v %s", res.IsError, errText(res))
	}
}

// TestMemoryMergeNeverWritesWithoutConfirm: memory_merge proposes a
// merge of a fact with its conflict copy — three-way with the ancestor
// the agent passes, two-way without — and writes nothing: the fact, the
// copy and the provider are untouched until a memory_put adopts it.
func TestMemoryMergeNeverWritesWithoutConfirm(t *testing.T) {
	e, _, _ := newMemoryEnv(t, Options{}, nil, memoryConfig("/work/.agent"), nil)
	base := "a\nb\nc\n"
	var put memoryPutOutput
	if res := e.call(t, "memory_put", map[string]any{"name": "notes", "content": base}, &put); res.IsError {
		t.Fatal(errText(res))
	}
	e.drain(t)
	// Another device changes c while this device changes a: the drive
	// keeps this device's upload as a conflict copy beside theirs.
	e.fake.Seed(strings.TrimPrefix(put.Path, "/"), []byte("---\nname: notes\n---\na\nb\nC\n"))
	if res := e.call(t, "memory_put", map[string]any{"name": "notes", "content": "A\nb\nc\n"}, &put); res.IsError {
		t.Fatal(errText(res))
	}
	e.drain(t)
	dir, _ := e.fs.StatPath(context.Background(), "/work/.agent/memory/test/facts")
	if err := e.fs.Refresh(context.Background(), dir.Ino); err != nil {
		t.Fatal(err)
	}
	var got memory.Fact
	if res := e.call(t, "memory_get", map[string]any{"name": "notes"}, &got); res.IsError {
		t.Fatal(errText(res))
	}
	if len(got.Conflicts) != 1 {
		t.Fatalf("no conflict copy to merge: %+v", got)
	}
	// Reading the copy may download it once; nothing may be written.
	writes := func() int { return e.fake.Calls("Upload") + e.fake.Calls("UploadPart") + e.fake.Calls("Create") + e.fake.Calls("Delete") + e.fake.Calls("Rename") }
	before := writes()
	var res memory.MergeResult
	if r := e.call(t, "memory_merge", map[string]any{"name": "notes", "ancestor": base}, &res); r.IsError {
		t.Fatal(errText(r))
	}
	if !res.ThreeWay || !res.Clean || res.Merged != "A\nb\nC" || res.Version != got.Version {
		t.Fatalf("three-way merge: %+v", res)
	}
	var two memory.MergeResult
	if r := e.call(t, "memory_merge", map[string]any{"name": "notes"}, &two); r.IsError {
		t.Fatal(errText(r))
	}
	if two.ThreeWay || two.Clean || two.Conflicts != 2 || !strings.Contains(two.Merged, "<<<<<<<") {
		t.Fatalf("two-way merge: %+v", two)
	}
	if writes() != before {
		t.Fatalf("memory_merge wrote to the provider: %d calls", writes()-before)
	}
	again := memory.Fact{}
	if r := e.call(t, "memory_get", map[string]any{"name": "notes"}, &again); r.IsError || again.Version != got.Version || len(again.Conflicts) != 1 {
		t.Fatalf("memory_merge wrote something: %+v", again)
	}
	if r := e.call(t, "memory_merge", map[string]any{"name": "nothing-here"}, nil); !r.IsError {
		t.Fatal("merge of a missing fact succeeded")
	}
	if r := e.call(t, "memory_merge", map[string]any{"name": "notes", "conflict": "no-such-copy.md"}, nil); !r.IsError {
		t.Fatal("merge with an unknown copy succeeded")
	}
}
