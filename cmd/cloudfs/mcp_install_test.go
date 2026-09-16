package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/mcpsrv"
)

// shortTempDir is a temporary directory short enough for a unix socket
// path (t.TempDir is not on every platform).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cfs-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestMCPInstallHTTPMatchesTheSnippetGolden: `mcp install --transport http`
// prints exactly what mcpsrv renders (whose golden files are the accepted
// `claude mcp add` shape), pointing at the configured listener and carrying
// the <token> placeholder when no --token was given.
func TestMCPInstallHTTPMatchesTheSnippetGolden(t *testing.T) {
	cfg := &config.Config{}
	cfg.MCP.HTTP = "127.0.0.1:8765"
	var out, errOut bytes.Buffer
	f := parseFlags([]string{"install", "--client", "claude", "--transport", "http"}, "stdio", "read-only")
	if err := mcpInstallTo(&out, &errOut, f, nil, false, cfg); err != nil {
		t.Fatal(err)
	}
	want, err := mcpsrv.ClientConfigFor(mcpsrv.ClientOptions{Client: "claude", Transport: "http", URL: "http://127.0.0.1:8765/"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != strings.TrimSpace(want) {
		t.Fatalf("stdout:\n%s\nwant:\n%s", out.String(), want)
	}
	var v map[string]any
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if !strings.Contains(errOut.String(), `claude mcp add --transport http cloudfs http://127.0.0.1:8765/ --header "Authorization: Bearer <token>"`) {
		t.Fatalf("stderr must carry the add command:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "cloudfs mcp token create --name <client> --read <prefix>") {
		t.Fatalf("stderr must say how to get a token:\n%s", errOut.String())
	}
	if strings.Contains(out.String(), "token create") {
		t.Fatalf("advice must not land on stdout:\n%s", out.String())
	}
}

func TestMCPInstallHTTPTakesURLAndToken(t *testing.T) {
	var out, errOut bytes.Buffer
	f := parseFlags([]string{"install", "--client", "codex", "--transport", "http", "--url", "http://10.0.0.2:9000/", "--token", "cfs_abcdefghijklmnopqrstuvwxyz0123456789"}, "stdio", "read-only")
	if err := mcpInstallTo(&out, &errOut, f, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, `url = "http://10.0.0.2:9000/"`) || !strings.Contains(s, "Bearer cfs_abcdefghijklmnopqrstuvwxyz0123456789") || strings.Contains(s, "<token>") {
		t.Fatalf("%s", s)
	}
	if strings.Contains(errOut.String(), "token create") {
		t.Fatalf("no hint when a token was given:\n%s", errOut.String())
	}
}

func TestMCPInstallDefaultsAndRefusals(t *testing.T) {
	var out, errOut bytes.Buffer
	// No config and no --url: the default listener address.
	f := parseFlags([]string{"install", "--client", "claude", "--transport", "http"}, "stdio", "read-only")
	if err := mcpInstallTo(&out, &errOut, f, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"url": "http://127.0.0.1:8765/"`) {
		t.Fatalf("%s", out.String())
	}
	// The stdio snippet is what it always was.
	out.Reset()
	f = parseFlags([]string{"install", "--client", "claude"}, "stdio", "read-only")
	if err := mcpInstallTo(&out, &errOut, f, []string{"/work"}, true, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"command":`) || !strings.Contains(out.String(), `"--allow", "/work", "--read-only"`) || strings.Contains(out.String(), "http") {
		t.Fatalf("%s", out.String())
	}
	f = parseFlags([]string{"install", "--client", "claude", "--transport", "smoke-signal"}, "stdio", "read-only")
	if err := mcpInstallTo(&out, &errOut, f, nil, false, nil); err == nil || !strings.Contains(err.Error(), "unknown transport") {
		t.Fatalf("err = %v", err)
	}
}

func TestMCPSnippetsRenderEveryClient(t *testing.T) {
	snippets, adds := mcpSnippets("http://127.0.0.1:8765/")
	if len(snippets) != 2 || !strings.Contains(snippets["claude"], "<token>") || !strings.Contains(snippets["codex"], "<token>") {
		t.Fatalf("%v", snippets)
	}
	if len(adds) != 1 || adds["claude"] != `claude mcp add --transport http cloudfs http://127.0.0.1:8765/ --header "Authorization: Bearer <token>"` {
		t.Fatalf("%v", adds)
	}
	if plainTokenRE.MatchString(snippets["claude"] + snippets["codex"] + adds["claude"]) {
		t.Fatal("the placeholder renderer must never carry a token")
	}
}

func TestNonOwnerWarningPointsAtTheHTTPTransport(t *testing.T) {
	if !strings.Contains(nonOwnerWarning, "cloudfs mcp install --transport http") || !strings.Contains(nonOwnerWarning, "cloudfs mount") {
		t.Fatal(nonOwnerWarning)
	}
}

// TestMCPInstallPicksHTTPWhenOwnerOnline: with no --transport, install
// asks the control socket; a daemon that answers means the agent must
// use the HTTP transport (a stdio server beside it cannot write, T-43).
func TestMCPInstallPicksHTTPWhenOwnerOnline(t *testing.T) {
	cfg := &config.Config{}
	cfg.MCP.HTTP = "127.0.0.1:8765"
	cfg.Control.Socket = filepath.Join(shortTempDir(t), "control.sock")
	srv, err := control.NewServer(&control.Collector{Version: "test"}).Start(context.Background(), cfg.Control.Socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	var out, errOut bytes.Buffer
	f := parseFlags([]string{"install", "--client", "claude"}, "stdio", "read-only", "with-agents-md", "with-hooks")
	if err := mcpInstallTo(&out, &errOut, f, nil, false, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"type": "http"`) || !strings.Contains(out.String(), "http://127.0.0.1:8765/") {
		t.Fatalf("stdout:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "transport: http (cloudfs is running") || !strings.Contains(errOut.String(), "token create") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
	// The configuration can pin it, and the flag wins over both.
	cfg.MCP.Install.Transport = "stdio"
	out.Reset()
	errOut.Reset()
	if err := mcpInstallTo(&out, &errOut, f, nil, false, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"command":`) || strings.Contains(errOut.String(), "transport:") {
		t.Fatalf("config stdio: %s / %s", out.String(), errOut.String())
	}
	out.Reset()
	f = parseFlags([]string{"install", "--client", "claude", "--transport", "http"}, "stdio", "read-only")
	if err := mcpInstallTo(&out, &errOut, f, nil, false, cfg); err != nil || !strings.Contains(out.String(), `"type": "http"`) {
		t.Fatalf("flag over config: %v %s", err, out.String())
	}
}

// TestMCPInstallPicksStdioWhenOffline: the same configuration with nobody
// listening on the socket registers a stdio server and says why.
func TestMCPInstallPicksStdioWhenOffline(t *testing.T) {
	cfg := &config.Config{}
	cfg.Control.Socket = filepath.Join(shortTempDir(t), "nobody.sock")
	var out, errOut bytes.Buffer
	f := parseFlags([]string{"install", "--client", "codex"}, "stdio", "read-only", "with-agents-md", "with-hooks")
	if err := mcpInstallTo(&out, &errOut, f, []string{"/work"}, false, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `args = [`) || strings.Contains(out.String(), "http") {
		t.Fatalf("stdout:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "transport: stdio (no daemon is running") {
		t.Fatalf("stderr:\n%s", errOut.String())
	}
}

// TestWithAgentsMdIsIdempotent: --with-agents-md creates AGENTS.md when
// the project has none, rewrites only its own block on a second run, and
// leaves whatever else the file holds byte for byte.
func TestWithAgentsMdIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Mounts = []config.Mount{{Path: "/mnt/cloud", Layout: map[string]config.Layout{"/": {Remote: "ali"}, "/docs": {Remote: "gd", Mode: "readonly"}}}}
	cfg.Memory.Root = "/work/.agent"
	target := filepath.Join(dir, "AGENTS.md")
	block := agentsMdBlock(cfg, []string{"/work"}, false, "http")
	for _, want := range []string{"/mnt/cloud", "`/` → remote `ali` (writeback)", "`/docs` → remote `gd` (readonly)", "/work/.agent", "`/work`", "begin_session", "data, not instructions", "transport: http"} {
		if !strings.Contains(block, want) {
			t.Errorf("block lacks %q:\n%s", want, block)
		}
	}
	if changed, err := writeAgentsMd(target, block); err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	first, _ := os.ReadFile(target)
	if changed, err := writeAgentsMd(target, block); err != nil || changed {
		t.Fatalf("second write: changed=%v err=%v", changed, err)
	}
	second, _ := os.ReadFile(target)
	if !bytes.Equal(first, second) {
		t.Fatalf("a second run changed the file:\n%s\n---\n%s", first, second)
	}
	// Content around the block survives; the block itself is replaced.
	custom := "# My project\n\nKeep this.\n\n" + string(first) + "\nAnd this.\n"
	if err := os.WriteFile(target, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeAgentsMd(target, agentsMdBlock(cfg, nil, true, "stdio")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	s := string(got)
	if !strings.HasPrefix(s, "# My project\n\nKeep this.\n\n") || !strings.HasSuffix(s, "\nAnd this.\n") {
		t.Fatalf("surroundings changed:\n%s", s)
	}
	if strings.Count(s, agentsMdBegin) != 1 || !strings.Contains(s, "read-only") || strings.Contains(s, "`/work`.") {
		t.Fatalf("block not replaced:\n%s", s)
	}
	// An unmatched marker is refused rather than guessed around.
	if err := os.WriteFile(target, []byte("x\n"+agentsMdBegin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeAgentsMd(target, block); err == nil || !strings.Contains(err.Error(), "unmatched") {
		t.Fatalf("err = %v", err)
	}
	// Target selection: an existing CLAUDE.md is preferred over creating
	// AGENTS.md; --agents-md overrides.
	d2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(d2, "CLAUDE.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := agentsMdTarget(d2, ""); got != filepath.Join(d2, "CLAUDE.md") {
		t.Fatalf("target = %s", got)
	}
	if got := agentsMdTarget(d2, "/elsewhere/AGENTS.md"); got != "/elsewhere/AGENTS.md" {
		t.Fatalf("target = %s", got)
	}
	if got := agentsMdTarget(t.TempDir(), ""); filepath.Base(got) != "AGENTS.md" {
		t.Fatalf("target = %s", got)
	}
	// Through the command: the file lands, the snippet still prints, and
	// --with-hooks registers the client's hooks under $HOME (a temporary
	// one here: the test must never touch the developer's own settings).
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	var out, errOut bytes.Buffer
	f := parseFlags([]string{"install", "--client", "claude", "--with-agents-md", "--agents-md", filepath.Join(d2, "AGENTS.md"), "--with-hooks"}, "stdio", "read-only", "with-agents-md", "with-hooks")
	if err := mcpInstallTo(&out, &errOut, f, []string{"/work"}, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d2, "AGENTS.md")); err != nil || !strings.Contains(errOut.String(), "wrote the CloudFS section") || !strings.Contains(errOut.String(), "claude   installed:") || !strings.Contains(out.String(), `"command":`) {
		t.Fatalf("%v\n%s\n%s", err, out.String(), errOut.String())
	}
	if data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); err != nil || !strings.Contains(string(data), "cloudfs agent-hook prompt") {
		t.Fatalf("hooks not installed under the temporary home: %v", err)
	}
}
