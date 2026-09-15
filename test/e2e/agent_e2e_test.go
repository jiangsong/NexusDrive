package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"cloudfs/internal/agent"
	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	"cloudfs/internal/mcpsrv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bearer stamps one access token on every request of an HTTP client, the
// way an agent's MCP client configuration does.
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// beginSessionOutput mirrors the begin_session tool's structured result.
type beginSessionOutput struct {
	SessionID string `json:"session_id"`
	Workspace string `json:"workspace"`
	URI       string `json:"uri"`
}

// TestAgentTokenSandboxChainAndAuditInTheBrowser is the whole of phase one
// line A in one run: an access token scoped to /work admits an HTTP client;
// the client begins a sandboxed session, delivers two files into it and is
// refused one write outside it; finishing the session records the two
// artifacts; the control API reports the refusal; and the console, rendered
// in a real browser, shows the denied row on the audit tab and the two
// artifact rows on the session panel.
func TestAgentTokenSandboxChainAndAuditInTheBrowser(t *testing.T) {
	s := newUnmountedStack(t, stackOptions{})
	ctx := context.Background()
	s.fake.Seed("work/notes.md", []byte("keep me"))

	// The token is created through the store, which is what both the CLI
	// and POST /mcp/tokens call.
	plain, _, err := s.d.Agent.CreateToken(ctx, agent.TokenSpec{Name: "e2e", Read: []string{"/work"}})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := mcpsrv.New(mcpsrv.Options{FS: s.d.FS, Sessions: s.d.Sessions, Workspace: "/work/.agent", Version: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(mcpsrv.NewHTTPHandler(srv, mcpsrv.HTTPAuth{Verify: s.d.Agent.VerifyToken}))
	defer ts.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "codex", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: ts.URL, HTTPClient: &http.Client{Transport: bearer{plain}},
		DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	call := func(name string, args map[string]any, out any) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out != nil && !res.IsError && res.StructuredContent != nil {
			b, _ := json.Marshal(res.StructuredContent)
			if err := json.Unmarshal(b, out); err != nil {
				t.Fatalf("%s: decode: %v", name, err)
			}
		}
		return res
	}

	var begun beginSessionOutput
	if res := call("begin_session", map[string]any{"sandbox": true}, &begun); res.IsError {
		t.Fatalf("begin_session: %s", toolText(res))
	}
	if begun.SessionID == "" || !strings.HasPrefix(begun.Workspace, "/work/.agent/") {
		t.Fatalf("begin_session returned %+v", begun)
	}
	for _, name := range []string{"a.md", "b.md"} {
		if res := call("write_file", map[string]any{"path": begun.Workspace + "/" + name, "content": name}, nil); res.IsError {
			t.Fatalf("write %s: %s", name, toolText(res))
		}
	}
	// The sandbox: a path the token may write is refused while the session
	// is confined to its directory.
	if res := call("write_file", map[string]any{"path": "/work/notes.md", "content": "overwrite"}, nil); !res.IsError {
		t.Fatal("sandbox escaped: a write outside the session directory succeeded")
	}
	var fin struct {
		Artifacts []agent.Artifact `json:"artifacts"`
	}
	if res := call("finish_session", map[string]any{"summary": "e2e"}, &fin); res.IsError {
		t.Fatalf("finish_session: %s", toolText(res))
	}
	if len(fin.Artifacts) != 2 {
		t.Fatalf("finish_session listed %d artifacts: %+v", len(fin.Artifacts), fin.Artifacts)
	}
	if got, err := s.d.FS.ReadFileRange(ctx, "/work/notes.md", 0, 64); err != nil || string(got) != "keep me" {
		t.Fatalf("the denied write changed the file: %q %v", got, err)
	}

	// The control API: the refusal is on record, the session carries its
	// two artifacts and says it was sandboxed.
	h := control.NewServer(s.d.Collector()).Handler()
	if w := uiCall(t, h, "GET", "/audit?result=denied", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"/work/notes.md"`) {
		t.Fatalf("audit: %d %s", w.Code, w.Body.String())
	}
	var detail control.SessionDetail
	w := uiCall(t, h, "GET", "/sessions/"+begun.SessionID, "")
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || len(detail.Artifacts) != 2 || !detail.Session.Sandbox {
		t.Fatalf("session detail: %v %s", err, w.Body.String())
	}

	// The console in a real browser.
	chrome := requireBrowser(t)
	base := startControlUI(t, s.d.Collector())
	if dom := renderedDOM(t, chrome, base+"/?lang=en#/agents?tab=audit", `data-result="denied"`); !strings.Contains(dom, `data-result="denied"`) || !strings.Contains(dom, "/work/notes.md") {
		t.Fatalf("the audit tab shows no denied row:\n%s", dom)
	}
	if dom := renderedDOM(t, chrome, base+"/?lang=en#/agents?session="+begun.SessionID, "data-artifact="); strings.Count(dom, "data-artifact=") != 2 {
		t.Fatalf("the session panel does not list two artifacts:\n%s", dom)
	}
}

// TestAgentSessionManifestMatchesTheMount needs a kernel mount: what the
// session's manifest.json says it delivered is what `ls` of the session
// directory shows at the mount point.
func TestAgentSessionManifestMatchesTheMount(t *testing.T) {
	s := newStackWith(t, "writeback", stackOptions{mcp: func(d *daemon.Daemon, o *mcpsrv.Options) {
		o.Sessions, o.Workspace = d.Sessions, "/.agent"
	}})
	var b beginSessionOutput
	if res := s.callTool(t, "begin_session", map[string]any{}, &b); res.IsError {
		t.Fatalf("begin_session: %s", toolText(res))
	}
	for _, name := range []string{"one.md", "two.md"} {
		if res := s.callTool(t, "write_file", map[string]any{"path": b.Workspace + "/" + name, "content": name}, nil); res.IsError {
			t.Fatalf("write %s: %s", name, toolText(res))
		}
	}
	if res := s.callTool(t, "finish_session", map[string]any{}, nil); res.IsError {
		t.Fatalf("finish_session: %s", toolText(res))
	}
	s.settle(t)

	dir := filepath.Join(s.dir, strings.TrimPrefix(b.Workspace, "/"))
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m agent.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest.json: %v\n%s", err, raw)
	}
	if m.SessionID != b.SessionID || m.FinishedAt == nil {
		t.Fatalf("manifest is not the finished session's: %+v", m)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, e := range entries {
		if e.Name() != "manifest.json" {
			listed = append(listed, b.Workspace+"/"+e.Name())
		}
	}
	var inManifest []string
	for _, a := range m.Artifacts {
		inManifest = append(inManifest, a.Path)
	}
	sort.Strings(listed)
	sort.Strings(inManifest)
	if len(listed) != 2 || strings.Join(listed, ",") != strings.Join(inManifest, ",") {
		t.Fatalf("ls %v manifest %v", listed, inManifest)
	}
}
