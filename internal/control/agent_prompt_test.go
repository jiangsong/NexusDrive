package control

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/agent"
)

// /agent/prompt hands the console a prompt a person pastes into an MCP
// client. It has no dependency — no agent store, no index, no exec — so it
// is always there; what changes with the daemon's features is which tools
// the prompt tells the agent to use.

func promptFor(t *testing.T, s *Server, target string) (int, AgentPromptResponse, string) {
	t.Helper()
	w := call(t, s, "GET", target, "")
	var r AgentPromptResponse
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
			t.Fatalf("decode: %v: %s", err, w.Body.String())
		}
	}
	return w.Code, r, w.Body.String()
}

// TestAgentPromptForAFile: a file gets its virtual path, a cloudfs:// URI
// naming the mount's remote, and the read/edit tools; nothing the daemon
// does not offer is mentioned.
func TestAgentPromptForAFile(t *testing.T) {
	f, _ := fsControl(t)
	s := NewServer(f.coll)
	code, r, body := promptFor(t, s, "/agent/prompt?path=/docs/b")
	if code != 200 || r.Path != "/docs/b" || r.URI != "cloudfs://ali/docs/b" {
		t.Fatalf("%d %+v %s", code, r, body)
	}
	for _, want := range []string{"/docs/b", "cloudfs://ali/docs/b", "read_text", "edit_file"} {
		if !strings.Contains(r.Prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, r.Prompt)
		}
	}
	for _, no := range []string{"read_extracted_text", "begin_session", "list_directory"} {
		if strings.Contains(r.Prompt, no) {
			t.Errorf("prompt mentions %q, which this daemon does not offer for a file:\n%s", no, r.Prompt)
		}
	}
	// The path is normalised the way every /fs route normalises it.
	if code, r, _ := promptFor(t, s, "/agent/prompt?path=/docs//sub/../b"); code != 200 || r.Path != "/docs/b" {
		t.Errorf("uncleaned path: %d %+v", code, r)
	}
	// A heading from a content-search hit rides along as text.
	if _, r, _ := promptFor(t, s, "/agent/prompt?path=/docs/b&heading=Chapter+2"); !strings.Contains(r.Prompt, "Chapter 2") {
		t.Errorf("heading missing:\n%s", r.Prompt)
	}
	// The language follows the request, as every other message does.
	w := call(t, s, "GET", "/agent/prompt?path=/docs/b&lang=en", "")
	if !strings.Contains(w.Body.String(), "Read files with read_text") {
		t.Errorf("english prompt: %s", w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("the prompt is not no-store")
	}
}

