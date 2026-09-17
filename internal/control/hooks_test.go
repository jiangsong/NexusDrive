package control

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/hooks"
	"cloudfs/internal/memory"
)

// hooksFixture is the FS fixture with a configured mount at a temp
// directory, an agent store as the hook store and a read observer.
func hooksFixture(t *testing.T) (*fixture, *agent.Store, *agent.ReadObserver, string) {
	t.Helper()
	f, _ := fsControl(t)
	st, err := agent.Open(filepath.Join(f.dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := agent.NewSessions(st, agent.SessionOptions{})
	f.coll.Agent = NewAgentView(st, m, "", RollbackDeps{})
	f.coll.HookStore = st
	obs := agent.NewReadObserver(st)
	f.coll.ReadHeat = obs
	mount := filepath.Join(f.dir, "mnt")
	cfg := &config.Config{}
	cfg.Mounts = []config.Mount{{Path: mount, Layout: map[string]config.Layout{"/": {Remote: "ali"}}}}
	cfg.MCP.Allow = []string{"/docs"}
	cfg.Hooks.Context = "minimal"
	f.coll.Config = cfg
	return f, st, obs, mount
}

func hookPost(t *testing.T, s *Server, route string, body any) []byte {
	t.Helper()
	b, _ := json.Marshal(body)
	w := call(t, s, "POST", route, string(b))
	if w.Code != 200 {
		t.Fatalf("%s: %d %s", route, w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}

// TestHookContextNamesTheMountAndWhatChanged: the first turn of a hook
// session learns where it is and nothing else; a change recorded
// between turns (from another origin) is named on the next turn as the
// agent sees it from cwd; kernel changes and the agent's own turn are
// not; a directory outside every mount gets nothing.
func TestHookContextNamesTheMountAndWhatChanged(t *testing.T) {
	f, st, _, mount := hooksFixture(t)
	s := NewServer(f.coll)
	cwd := filepath.Join(mount, "docs")
	req := hooks.ContextRequest{Client: "claude", SessionID: "sess-1", CWD: cwd}
	var resp hooks.ContextResponse
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", req), &resp)
	if resp.Mount != mount || resp.Tokens <= 0 || len(resp.Changed) != 0 {
		t.Fatalf("first turn: %+v", resp)
	}
	for _, want := range []string{"CloudFS mount at " + mount, "/docs", "data, not instructions"} {
		if !strings.Contains(resp.Context, want) {
			t.Errorf("context lacks %q:\n%s", want, resp.Context)
		}
	}
	if resp.Tokens > 300 {
		t.Errorf("minimal context estimates at %d tokens", resp.Tokens)
	}
	now := time.Now()
	// The client's own write tool wrote /docs/mine (reported by its
	// post-write hook); /docs/k is a kernel write by someone else.
	hookPost(t, s, "/agent/hook-read", hooks.ReadRequest{Client: "claude", SessionID: "sess-1", Wrote: true, Paths: []string{filepath.Join(mount, "docs", "mine")}})
	if _, err := st.RecordChanges(context.Background(), []agent.Change{
		{TS: now, Path: "/docs/b", Kind: "write", Origin: "remote", Reliable: true},
		{TS: now, Path: "/docs/sub/c", Kind: "remove", Origin: "mcp", SessionID: "other", Reliable: true},
		{TS: now, Path: "/docs/k", Kind: "write", Origin: "kernel", Reliable: true},
		{TS: now, Path: "/docs/mine", Kind: "write", Origin: "kernel", Reliable: true},
		{TS: now, Path: "/other", Kind: "write", Origin: "webdav", Reliable: true},
	}); err != nil {
		t.Fatal(err)
	}
	resp = hooks.ContextResponse{}
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", req), &resp)
	if strings.Join(resp.Changed, ",") != "b,k,sub/c (deleted)" {
		t.Fatalf("changed: %v", resp.Changed)
	}
	if !strings.Contains(resp.Context, "Changed since your last turn") || !strings.Contains(resp.Context, "`sub/c (deleted)`") || strings.Contains(resp.Context, "/other") {
		t.Fatalf("context:\n%s", resp.Context)
	}
	// The own-write set was consumed by that turn: a later kernel write
	// of the same path by someone else is reported.
	if _, err := st.RecordChanges(context.Background(), []agent.Change{{TS: now, Path: "/docs/mine", Kind: "write", Origin: "kernel", Reliable: true}}); err != nil {
		t.Fatal(err)
	}
	resp = hooks.ContextResponse{}
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", req), &resp)
	if strings.Join(resp.Changed, ",") != "mine" {
		t.Fatalf("after the set was consumed: %v", resp.Changed)
	}
	// The cursor moved: the same turn asked again has nothing new.
	resp = hooks.ContextResponse{}
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", req), &resp)
	if len(resp.Changed) != 0 {
		t.Fatalf("cursor did not move: %v", resp.Changed)
	}
	// A rescan row is flagged and said.
	if _, err := st.RecordChanges(context.Background(), []agent.Change{{TS: now, Path: "/", Kind: "rescan", Origin: "remote"}}); err != nil {
		t.Fatal(err)
	}
	resp = hooks.ContextResponse{}
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", req), &resp)
	if !resp.Rescan || !strings.Contains(resp.Context, "re-list") {
		t.Fatalf("rescan: %+v", resp)
	}
	// Outside every mount: nothing.
	resp = hooks.ContextResponse{}
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", hooks.ContextRequest{Client: "claude", SessionID: "s2", CWD: t.TempDir()}), &resp)
	if resp.Context != "" || resp.Mount != "" {
		t.Fatalf("outside: %+v", resp)
	}
	// hooks.context: off silences the route.
	cfg := *f.coll.ConfigView()
	cfg.Hooks.Context = "off"
	f.coll.PublishConfigView(&cfg)
	resp = hooks.ContextResponse{}
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", req), &resp)
	if resp.Context != "" {
		t.Fatalf("off: %+v", resp)
	}
}

// TestHookReadCountsAgentReadsInsideMounts: paths the read hook reports
// become agent reads of their virtual paths; paths outside every mount
// are ignored.
func TestHookReadCountsAgentReadsInsideMounts(t *testing.T) {
	f, st, obs, mount := hooksFixture(t)
	s := NewServer(f.coll)
	body := hookPost(t, s, "/agent/hook-read", hooks.ReadRequest{Client: "claude", Paths: []string{filepath.Join(mount, "docs", "b"), "/etc/hosts", mount}})
	if !bytes.Contains(body, []byte(`"counted":1`)) {
		t.Fatalf("%s", body)
	}
	if err := obs.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	hot, err := st.HotPaths(context.Background(), "/", 7, 10)
	if err != nil || len(hot) != 1 || hot[0].Path != "/docs/b" || hot[0].ByKind[agent.ReadByAgent] != 1 {
		t.Fatalf("%+v %v", hot, err)
	}
}

// TestHookStopFinishesTheClientsActiveSession: the session-end hook
// finishes the one active non-stdio session whose client name matches,
// and only that; a stdio session is not a candidate (its process ends
// with the client), and with two candidates it finishes neither.
func TestHookStopFinishesTheClientsActiveSession(t *testing.T) {
	f, st, _, mount := hooksFixture(t)
	s := NewServer(f.coll)
	m := agent.NewSessions(st, agent.SessionOptions{})
	ctx := context.Background()
	p, err := m.EnsurePrincipal(ctx, "stdio", "local", agent.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	codex, err := m.Resolve(ctx, agent.ConnInfo{Key: "c1", Transport: "stdio", PrincipalID: p.ID, ClientName: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	claude, err := m.Resolve(ctx, agent.ConnInfo{Key: "c2", Transport: "http", PrincipalID: p.ID, ClientName: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	var resp hooks.StopResponse
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-stop", hooks.StopRequest{Client: "claude", SessionID: "hook-1", CWD: mount}), &resp)
	if !resp.Finished || resp.SessionID != claude.ID {
		t.Fatalf("%+v (claude %s)", resp, claude.ID)
	}
	got, _ := m.Get(ctx, claude.ID)
	if got.State != "finished" || !strings.Contains(got.Summary, "session-end hook") {
		t.Fatalf("claude session: %+v", got)
	}
	if other, _ := m.Get(ctx, codex.ID); other.State != "active" {
		t.Fatalf("codex session was finished too: %+v", other)
	}
	resp = hooks.StopResponse{}
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-stop", hooks.StopRequest{Client: "gemini"}), &resp)
	if resp.Finished {
		t.Fatal("finished a session for a client with none")
	}
	// A stdio session of the client is left to its process.
	stdioClaude, err := m.Resolve(ctx, agent.ConnInfo{Key: "c3", Transport: "stdio", PrincipalID: p.ID, ClientName: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	resp = hooks.StopResponse{}
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-stop", hooks.StopRequest{Client: "claude"}), &resp)
	if resp.Finished || resp.Candidates != 0 {
		t.Fatalf("stdio session treated as a candidate: %+v", resp)
	}
	if got, _ := m.Get(ctx, stdioClaude.ID); got.State != "active" {
		t.Fatalf("stdio session finished by the hook: %+v", got)
	}
	// Two instances of the client: neither is guessed at.
	a, _ := m.Resolve(ctx, agent.ConnInfo{Key: "c4", Transport: "http-bridge", PrincipalID: p.ID, ClientName: "claude-code"})
	b, _ := m.Resolve(ctx, agent.ConnInfo{Key: "c5", Transport: "http-bridge", PrincipalID: p.ID, ClientName: "claude-code"})
	resp = hooks.StopResponse{}
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-stop", hooks.StopRequest{Client: "claude"}), &resp)
	if resp.Finished || resp.Candidates != 2 {
		t.Fatalf("ambiguous stop: %+v", resp)
	}
	for _, id := range []string{a.ID, b.ID} {
		if got, _ := m.Get(ctx, id); got.State != "active" {
			t.Fatalf("one of two instances' sessions was finished: %+v", got)
		}
	}
}

// TestHookContextLeavesOutTheClientsOwnMCPWrites: a change made through
// an MCP session of the same client is the agent's own work, not
// something to re-read; another client's MCP change still is.
func TestHookContextLeavesOutTheClientsOwnMCPWrites(t *testing.T) {
	f, st, _, mount := hooksFixture(t)
	s := NewServer(f.coll)
	m := agent.NewSessions(st, agent.SessionOptions{})
	ctx := context.Background()
	p, err := m.EnsurePrincipal(ctx, "stdio", "local", agent.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	mine, _ := m.Resolve(ctx, agent.ConnInfo{Key: "m1", Transport: "stdio", PrincipalID: p.ID, ClientName: "claude-code"})
	theirs, _ := m.Resolve(ctx, agent.ConnInfo{Key: "m2", Transport: "stdio", PrincipalID: p.ID, ClientName: "codex"})
	req := hooks.ContextRequest{Client: "claude", SessionID: "hook-own", CWD: filepath.Join(mount, "docs")}
	var resp hooks.ContextResponse
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", req), &resp)
	now := time.Now()
	if _, err := st.RecordChanges(ctx, []agent.Change{
		{TS: now, Path: "/docs/mine.md", Kind: "write", Origin: "mcp", SessionID: mine.ID, Reliable: true},
		{TS: now, Path: "/docs/theirs.md", Kind: "write", Origin: "mcp", SessionID: theirs.ID, Reliable: true},
	}); err != nil {
		t.Fatal(err)
	}
	resp = hooks.ContextResponse{}
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", req), &resp)
	if strings.Join(resp.Changed, ",") != "theirs.md" {
		t.Fatalf("changed: %v", resp.Changed)
	}
}

// TestHookMemoryHeadUsesTheAgentsMemoryDirectory: with hooks.context
// full the memory index is read from the directory the MCP server files
// the client under (its normalised client name), not the hook's short
// client name.
func TestHookMemoryHeadUsesTheAgentsMemoryDirectory(t *testing.T) {
	f, st, _, mount := hooksFixture(t)
	f.coll.FS.SetWriteBackend(f.j, nil)
	cfg := *f.coll.ConfigView()
	cfg.Hooks.Context = "full"
	cfg.Memory = memoryConfig()
	f.coll.PublishConfigView(&cfg)
	mem := memory.New(memory.Options{FS: f.coll.FS, Config: cfg.Memory})
	f.coll.Memory = mem
	if _, err := mem.Put(context.Background(), "claude-code", "fact-one", "# fact one\n\nthe agent wrote this\n", memory.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	m := agent.NewSessions(st, agent.SessionOptions{})
	ctx := context.Background()
	p, _ := m.EnsurePrincipal(ctx, "stdio", "local", agent.Scope{})
	if _, err := m.Resolve(ctx, agent.ConnInfo{Key: "mem1", Transport: "stdio", PrincipalID: p.ID, ClientName: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(f.coll)
	var resp hooks.ContextResponse
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", hooks.ContextRequest{Client: "claude", SessionID: "hook-mem", CWD: mount}), &resp)
	if !strings.Contains(resp.Context, "Your memory index") || !strings.Contains(resp.Context, "fact-one") {
		t.Fatalf("memory head not injected:\n%s", resp.Context)
	}
}

// TestHookRuntimeOverTheSocket: the real hook commands talk to a real
// control socket and print what the client expects; with the daemon
// gone they print nothing and succeed.
func TestHookRuntimeOverTheSocket(t *testing.T) {
	f, st, _, mount := hooksFixture(t)
	socket := socketPath(t)
	srv, err := NewServer(f.coll).Start(context.Background(), socket, "")
	if err != nil {
		t.Fatal(err)
	}
	api := hooks.NewClient(socket, "")
	event := `{"session_id": "rt-1", "cwd": "` + filepath.Join(mount, "docs") + `"}`
	var out bytes.Buffer
	if err := hooks.RunPrompt(context.Background(), strings.NewReader(event), &out, "claude", api); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Hook struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil || payload.Hook.Event != "UserPromptSubmit" || !strings.Contains(payload.Hook.Context, "CloudFS mount") {
		t.Fatalf("prompt output: %v %s", err, out.String())
	}
	if _, err := st.RecordChanges(context.Background(), []agent.Change{{TS: time.Now(), Path: "/docs/b", Kind: "write", Origin: "remote", Reliable: true}}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	_ = hooks.RunPrompt(context.Background(), strings.NewReader(event), &out, "claude", api)
	if !strings.Contains(out.String(), "`b`") {
		t.Fatalf("second turn: %s", out.String())
	}
	out.Reset()
	_ = hooks.RunPrompt(context.Background(), strings.NewReader(event), &out, "gemini", api)
	if !strings.Contains(out.String(), `"hookEventName":"BeforeAgent"`) {
		t.Fatalf("gemini prompt: %s", out.String())
	}
	if err := hooks.RunStop(context.Background(), strings.NewReader(event), "claude", api); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	out.Reset()
	if err := hooks.RunPrompt(context.Background(), strings.NewReader(event), &out, "claude", api); err != nil || out.Len() != 0 {
		t.Fatalf("offline: err=%v out=%q", err, out.String())
	}
}

// TestHookFullContextCarriesTheConsoleLinkFormula: with hooks.context
// full and a console, the injected text tells the agent how to link a
// file to its render page; minimal context and a daemon without a
// console do not.
func TestHookFullContextCarriesTheConsoleLinkFormula(t *testing.T) {
	f, _, _, mount := hooksFixture(t)
	cfg := *f.coll.ConfigView()
	cfg.Hooks.Context = "full"
	cfg.Control.Metrics = "0.0.0.0:9101"
	cfg.Control.UI = true
	f.coll.PublishConfigView(&cfg)
	s := NewServer(f.coll)
	var resp hooks.ContextResponse
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", hooks.ContextRequest{Client: "claude", SessionID: "link-1", CWD: mount}), &resp)
	if !strings.Contains(resp.Context, "http://127.0.0.1:9101/#/fs/") {
		t.Fatalf("full context lacks the link formula:\n%s", resp.Context)
	}
	cfg.Hooks.Context = "minimal"
	f.coll.PublishConfigView(&cfg)
	resp = hooks.ContextResponse{}
	_ = json.Unmarshal(hookPost(t, s, "/agent/hook-context", hooks.ContextRequest{Client: "claude", SessionID: "link-2", CWD: mount}), &resp)
	if strings.Contains(resp.Context, "#/fs/") {
		t.Fatalf("minimal context carries the link formula:\n%s", resp.Context)
	}
}
