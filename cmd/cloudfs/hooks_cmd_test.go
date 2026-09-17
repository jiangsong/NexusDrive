package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/hooks"
)

// TestHooksCommandInstallsStatusAndUninstalls drives the CLI end to end
// against a temporary home.
func TestHooksCommandInstallsStatusAndUninstalls(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A configuration whose mount path feeds the guard's registry.
	cfgPath := filepath.Join(home, "cloudfs.yaml")
	mount := filepath.Join(home, "cloud")
	if err := os.WriteFile(cfgPath, []byte("remotes:\n  ali:\n    type: fake\nmounts:\n  - path: "+mount+"\n    layout:\n      /:\n        remote: ali\ncache:\n  dir: "+filepath.Join(home, "cache")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdHooks(context.Background(), []string{"install", "--config", cfgPath}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "claude   installed:") {
		t.Fatalf("%s", out.String())
	}
	mounts, err := hooks.ReadMounts(hooks.MountsPath(home))
	if err != nil || len(mounts) != 1 || mounts[0] != mount {
		t.Fatalf("registry: %v %v", mounts, err)
	}
	out.Reset()
	if err := cmdHooks(context.Background(), []string{"status", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var st []hooks.Status
	if err := json.Unmarshal(out.Bytes(), &st); err != nil || len(st) != 3 || !st[0].Installed || st[1].Installed {
		t.Fatalf("%v %s", err, out.String())
	}
	out.Reset()
	if err := cmdHooks(context.Background(), []string{"uninstall", "--client", "claude"}, &out); err != nil || !strings.Contains(out.String(), "claude   removed:") {
		t.Fatalf("%v %s", err, out.String())
	}
	if err := cmdHooks(context.Background(), []string{"dance"}, &out); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
}

// TestHookSurvivesControlPlaneDown (T-54): with no daemon (and even no
// configuration) the hook runtime exits successfully and prints nothing.
func TestHookSurvivesControlPlaneDown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(home, "cloudfs.yaml")
	if err := os.WriteFile(cfgPath, []byte("remotes:\n  ali:\n    type: fake\nmounts:\n  - path: "+filepath.Join(home, "cloud")+"\n    layout:\n      /:\n        remote: ali\ncontrol:\n  socket: "+filepath.Join(home, "none.sock")+"\ncache:\n  dir: "+filepath.Join(home, "cache")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"prompt", "read", "stop"} {
		var out bytes.Buffer
		in := strings.NewReader(`{"session_id":"x","cwd":"` + home + `"}`)
		if err := cmdAgentHook(context.Background(), []string{ev, "--client", "claude", "--config", cfgPath}, in, &out); err != nil || out.Len() != 0 {
			t.Fatalf("%s: err=%v out=%q", ev, err, out.String())
		}
	}
	if err := cmdAgentHook(context.Background(), []string{"prompt", "--config", filepath.Join(home, "missing.yaml")}, strings.NewReader("{}"), &bytes.Buffer{}); err != nil {
		t.Fatalf("no config must still succeed: %v", err)
	}
}
