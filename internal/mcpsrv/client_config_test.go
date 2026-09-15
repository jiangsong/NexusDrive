package mcpsrv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// golden compares got with testdata/<name>; CLOUDFS_UPDATE_GOLDEN=1 rewrites
// the file. The snippets are what people paste into their client, so a
// change to them is a decision that shows up in the diff, not a side effect.
func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if os.Getenv("CLOUDFS_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("%s mismatch:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func TestClaudeHTTPSnippetGolden(t *testing.T) {
	got, err := ClientConfigFor(ClientOptions{Client: "claude", Transport: "http", URL: "http://127.0.0.1:8765/"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "claude_http.golden", got)
	var v struct {
		Servers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	srv := v.Servers["cloudfs"]
	if srv.Type != "http" || srv.URL != "http://127.0.0.1:8765/" || srv.Headers["Authorization"] != "Bearer <token>" {
		t.Fatalf("snippet parsed as %+v", srv)
	}
}

func TestClaudeAddCommandGolden(t *testing.T) {
	golden(t, "claude_http_add.golden", ClientAddCommand(ClientOptions{Client: "claude", Transport: "http", URL: "http://127.0.0.1:8765/"})+"\n")
	if got := ClientAddCommand(ClientOptions{Client: "codex", Transport: "http", URL: "http://127.0.0.1:8765/"}); got != "" {
		t.Fatalf("codex has no add command, got %q", got)
	}
	if got := ClientAddCommand(ClientOptions{Client: "claude", Binary: "/usr/local/bin/cloudfs"}); got != "" {
		t.Fatalf("stdio has no add command here, got %q", got)
	}
}

func TestCodexHTTPSnippetGolden(t *testing.T) {
	got, err := ClientConfigFor(ClientOptions{Client: "codex", Transport: "http", URL: "http://127.0.0.1:8765/"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "codex_http.golden", got)
}

func TestHTTPSnippetCarriesTheGivenToken(t *testing.T) {
	const token = "cfs_0123456789abcdefghijklmnopqrstuvwxyz"
	for _, client := range []string{"claude", "codex"} {
		got, err := ClientConfigFor(ClientOptions{Client: client, Transport: "http", URL: "http://127.0.0.1:8765/", Token: token})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "Bearer "+token) || strings.Contains(got, TokenPlaceholder) {
			t.Fatalf("%s: %s", client, got)
		}
	}
	cmd := ClientAddCommand(ClientOptions{Client: "claude", Transport: "http", URL: "http://127.0.0.1:8765/", Token: token})
	if !strings.Contains(cmd, "Bearer "+token) {
		t.Fatal(cmd)
	}
}

func TestHTTPSnippetRefusesBadInput(t *testing.T) {
	if _, err := ClientConfigFor(ClientOptions{Client: "claude", Transport: "http"}); err == nil {
		t.Fatal("no URL must be refused")
	}
	if _, err := ClientConfigFor(ClientOptions{Client: "claude", Transport: "carrier-pigeon", URL: "http://127.0.0.1:8765/"}); err == nil {
		t.Fatal("an unknown transport must be refused")
	}
	if _, err := ClientConfigFor(ClientOptions{Client: "emacs", Transport: "http", URL: "http://127.0.0.1:8765/"}); err == nil {
		t.Fatal("an unknown client must be refused")
	}
}

func TestStdioSnippetIsUnchanged(t *testing.T) {
	for _, client := range []string{"claude", "codex"} {
		old, err := ClientConfig(client, "/usr/local/bin/cloudfs", []string{"/work"}, true)
		if err != nil {
			t.Fatal(err)
		}
		now, err := ClientConfigFor(ClientOptions{Client: client, Binary: "/usr/local/bin/cloudfs", Allow: []string{"/work"}, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if old != now {
			t.Fatalf("%s: the stdio snippet changed:\n%s\n---\n%s", client, old, now)
		}
		if !strings.Contains(now, `"--allow", "/work", "--read-only"`) {
			t.Fatalf("%s: %s", client, now)
		}
	}
}

func TestNonOwnerOptionIsCarried(t *testing.T) {
	e := newEnv(t, Options{NonOwner: true})
	if !e.server.opt.NonOwner {
		t.Fatal("NonOwner was dropped by New")
	}
	if errRequiresOwner.Error() != "requires the storage owner; use the HTTP transport: cloudfs mcp install --transport http" {
		t.Fatalf("%v", errRequiresOwner)
	}
}
