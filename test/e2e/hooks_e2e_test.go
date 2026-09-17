package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/control"
	"cloudfs/internal/hooks"
)

// TestHookPromptInjectsChangedFiles is link (b) of the P1 acceptance
// (docs/agent-first-design.md §10.3) on a real mount and a real control
// socket: a terminal writes x.md → the turn-start hook's stdout names
// x.md as changed by someone else → the read hook counts an agent read
// of it in read_heat → the session-end hook finishes the hook session.
// A file the same client wrote through MCP in the meantime is not
// reported back to it as someone else's change; with hooks.context off
// the hook prints nothing but the session still exists.
func TestHookPromptInjectsChangedFiles(t *testing.T) {
	s := newStack(t, "writeback")
	s.cfg.Hooks.Context = "minimal"
	s.cfg.MCP.Allow = []string{"/"}
	coll := s.d.Collector()
	coll.PublishConfigView(s.cfg)
	socket := shortSocket(t)
	srv, err := control.NewServer(coll).Start(context.Background(), socket, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	api := hooks.NewClient(socket, "")
	event := `{"session_id": "hook-e2e-1", "cwd": "` + s.dir + `"}`

	// Turn one: the session is created; nothing has changed yet.
	var out bytes.Buffer
	if err := hooks.RunPrompt(context.Background(), strings.NewReader(event), &out, "claude", api); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "CloudFS mount") {
		t.Fatalf("first turn: %s", out.String())
	}
	// Between turns: the terminal writes x.md; the agent's own Write tool
	// writes own.md through the mount (a kernel write like any other) and
	// its post-write hook says so; the agent also writes mine.md through
	// MCP under a different client name.
	if err := os.WriteFile(filepath.Join(s.dir, "x.md"), []byte("changed by a person\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "own.md"), []byte("written by the agent's Write tool\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wrote := `{"session_id": "hook-e2e-1", "cwd": "` + s.dir + `", "tool_name": "Write", "tool_input": {"file_path": "` + filepath.Join(s.dir, "own.md") + `"}}`
	if err := hooks.RunWrite(context.Background(), strings.NewReader(wrote), "claude", api); err != nil {
		t.Fatal(err)
	}
	if own, err := s.d.Agent.HookWrites(context.Background(), "claude", "hook-e2e-1"); err != nil || len(own) != 1 || own[0] != "/own.md" {
		t.Fatalf("the write hook did not record the agent's own write: %v %v", own, err)
	}
	if res := s.callTool(t, "write_file", map[string]any{"path": "/mine.md", "content": "by the agent"}, nil); res.IsError {
		t.Fatal(toolText(res))
	}
	s.settle(t)
	waitChange(t, s.d.Agent, "/x.md")
	waitChange(t, s.d.Agent, "/own.md")
	waitChange(t, s.d.Agent, "/mine.md")
	out.Reset()
	if err := hooks.RunPrompt(context.Background(), strings.NewReader(event), &out, "claude", api); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Hook struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("second turn is not hook JSON: %v %s", err, out.String())
	}
	if !strings.Contains(payload.Hook.Context, "x.md") {
		t.Fatalf("the terminal's write was not injected: %s", payload.Hook.Context)
	}
	if strings.Contains(payload.Hook.Context, "own.md") {
		t.Fatalf("the agent's own write was handed back to it as someone else's: %s", payload.Hook.Context)
	}
	// The MCP session is a different client ("e2e"), so its write is a
	// change by someone else from the hook's point of view; only writes
	// by a session of the same client name are left out. Both are on the
	// record either way — the agent can ask history.
	if !strings.HasPrefix(payload.Hook.Context, "cloudfs:") {
		t.Fatalf("the injected text does not start with the data-not-instructions prefix: %q", payload.Hook.Context[:min(60, len(payload.Hook.Context))])
	}

	// The read hook: an agent read of x.md lands in read_heat as agent.
	read := `{"session_id": "hook-e2e-1", "cwd": "` + s.dir + `", "tool_name": "Read", "tool_input": {"file_path": "` + filepath.Join(s.dir, "x.md") + `"}}`
	if err := hooks.RunRead(context.Background(), strings.NewReader(read), "claude", api); err != nil {
		t.Fatal(err)
	}
	if err := s.d.ReadHeat.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	hot, err := s.d.Agent.HotPaths(context.Background(), "/", 7, 10)
	if err != nil {
		t.Fatal(err)
	}
	counted := false
	for _, h := range hot {
		counted = counted || (h.Path == "/x.md" && h.ByKind[agent.ReadByAgent] >= 1)
	}
	if !counted {
		t.Fatalf("the hook's read is not in read_heat as agent: %+v", hot)
	}

	// The session-end hook finishes the hook session.
	if err := hooks.RunStop(context.Background(), strings.NewReader(event), "claude", api); err != nil {
		t.Fatal(err)
	}
	m := agent.NewSessions(s.d.Agent, agent.SessionOptions{})
	list, _, err := m.List(context.Background(), agent.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var hook *agent.Session
	for i := range list {
		if list[i].Transport == agent.TransportHook {
			hook = &list[i]
		}
	}
	if hook == nil || hook.State != "finished" || hook.FinishedAt.IsZero() || hook.ClientName != "claude" {
		t.Fatalf("hook session after stop: %+v", hook)
	}
	if p, err := m.Principal(context.Background(), hook.PrincipalID); err != nil || p.Kind+":"+p.Name != agent.HookPrincipalName("claude") || !p.Scope.ReadOnly {
		t.Fatalf("hook principal: %+v %v", p, err)
	}

	// hooks.context off: nothing on stdout, but the turn still begins a session.
	s.cfg.Hooks.Context = "off"
	coll.PublishConfigView(s.cfg)
	out.Reset()
	off := `{"session_id": "hook-e2e-2", "cwd": "` + s.dir + `"}`
	if err := hooks.RunPrompt(context.Background(), strings.NewReader(off), &out, "claude", api); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "additionalContext") {
		t.Fatalf("context off still injected: %s", out.String())
	}
}

// waitChange polls the change record for a row naming p.
func waitChange(t *testing.T, st *agent.Store, p string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok, _ := st.LastWriter(context.Background(), p); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no change recorded for %s", p)
}

// shortSocket is a Unix socket path short enough for every platform.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cfs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "control.sock")
}