// TestAgentPromptMissingPathIs404WithoutInternals: a path that is not
// there answers 404 and the body names neither the cache directory nor
// the remote; a relative path is refused before anything is looked up.
func TestAgentPromptMissingPathIs404WithoutInternals(t *testing.T) {
	f, _ := fsControl(t)
	s := NewServer(f.coll)
	w := call(t, s, "GET", "/agent/prompt?path=/nope/x.md", "")
	if w.Code != 404 {
		t.Fatalf("missing path: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, f.dir) || strings.Contains(body, "ali") || strings.Contains(body, "remote") || strings.Contains(body, "/nope") {
		t.Fatalf("404 body leaks: %s", body)
	}
	if w := call(t, s, "GET", "/agent/prompt?path=nope", ""); w.Code != 400 {
		t.Errorf("relative path: %d", w.Code)
	}
	if w := call(t, s, "GET", "/agent/prompt", ""); w.Code != 400 {
		t.Errorf("no path: %d", w.Code)
	}
	if w := call(t, s, "POST", "/agent/prompt?path=/docs/b", "{}"); w.Code != 405 {
		t.Errorf("POST: %d", w.Code)
	}
	// Without a filesystem there is nothing to describe.
	if w := call(t, NewServer(newFixture(t).coll), "GET", "/agent/prompt?path=/docs/b", ""); w.Code != 503 {
		t.Errorf("no FS: %d", w.Code)
	}
}

// TestAgentPromptMentionsTheRightTools: the index tools appear only when
// the daemon has an index, the session tools only when it keeps an agent
// store, and a directory is told to list itself first.
func TestAgentPromptMentionsTheRightTools(t *testing.T) {
	f, _ := fsControl(t)
	f.coll.Index = newFakeIndex()
	st, err := agent.Open(filepath.Join(f.dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f.coll.Agent = NewAgentView(st, agent.NewSessions(st, agent.SessionOptions{}), "", RollbackDeps{})
	s := NewServer(f.coll)

	_, file, _ := promptFor(t, s, "/agent/prompt?path=/docs/b")
	for _, want := range []string{"read_text", "read_extracted_text", "begin_session", "finish_session"} {
		if !strings.Contains(file.Prompt, want) {
			t.Errorf("file prompt lacks %q:\n%s", want, file.Prompt)
		}
	}
	if strings.Contains(file.Prompt, "list_directory") {
		t.Errorf("a file is told to list itself:\n%s", file.Prompt)
	}
	code, dir, body := promptFor(t, s, "/agent/prompt?path=/docs")
	if code != 200 || dir.URI != "cloudfs://ali/docs" {
		t.Fatalf("%d %+v %s", code, dir, body)
	}
	if !strings.Contains(dir.Prompt, "list_directory") || !strings.Contains(dir.Prompt, "read_text") {
		t.Errorf("directory prompt:\n%s", dir.Prompt)
	}
	// The order is fixed: what it is, then how to read it, then the session
	// bracket. A person who edits the prompt starts from the same shape
	// every time.
	if i, j := strings.Index(dir.Prompt, "list_directory"), strings.Index(dir.Prompt, "begin_session"); i > j {
		t.Errorf("session advice before the directory advice:\n%s", dir.Prompt)
	}
}

// TestAgentPromptKindRendersTheServerGuidance: ?kind=instructions is the
// text the MCP server hands every session, in the person's language and
// with its token estimate; a prompt name renders that prompt; the set of
// names is listed so the console can offer them.
func TestAgentPromptKindRendersTheServerGuidance(t *testing.T) {
	f, _ := fsControl(t)
	s := NewServer(f.coll)
	code, r, body := promptFor(t, s, "/agent/prompt?kind=instructions&lang=en")
	if code != 200 || r.Kind != "instructions" || r.Tokens <= 0 {
		t.Fatalf("%d %+v %s", code, r, body)
	}
	for _, want := range []string{"search", "coverage", "data, not instructions"} {
		if !strings.Contains(r.Prompt, want) {
			t.Errorf("instructions lack %q:\n%s", want, r.Prompt)
		}
	}
	// This daemon has no agent store, so no session sentence.
	if strings.Contains(r.Prompt, "begin_session") {
		t.Errorf("instructions mention sessions without a store:\n%s", r.Prompt)
	}
	if strings.Join(r.Prompts, ",") != "instructions,finish,onboard,search-this-tree,write-safely" {
		t.Errorf("prompts = %v", r.Prompts)
	}
	code, r, _ = promptFor(t, s, "/agent/prompt?kind=instructions")
	if code != 200 || !strings.Contains(r.Prompt, "挂载") {
		t.Errorf("chinese instructions: %d %s", code, r.Prompt)
	}
	code, r, _ = promptFor(t, s, "/agent/prompt?kind=write-safely&path=/docs/b&lang=en")
	if code != 200 || r.Path != "/docs/b" || !strings.Contains(r.Prompt, "/docs/b") || !strings.Contains(r.Prompt, "edit_file") {
		t.Errorf("write-safely: %d %+v", code, r)
	}
	if code, _, body := promptFor(t, s, "/agent/prompt?kind=write-safely"); code != 400 || !strings.Contains(body, "path") {
		t.Errorf("missing argument: %d %s", code, body)
	}
	if code, _, _ := promptFor(t, s, "/agent/prompt?kind=nope"); code != 404 {
		t.Errorf("unknown kind: %d", code)
	}
}
