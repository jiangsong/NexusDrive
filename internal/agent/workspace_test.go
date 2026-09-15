package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSessionDirNameIsStableAndSafe(t *testing.T) {
	got := SessionDirName("Claude Code/1.0", time.Date(2026, 9, 14, 23, 0, 0, 0, time.UTC), "ab12cd34-ef56")
	if got != "claude-code-1-0-20260914-ab12cd34" {
		t.Fatalf("%q", got)
	}
	if got := SessionDirName("", time.Unix(0, 0).UTC(), "12345678"); got != "agent-19700101-12345678" {
		t.Fatalf("empty client: %q", got)
	}
	if got := SessionDirName("--Codex!!", time.Unix(0, 0).UTC(), "abc"); got != "codex-19700101-abc" {
		t.Fatalf("punctuation: %q", got)
	}
	// The date is the UTC day, so two machines in different zones agree.
	late := time.Date(2026, 9, 14, 23, 30, 0, 0, time.FixedZone("east", 3*3600))
	if got := SessionDirName("x", late, "12345678"); !strings.Contains(got, "-20260914-") {
		t.Fatalf("zone leaked into the date: %q", got)
	}
}

func TestDefaultWorkspace(t *testing.T) {
	if ws, err := DefaultWorkspace("/x/.box", Scope{}); err != nil || ws != "/x/.box" {
		t.Fatal(ws, err)
	}
	if ws, err := DefaultWorkspace("/x/.box/", Scope{Read: []string{"/work"}}); err != nil || ws != "/x/.box" {
		t.Fatalf("configured value is not normalised: %q %v", ws, err)
	}
	if ws, err := DefaultWorkspace("", Scope{Read: []string{"/work", "/gd"}}); err != nil || ws != "/work/.agent" {
		t.Fatal(ws, err)
	}
	if _, err := DefaultWorkspace("", Scope{}); !errors.Is(err, ErrWorkspaceUnset) {
		t.Fatal(err)
	}
	if _, err := DefaultWorkspace("", Scope{Read: []string{"/"}}); !errors.Is(err, ErrWorkspaceUnset) {
		t.Fatalf("root prefix: %v", err)
	}
	if !strings.Contains(ErrWorkspaceUnset.Error(), "mcp.workspace") {
		t.Fatalf("the error must name the setting: %v", ErrWorkspaceUnset)
	}
}

func TestArtifactPathsComeFromOkWritesOnly(t *testing.T) {
	rows := []AuditRow{
		{Tool: "write_file", Result: "ok", Paths: []string{"/w/s1/a.md"}},
		{Tool: "write_file", Result: "denied", Paths: []string{"/w/notes.md"}},
		{Tool: "move", Result: "ok", Paths: []string{"/w/s1/tmp.md", "/w/s1/b.md"}},
		{Tool: "delete", Result: "ok", Paths: []string{"/w/s1/old.md"}},
		{Tool: "read_text", Result: "ok", Paths: []string{"/w/other.md"}},
		{Tool: "write_file", Result: "ok", Paths: []string{"/w/s1/a.md"}},
		{Tool: "write_file", Result: "ok", Paths: []string{"/w/s1/manifest.json"}},
		{Tool: "write_file", Result: "error", Paths: []string{"/w/s1/failed.md"}},
		{Tool: "create_directory", Result: "ok", Paths: []string{}},
	}
	got := ArtifactPaths(rows, "/w/s1")
	if strings.Join(got, ",") != "/w/s1/a.md,/w/s1/b.md" {
		t.Fatalf("%v", got)
	}
	// Without a session directory nothing is excluded but nothing breaks.
	if got := ArtifactPaths(rows, ""); len(got) != 3 || got[2] != "/w/s1/manifest.json" {
		t.Fatalf("%v", got)
	}
}

