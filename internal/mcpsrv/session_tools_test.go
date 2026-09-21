package mcpsrv

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
)

func begin(t *testing.T, e *env, args map[string]any) beginSessionOutput {
	t.Helper()
	var out beginSessionOutput
	if res := e.call(t, "begin_session", args, &out); res.IsError {
		t.Fatalf("begin_session: %s", errText(res))
	}
	if out.SessionID == "" || out.Workspace == "" || !strings.HasPrefix(out.URI, "cloudfs://ali"+out.Workspace) {
		t.Fatalf("begin_session output: %+v", out)
	}
	return out
}

func readManifest(t *testing.T, e *env, manifest string) agent.Manifest {
	t.Helper()
	var rt readTextOutput
	if res := e.call(t, "read_text", map[string]any{"path": manifest}, &rt); res.IsError {
		t.Fatalf("read %s: %s", manifest, errText(res))
	}
	var m agent.Manifest
	if err := json.Unmarshal([]byte(rt.Content), &m); err != nil {
		t.Fatalf("manifest %s: %v\n%s", manifest, err, rt.Content)
	}
	return m
}

func TestBeginSessionCreatesADirectoryWithASkeletonManifest(t *testing.T) {
	e, _ := newAgentEnv(t, Options{Workspace: "/work/.agent"}, agent.Scope{Read: []string{"/work"}})
	b := begin(t, e, map[string]any{"name": "sort q3"})
	if !strings.HasPrefix(b.Workspace, "/work/.agent/test-") {
		t.Fatalf("workspace %q", b.Workspace)
	}
	var st1 statOutput
	if res := e.call(t, "stat", map[string]any{"path": b.Workspace}, &st1); res.IsError || st1.Kind != "directory" {
		t.Fatalf("session dir: %s %+v", errText(res), st1)
	}
	m := readManifest(t, e, path.Join(b.Workspace, "manifest.json"))
	if m.SessionID != b.SessionID || m.Client != "test" || m.Principal != "stdio:local" || m.FinishedAt != nil || m.Summary != "sort q3" || len(m.Artifacts) != 0 {
		t.Fatalf("skeleton: %+v", m)
	}
	// The explicit session is the connection's current session now: a
	// listing shows it active and the implicit one finished.
	var ls listSessionsOutput
	if res := e.call(t, "list_sessions", map[string]any{}, &ls); res.IsError {
		t.Fatal(errText(res))
	}
	if len(ls.Sessions) != 2 || ls.Sessions[0].ID != b.SessionID || ls.Sessions[0].State != "active" || ls.Sessions[1].State != "finished" {
		t.Fatalf("list_sessions: %+v", ls.Sessions)
	}
	if ls.Sessions[0].Workspace != b.Workspace {
		t.Fatalf("session row lacks its workspace: %+v", ls.Sessions[0])
	}
	if res := e.call(t, "list_sessions", map[string]any{"state": "paused"}, nil); !res.IsError {
		t.Fatal("bad state accepted")
	}
}

