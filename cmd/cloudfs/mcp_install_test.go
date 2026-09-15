package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/mcpsrv"
)

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
