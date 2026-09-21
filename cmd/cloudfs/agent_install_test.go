package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/agent"
	"cloudfs/internal/integration"
	"cloudfs/internal/mcpsrv"
)

func writeAgentTestConfig(t *testing.T, home, name string) string {
	t.Helper()
	path := filepath.Join(home, name)
	body := "remotes:\n  fake:\n    type: fake\nmounts:\n  - path: " + filepath.Join(home, "mnt") + "\n    layout:\n      /work:\n        remote: fake\ncache:\n  dir: " + filepath.Join(home, "cache") + "\nmcp:\n  http: 127.0.0.1:1\n  allow: [/work]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAgentInstallStatusUninstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CLOUDFS_CONFIG", "")
	bin := filepath.Join(home, "bin")
	_ = os.MkdirAll(bin, 0700)
	for _, c := range []string{"codex", "claude"} {
		_ = os.WriteFile(filepath.Join(bin, c), []byte("#!/bin/sh\necho test-client-1\n"), 0700)
	}
	t.Setenv("PATH", bin)
	cfgPath := filepath.Join(home, "owner.yaml")
	body := "remotes:\n  fake:\n    type: fake\nmounts:\n  - path: " + filepath.Join(home, "mnt") + "\n    layout:\n      /work:\n        remote: fake\ncache:\n  dir: " + filepath.Join(home, "cache") + "\ncontrol:\n  socket: " + filepath.Join(home, "offline.sock") + "\n  metrics: ''\nmcp:\n  http: 127.0.0.1:1\n  allow: [/work]\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for i := 0; i < 2; i++ {
		if err := cmdAgent(context.Background(), []string{"install", "--config", cfgPath, "--client", "codex,claude", "--json"}, &out); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []string{"codex", "claude"} {
		s := integration.Inspect(home, c)
		if !s.SkillInstalled || !s.MCPConfigured || !s.HooksInstalled {
			t.Fatalf("%+v", s)
		}
		if integration.OwnerConfig(home, c) != cfgPath {
			t.Fatal("owner config lost")
		}
	}
	out.Reset()
	if err := cmdAgent(context.Background(), []string{"status", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out.Bytes()) || !strings.Contains(out.String(), `"owner_online":false`) || strings.Contains(out.String(), "cfs_") {
		t.Fatalf("unsafe status: %s", out.String())
	}
	cfg, _, err := loadConfig(parseFlags([]string{"--config", cfgPath}))
	if err != nil {
		t.Fatal(err)
	}
	st, err := agent.Open(filepath.Join(cfg.StateDir(), "agent"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tokens, err := st.Tokens(context.Background())
	if err != nil || len(tokens) != 2 {
		t.Fatalf("reinstall minted tokens: %d %v", len(tokens), err)
	}
	for _, tok := range tokens {
		if tok.Owner != agent.DefaultOwner() || len(tok.Scope.Read) != 1 || tok.Scope.Read[0] != "/work" {
			t.Fatalf("wrong principal: %+v", tok)
		}
	}
	if err := cmdAgent(context.Background(), []string{"uninstall", "--client", "codex,claude"}, &out); err != nil {
		t.Fatal(err)
	}
	tokens, _ = st.Tokens(context.Background())
	for _, tok := range tokens {
		if tok.RevokedAt.IsZero() {
			t.Fatal("credential not revoked")
		}
	}
}

func TestAgentUninstallFindsOwnerAfterAnUninstalledClient(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CLOUDFS_CONFIG", "")
	cfgPath := writeAgentTestConfig(t, home, "owner.yaml")
	snippet, err := mcpsrv.ClientConfigFor(mcpsrv.ClientOptions{Client: "claude", Transport: "http", URL: "http://127.0.0.1:1/", Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := integration.Apply(home, "claude", "test", snippet, false, cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := cmdAgent(context.Background(), []string{"uninstall", "--client", "codex,claude"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := integration.Inspect(home, "claude"); got.SkillInstalled || got.MCPConfigured || got.HooksInstalled {
		t.Fatalf("claude integration retained: %+v", got)
	}
}

func TestAgentUninstallReturnsMissingConfigError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	err := cmdAgent(context.Background(), []string{"uninstall", "--client", "claude", "--config", filepath.Join(home, "missing.yaml")}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no config file") {
		t.Fatalf("error = %v", err)
	}
}