func TestBeginSandboxNarrowsWriteOnly(t *testing.T) {
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	m := newSessions(t, &now)
	p, err := m.EnsurePrincipal(context.Background(), "stdio", "local", Scope{Read: []string{"/work"}})
	if err != nil {
		t.Fatal(err)
	}
	c := ConnInfo{Key: "stdio:0x9", Transport: "stdio", PrincipalID: p.ID, ClientName: "Claude Code", ClientVersion: "1.2"}
	implicit, err := m.Resolve(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Begin(context.Background(), implicit, BeginOptions{Name: "sort q3", Sandbox: true, Workspace: "/work/.agent"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.Workspace, "/work/.agent/claude-code-20260914-") || s.Workspace != "/work/.agent/"+SessionDirName("Claude Code", now, s.ID) {
		t.Fatalf("workspace %q", s.Workspace)
	}
	if !s.Sandbox || s.Scope.Sandbox != s.Workspace || s.Summary != "sort q3" || s.ClientName != "Claude Code" || s.ClientVersion != "1.2" {
		t.Fatalf("%+v", s)
	}
	if _, err := s.Scope.Check("/work/n.md", true); !errors.Is(err, ErrDenied) {
		t.Fatalf("write outside the sandbox: %v", err)
	}
	if _, err := s.Scope.Check(s.Workspace+"/out.md", true); err != nil {
		t.Fatalf("write inside the sandbox: %v", err)
	}
	if _, err := s.Scope.Check("/work/n.md", false); err != nil {
		t.Fatalf("sandbox lost read access: %v", err)
	}
	// The connection's implicit session is over and the next call lands
	// on the explicit one.
	if old, _ := m.Get(context.Background(), implicit.ID); old.State != "finished" {
		t.Fatalf("previous session: %+v", old)
	}
	if again, _ := m.Resolve(context.Background(), c); again.ID != s.ID {
		t.Fatalf("resolve after begin: %s != %s", again.ID, s.ID)
	}
	// A plain session begun from the sandboxed one is not sandboxed.
	plain, err := m.Begin(context.Background(), s, BeginOptions{Workspace: "/work/.agent"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Sandbox || plain.Scope.Sandbox != "" || plain.Workspace == s.Workspace {
		t.Fatalf("%+v", plain)
	}
	if _, err := plain.Scope.Check("/work/n.md", true); err != nil {
		t.Fatalf("plain session inherited the sandbox: %v", err)
	}
	if _, err := m.Begin(context.Background(), plain, BeginOptions{}); !errors.Is(err, ErrWorkspaceUnset) {
		t.Fatalf("begin without a workspace: %v", err)
	}
	if _, err := m.Begin(context.Background(), Session{}, BeginOptions{Workspace: "/w"}); err == nil {
		t.Fatal("begin without a connection succeeded")
	}
}

func TestBeginUsesTheTokenNameOverHTTP(t *testing.T) {
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	m := newSessions(t, &now)
	_, tok, err := m.Store().CreateToken(context.Background(), TokenSpec{Name: "codex-laptop", Read: []string{"/work"}})
	if err != nil {
		t.Fatal(err)
	}
	c := ConnInfo{Key: "token:" + tok.ID, Transport: "http-token", PrincipalID: tok.ID, ClientName: "generic-sdk"}
	implicit, err := m.Resolve(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Begin(context.Background(), implicit, BeginOptions{Workspace: "/work/.agent"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.Workspace, "/work/.agent/codex-laptop-20260914-") {
		t.Fatalf("workspace %q", s.Workspace)
	}
}

func TestFinishWithRecordsArtifactsAndKeepsTheLabel(t *testing.T) {
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	m := newSessions(t, &now)
	p, _ := m.EnsurePrincipal(context.Background(), "stdio", "local", Scope{Read: []string{"/work"}})
	implicit, _ := m.Resolve(context.Background(), ConnInfo{Key: "stdio:0x3", Transport: "stdio", PrincipalID: p.ID})
	s, err := m.Begin(context.Background(), implicit, BeginOptions{Name: "label", Workspace: "/work/.agent"})
	if err != nil {
		t.Fatal(err)
	}
	arts := []Artifact{{Path: s.Workspace + "/a.md", URI: "cloudfs://ali" + s.Workspace + "/a.md", Size: 3, State: "local"}}
	done, err := m.FinishWith(context.Background(), s.ID, "", arts)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != "finished" || done.Summary != "label" || len(done.Artifacts) != 1 || done.Artifacts[0].Path != arts[0].Path || done.FinishedAt.IsZero() {
		t.Fatalf("%+v", done)
	}
	if got, _ := m.Get(context.Background(), s.ID); len(got.Artifacts) != 1 || got.Artifacts[0].State != "local" {
		t.Fatalf("artifacts not stored: %+v", got)
	}
	// Once finished, a second finish changes nothing.
	if again, err := m.FinishWith(context.Background(), s.ID, "late", nil); err != nil || again.Summary != "label" || len(again.Artifacts) != 1 {
		t.Fatalf("%+v %v", again, err)
	}
	mine, _, _ := m.List(context.Background(), ListQuery{PrincipalID: p.ID})
	other, _, _ := m.List(context.Background(), ListQuery{PrincipalID: "nobody"})
	if len(mine) != 2 || len(other) != 0 {
		t.Fatalf("principal filter: mine=%d other=%d", len(mine), len(other))
	}
	if _, err := m.FinishWith(context.Background(), "missing", "", nil); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal(err)
	}
}

func TestManifestJSONHasTheDocumentedShape(t *testing.T) {
	started := time.Date(2026, 9, 14, 10, 2, 11, 0, time.UTC)
	s := Session{
		ID: "3f9a01c2-0000", ClientName: "claude-code", StartedAt: started, FinishedAt: started.Add(time.Minute),
		Scope:     Scope{Read: []string{"/work"}, Sandbox: "/work/.agent/claude-code-20260914-3f9a01c2"},
		Summary:   "done",
		Artifacts: []Artifact{{Path: "/work/.agent/claude-code-20260914-3f9a01c2/report.md", URI: "cloudfs://ali/work/.agent/claude-code-20260914-3f9a01c2/report.md", Size: 18233, State: "synced"}},
	}
	b, err := ManifestOf(s, "token:codex-laptop").JSON()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"session_id", "client", "principal", "started_at", "finished_at", "scope", "summary", "artifacts"} {
		if _, ok := m[key]; !ok {
			t.Errorf("manifest lacks %q: %s", key, b)
		}
	}
	arts := m["artifacts"].([]any)
	a := arts[0].(map[string]any)
	for _, key := range []string{"path", "uri", "size", "state"} {
		if _, ok := a[key]; !ok {
			t.Errorf("artifact lacks %q: %s", key, b)
		}
	}
	if _, ok := a["download_url"]; ok {
		t.Errorf("an unshared artifact must not carry download_url: %s", b)
	}
	if m["principal"] != "token:codex-laptop" || m["finished_at"] != "2026-09-14T10:03:11Z" {
		t.Fatalf("%s", b)
	}
	// An unfinished session has no finished_at and an empty (not null)
	// artifact list.
	b, _ = ManifestOf(Session{ID: "x", StartedAt: started}, "stdio:local").JSON()
	if strings.Contains(string(b), "finished_at") || !strings.Contains(string(b), `"artifacts": []`) {
		t.Fatalf("%s", b)
	}
	if ManifestPath("/w/s1/") != "/w/s1/manifest.json" {
		t.Fatal(ManifestPath("/w/s1/"))
	}
}
