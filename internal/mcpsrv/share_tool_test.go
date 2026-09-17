package mcpsrv

import (
	"context"
	"strings"
	"testing"

	"cloudfs/internal/agent"
)

// TestConsoleURLInArtifacts (T-55): with a console address configured,
// every artifact finish_session lists carries console_url — the render
// page, a link with no credential in it — beside the cloudfs:// URI; the
// share tool names the same page; without one the field is absent.
func TestConsoleURLInArtifacts(t *testing.T) {
	e, _ := newAgentEnv(t, Options{Workspace: "/work/.agent", ConsoleURL: "http://127.0.0.1:9101"}, agent.Scope{Read: []string{"/work"}})
	b := begin(t, e, map[string]any{})
	if res := e.call(t, "write_file", map[string]any{"path": b.Workspace + "/q3 report.md", "content": "# Q3"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	var f finishSessionOutput
	if res := e.call(t, "finish_session", map[string]any{"summary": "done"}, &f); res.IsError {
		t.Fatal(errText(res))
	}
	if len(f.Artifacts) != 1 {
		t.Fatalf("%+v", f)
	}
	a := f.Artifacts[0]
	want := "http://127.0.0.1:9101/#/fs/" + strings.TrimPrefix(b.Workspace, "/") + "/q3%20report.md"
	if a.ConsoleURL != want {
		t.Fatalf("console_url %q, want %q", a.ConsoleURL, want)
	}
	if strings.Contains(a.ConsoleURL, "token") || strings.Contains(a.ConsoleURL, "cfs_") {
		t.Fatalf("the console link carries a credential: %s", a.ConsoleURL)
	}
	plain, _ := newAgentEnv(t, Options{Workspace: "/work/.agent"}, agent.Scope{Read: []string{"/work"}})
	pb := begin(t, plain, map[string]any{})
	if res := plain.call(t, "write_file", map[string]any{"path": pb.Workspace + "/x.md", "content": "x"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	var pf finishSessionOutput
	if res := plain.call(t, "finish_session", map[string]any{}, &pf); res.IsError {
		t.Fatal(errText(res))
	}
	if len(pf.Artifacts) != 1 || pf.Artifacts[0].ConsoleURL != "" {
		t.Fatalf("without a console: %+v", pf.Artifacts)
	}
}

// TestShareToolFollowsThePolicy: the tool refuses without confirm, refuses
// an uncached file without a download, refuses credential-looking
// content unless forced, refuses a path outside the scope, and on success
// costs one CreateShare and names the console page.
func TestShareToolFollowsThePolicy(t *testing.T) {
	e, _ := newAgentEnv(t, Options{ConsoleURL: "http://127.0.0.1:9101"}, agent.Scope{Read: []string{"/work"}})
	e.fake.Seed("work/report.md", []byte("# Q3\n\nfine\n"))
	e.fake.Seed("work/env.md", []byte("password = hunter2hunter2\n"))
	e.fake.Seed("private/p.md", []byte("p\n"))
	e.listDirs(t, "/work", "/private")
	if res := e.call(t, "share", map[string]any{"path": "/work/report.md", "confirm": false}, nil); !res.IsError || !strings.Contains(errText(res), "confirm=true") {
		t.Fatalf("without confirm: %v %s", res.IsError, errText(res))
	}
	reads := e.fake.Calls("ReadRange")
	if res := e.call(t, "share", map[string]any{"path": "/work/report.md", "confirm": true}, nil); !res.IsError || !strings.Contains(errText(res), "pin") {
		t.Fatalf("uncached: %v %s", res.IsError, errText(res))
	}
	if e.fake.Calls("ReadRange") != reads || e.fake.Calls("CreateShare") != 0 {
		t.Fatal("a refused share downloaded or shared")
	}
	if res := e.call(t, "share", map[string]any{"path": "/private/p.md", "confirm": true}, nil); !res.IsError || !strings.Contains(errText(res), "outside the allowed") {
		t.Fatalf("outside the scope: %s", errText(res))
	}
	for _, p := range []string{"/work/report.md", "/work/env.md"} {
		if err := e.fs.Prefetch(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	var out shareOutput
	if res := e.call(t, "share", map[string]any{"path": "/work/report.md", "confirm": true, "expires": "24h"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if out.URL == "" || out.ConsoleURL != "http://127.0.0.1:9101/#/fs/work/report.md" || out.Reversible || e.fake.Calls("CreateShare") != 1 {
		t.Fatalf("share: %+v (CreateShare %d)", out, e.fake.Calls("CreateShare"))
	}
	res := e.call(t, "share", map[string]any{"path": "/work/env.md", "confirm": true}, &out)
	if !res.IsError || !strings.Contains(errText(res), "password-assignment") || strings.Contains(errText(res), "hunter2") {
		t.Fatalf("credential content: %v %s", res.IsError, errText(res))
	}
	if res := e.call(t, "share", map[string]any{"path": "/work/env.md", "confirm": true, "force": true}, &out); res.IsError || !out.Forced {
		t.Fatalf("forced: %+v %s", out, errText(res))
	}
	if res := e.call(t, "share", map[string]any{"path": "/work/report.md", "confirm": true, "expires": "soon"}, nil); !res.IsError {
		t.Fatal("bad expires accepted")
	}
}