func TestFinishSessionWritesHandoffAsArtifact(t *testing.T) {
	e, _ := newAgentEnv(t, Options{Workspace: "/work/.agent"}, agent.Scope{Read: []string{"/work"}})
	b := begin(t, e, map[string]any{"name": "handoff"})
	var out finishSessionOutput
	if res := e.call(t, "finish_session", finishSessionInput{Handoff: "Continue with the parser tests.\n- inspect failures"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	want := path.Join(b.Workspace, "handoff.md")
	if out.Handoff != want || len(out.Artifacts) != 1 || out.Artifacts[0].Path != want {
		t.Fatalf("finish output: %+v", out)
	}
	var handoff readTextOutput
	if res := e.call(t, "read_text", readTextInput{Path: want}, &handoff); res.IsError {
		t.Fatal(errText(res))
	}
	if handoff.Content != "# Handoff\n\nContinue with the parser tests.\n- inspect failures\n" {
		t.Fatalf("handoff: %q", handoff.Content)
	}
	m := readManifest(t, e, out.Manifest)
	if len(m.Artifacts) != 1 || m.Artifacts[0].Path != want {
		t.Fatalf("manifest artifacts: %+v", m.Artifacts)
	}
}

func TestConcurrentSessionsKeepSeparateManifests(t *testing.T) {
	e1, _ := newAgentEnv(t, Options{Workspace: "/work/.agent"}, agent.Scope{Read: []string{"/work"}})
	e2 := secondClient(t, e1)
	b1 := begin(t, e1, map[string]any{})
	b2 := begin(t, e2, map[string]any{})
	if b1.Workspace == b2.Workspace || b1.SessionID == b2.SessionID {
		t.Fatalf("two sessions share a directory: %+v %+v", b1, b2)
	}
	if res := e1.call(t, "write_file", map[string]any{"path": b1.Workspace + "/out.md", "content": "one"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if res := e2.call(t, "write_file", map[string]any{"path": b2.Workspace + "/out.md", "content": "two"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	var f1, f2 finishSessionOutput
	if res := e1.call(t, "finish_session", map[string]any{"summary": "first"}, &f1); res.IsError {
		t.Fatal(errText(res))
	}
	if res := e2.call(t, "finish_session", map[string]any{"summary": "second"}, &f2); res.IsError {
		t.Fatal(errText(res))
	}
	if f1.SessionID != b1.SessionID || f2.SessionID != b2.SessionID {
		t.Fatalf("finished the wrong sessions: %+v %+v", f1, f2)
	}
	if len(f1.Artifacts) != 1 || len(f2.Artifacts) != 1 || f1.Artifacts[0].Path == f2.Artifacts[0].Path {
		t.Fatalf("%+v %+v", f1, f2)
	}
	for i, f := range []finishSessionOutput{f1, f2} {
		m := readManifest(t, e1, f.Manifest)
		if m.SessionID != f.SessionID || m.FinishedAt == nil || len(m.Artifacts) != 1 || !strings.HasPrefix(m.Artifacts[0].Path, path.Dir(f.Manifest)+"/") {
			t.Fatalf("manifest %s: %+v", f.Manifest, m)
		}
		a := m.Artifacts[0]
		if a.Path != path.Dir(f.Manifest)+"/out.md" || a.Size != 3 || a.State != "local" || a.URI != "cloudfs://ali"+a.Path || a.DownloadURL != "" {
			t.Fatalf("artifact %d: %+v", i, a)
		}
		if want := []string{"first", "second"}[i]; m.Summary != want {
			t.Fatalf("summary %q != %q", m.Summary, want)
		}
	}
}

func TestSandboxSessionCannotWriteOutsideItsDirectory(t *testing.T) {
	e, st := newAgentEnv(t, Options{Workspace: "/work/.agent"}, agent.Scope{Read: []string{"/work"}})
	e.fake.Seed("work/notes.md", []byte("keep"))
	b := begin(t, e, map[string]any{"sandbox": true})
	res := e.call(t, "write_file", map[string]any{"path": "/work/notes.md", "content": "x"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "outside the allowed") {
		t.Fatalf("sandbox write escaped: %v %s", res.IsError, errText(res))
	}
	if res := e.call(t, "write_file", map[string]any{"path": b.Workspace + "/ok.md", "content": "x"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if res := e.call(t, "create_directory", map[string]any{"path": b.Workspace + "/data"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	var rt readTextOutput
	if res := e.call(t, "read_text", map[string]any{"path": "/work/notes.md"}, &rt); res.IsError || rt.Content != "keep" {
		t.Fatalf("sandbox lost read access: %s %+v", errText(res), rt)
	}
	rows, _, err := st.Audit(context.Background(), agent.AuditQuery{Result: "denied"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Tool != "write_file" || len(rows[0].Paths) != 1 || rows[0].Paths[0] != "/work/notes.md" || rows[0].SessionID != b.SessionID {
		t.Fatalf("%+v", rows)
	}
	var ls listSessionsOutput
	e.call(t, "list_sessions", map[string]any{"state": "active"}, &ls)
	if len(ls.Sessions) != 1 || !ls.Sessions[0].Sandbox || ls.Sessions[0].Scope.Sandbox != b.Workspace {
		t.Fatalf("%+v", ls.Sessions)
	}
	// The directory is not an artifact; the file is.
	var f finishSessionOutput
	if res := e.call(t, "finish_session", map[string]any{}, &f); res.IsError {
		t.Fatal(errText(res))
	}
	if len(f.Artifacts) != 1 || f.Artifacts[0].Path != b.Workspace+"/ok.md" {
		t.Fatalf("%+v", f.Artifacts)
	}
	// After the sandbox session ends, the connection is back to the
	// principal's own scope.
	if res := e.call(t, "write_file", map[string]any{"path": "/work/notes.md", "content": "y"}, nil); res.IsError {
		t.Fatalf("write after finish: %s", errText(res))
	}
}

func TestShareSkipsLocalFilesWithoutALinkCall(t *testing.T) {
	e, _ := newAgentEnv(t, Options{Workspace: "/work/.agent"}, agent.Scope{Read: []string{"/work"}})
	b := begin(t, e, map[string]any{})
	// The uploader is never drained, so the file stays local.
	if res := e.call(t, "write_file", map[string]any{"path": b.Workspace + "/pending.md", "content": "not uploaded"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	start := time.Now()
	var f finishSessionOutput
	if res := e.call(t, "finish_session", map[string]any{"share": true}, &f); res.IsError {
		t.Fatal(errText(res))
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("finish_session took %v", took)
	}
	if len(f.Artifacts) != 1 || f.Artifacts[0].State != "local" || f.Artifacts[0].DownloadURL != "" || !f.Artifacts[0].ExpiresAt.IsZero() {
		t.Fatalf("%+v", f.Artifacts)
	}
	if n := e.fake.Calls("DownloadURL"); n != 0 {
		t.Fatalf("DownloadURL calls = %d", n)
	}
	if n := e.fake.Calls("ReadRange"); n != 0 {
		t.Fatalf("hashing a local file went to the provider: ReadRange calls = %d", n)
	}
	if f.Artifacts[0].SHA256 == "" {
		t.Fatalf("a local file is on disk and gets a checksum: %+v", f.Artifacts[0])
	}
	m := readManifest(t, e, f.Manifest)
	if len(m.Artifacts) != 1 || m.Artifacts[0].State != "local" || m.Artifacts[0].DownloadURL != "" {
		t.Fatalf("%+v", m.Artifacts)
	}
	raw, _ := json.Marshal(m.Artifacts[0])
	if strings.Contains(string(raw), "download_url") || strings.Contains(string(raw), "expires_at") {
		t.Fatalf("local artifact carries link fields: %s", raw)
	}
}

func TestShareLinksUploadedFiles(t *testing.T) {
	e, _ := newAgentEnv(t, Options{Workspace: "/work/.agent"}, agent.Scope{Read: []string{"/work"}})
	b := begin(t, e, map[string]any{})
	if res := e.call(t, "write_file", map[string]any{"path": b.Workspace + "/done.md", "content": "uploaded"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	e.drain(t)
	var f finishSessionOutput
	if res := e.call(t, "finish_session", map[string]any{"share": true, "summary": "shared"}, &f); res.IsError {
		t.Fatal(errText(res))
	}
	if len(f.Artifacts) != 1 || f.Artifacts[0].State != "synced" || f.Artifacts[0].DownloadURL == "" {
		t.Fatalf("%+v", f.Artifacts)
	}
	if f.Artifacts[0].SHA256 == "" {
		t.Fatalf("a cached artifact has no checksum: %+v", f.Artifacts[0])
	}
	if n := e.fake.Calls("DownloadURL"); n != 1 {
		t.Fatalf("DownloadURL calls = %d", n)
	}
	m := readManifest(t, e, f.Manifest)
	if len(m.Artifacts) != 1 || m.Artifacts[0].DownloadURL != f.Artifacts[0].DownloadURL || m.Summary != "shared" {
		t.Fatalf("%+v", m)
	}
}

func TestFinishWithoutShareNeverAsksForALink(t *testing.T) {
	e, _ := newAgentEnv(t, Options{Workspace: "/work/.agent"}, agent.Scope{Read: []string{"/work"}})
	b := begin(t, e, map[string]any{})
	e.call(t, "write_file", map[string]any{"path": b.Workspace + "/done.md", "content": "uploaded"}, nil)
	e.drain(t)
	var f finishSessionOutput
	if res := e.call(t, "finish_session", map[string]any{}, &f); res.IsError {
		t.Fatal(errText(res))
	}
	if len(f.Artifacts) != 1 || f.Artifacts[0].State != "synced" || f.Artifacts[0].DownloadURL != "" {
		t.Fatalf("%+v", f.Artifacts)
	}
	if n := e.fake.Calls("DownloadURL"); n != 0 {
		t.Fatalf("DownloadURL calls = %d", n)
	}
}

func TestBeginSessionWithoutWorkspaceIsAConfigError(t *testing.T) {
	e, _ := newAgentEnv(t, Options{}, agent.Scope{})
	res := e.call(t, "begin_session", map[string]any{}, nil)
	if !res.IsError || !strings.Contains(errText(res), "mcp.workspace") {
		t.Fatalf("%v %s", res.IsError, errText(res))
	}
	if n := e.fake.Calls("Mkdir"); n != 0 {
		t.Fatalf("Mkdir calls = %d", n)
	}
	var ls listSessionsOutput
	e.call(t, "list_sessions", map[string]any{}, &ls)
	if len(ls.Sessions) != 1 || ls.Sessions[0].Workspace != "" {
		t.Fatalf("a failed begin left a session: %+v", ls.Sessions)
	}
}

func TestBeginSessionWithoutAnAllowedWorkspaceIsDenied(t *testing.T) {
	// The workspace is configured outside what the caller may write.
	e, st := newAgentEnv(t, Options{Workspace: "/gd/.agent"}, agent.Scope{Read: []string{"/work"}})
	res := e.call(t, "begin_session", map[string]any{}, nil)
	if !res.IsError || !strings.Contains(errText(res), "outside the allowed") {
		t.Fatalf("%v %s", res.IsError, errText(res))
	}
	if n := e.fake.Calls("Mkdir"); n != 0 {
		t.Fatalf("Mkdir calls = %d", n)
	}
	rows, _, _ := st.Audit(context.Background(), agent.AuditQuery{Result: "denied"})
	if len(rows) != 1 || rows[0].Tool != "begin_session" || rows[0].Paths[0] != "/gd/.agent" {
		t.Fatalf("%+v", rows)
	}
}

func TestDefaultWorkspaceComesFromTheFirstAllowPrefix(t *testing.T) {
	e, _ := newAgentEnv(t, Options{}, agent.Scope{Read: []string{"/work", "/gd"}})
	b := begin(t, e, map[string]any{})
	if !strings.HasPrefix(b.Workspace, "/work/.agent/") {
		t.Fatalf("workspace %q", b.Workspace)
	}
}

func TestFinishSessionRefusesAnotherPrincipalsSession(t *testing.T) {
	e, st := newAgentEnv(t, Options{Workspace: "/work/.agent"}, agent.Scope{Read: []string{"/work"}})
	_, other, err := st.CreateToken(context.Background(), agent.TokenSpec{Name: "other", Read: []string{"/work"}})
	if err != nil {
		t.Fatal(err)
	}
	// Open a session for the other principal directly in the store.
	theirs, err := e.server.opt.Sessions.Resolve(context.Background(), agent.ConnInfo{Key: "token:" + other.ID, Transport: "http-token", PrincipalID: other.ID})
	if err != nil {
		t.Fatal(err)
	}
	res := e.call(t, "finish_session", map[string]any{"session_id": theirs.ID}, nil)
	if !res.IsError || !strings.Contains(errText(res), "another principal") {
		t.Fatalf("%v %s", res.IsError, errText(res))
	}
	if res := e.call(t, "finish_session", map[string]any{"session_id": "nope"}, nil); !res.IsError {
		t.Fatal("unknown session finished")
	}
	// The other principal's session is not in this principal's listing.
	var ls listSessionsOutput
	e.call(t, "list_sessions", map[string]any{}, &ls)
	for _, s := range ls.Sessions {
		if s.ID == theirs.ID {
			t.Fatalf("list_sessions leaked another principal's session: %+v", ls.Sessions)
		}
	}
	// Finishing twice is refused the second time.
	b := begin(t, e, map[string]any{})
	if res := e.call(t, "finish_session", map[string]any{"session_id": b.SessionID}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if res := e.call(t, "finish_session", map[string]any{"session_id": b.SessionID}, nil); !res.IsError || !strings.Contains(errText(res), "already finished") {
		t.Fatalf("%v %s", res.IsError, errText(res))
	}
}

func TestFinishImplicitSessionHasNoManifest(t *testing.T) {
	e, _ := newAgentEnv(t, Options{Workspace: "/work/.agent"}, agent.Scope{Read: []string{"/work"}})
	e.fake.Seed("work/seed.md", []byte("x"))
	if res := e.call(t, "write_file", map[string]any{"path": "/work/free.md", "content": "x"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	var f finishSessionOutput
	if res := e.call(t, "finish_session", map[string]any{"summary": "no box"}, &f); res.IsError {
		t.Fatal(errText(res))
	}
	if f.Manifest != "" || len(f.Artifacts) != 1 || f.Artifacts[0].Path != "/work/free.md" {
		t.Fatalf("%+v", f)
	}
}

func TestNonOwnerRefusesSessionTools(t *testing.T) {
	e, _ := newAgentEnv(t, Options{Workspace: "/work/.agent", NonOwner: true}, agent.Scope{Read: []string{"/work"}})
	for _, tool := range []string{"begin_session", "finish_session", "list_sessions"} {
		if res := e.call(t, tool, map[string]any{}, nil); !res.IsError || !strings.Contains(errText(res), "requires the storage owner") {
			t.Errorf("%s: %s", tool, errText(res))
		}
	}
	if n := e.fake.Calls("Mkdir"); n != 0 {
		t.Fatalf("Mkdir calls = %d", n)
	}
}

func TestSessionToolsAreAbsentWithoutASessionStore(t *testing.T) {
	e := newEnv(t, Options{})
	tools, err := e.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		switch tool.Name {
		case "begin_session", "finish_session", "list_sessions":
			t.Fatalf("%s registered without a session store", tool.Name)
		}
	}
}
