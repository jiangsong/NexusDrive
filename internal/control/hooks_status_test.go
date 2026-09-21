package control

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/hooks"
	"cloudfs/internal/integration"
)

// TestHooksRouteNeverWritesUserConfig: the route reports each client's
// registration, which are detected, which are verified, the context mode
// and the commands to copy — and leaves the home directory byte for byte
// as it found it, whichever method is tried.
func TestHooksRouteNeverWritesUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hooks.Install(home, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	cfg := config.Default()
	cfg.Hooks.Context = "full"
	f.coll.PublishConfigView(&cfg)
	s := NewServer(f.coll)
	before := treeDigest(t, home)

	w := call(t, s, "GET", "/agent/hooks", "")
	var resp HooksStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); w.Code != 200 || err != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if resp.Context != "full" {
		t.Fatalf("%+v", resp)
	}
	byClient := map[string]hooks.Status{}
	for _, c := range resp.Clients {
		byClient[c.Client] = c
	}
	if c := byClient["claude"]; !c.Present || c.Installed || !c.Verified {
		t.Fatalf("claude: %+v", c)
	}
	if c := byClient["codex"]; !c.Installed || c.Verified {
		t.Fatalf("codex: %+v", c)
	}
	if c := byClient["gemini"]; c.Present || c.Installed || c.Verified {
		t.Fatalf("gemini: %+v", c)
	}
	if len(resp.Detected) != 2 || resp.Detected[0] != "claude" || resp.Detected[1] != "codex" {
		t.Fatalf("detected: %v", resp.Detected)
	}
	if resp.MountsRegistry != hooks.MountsPath(home) {
		t.Fatalf("registry: %q", resp.MountsRegistry)
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		if w := call(t, s, method, "/agent/hooks", `{"client":"claude"}`); w.Code != 405 {
			t.Fatalf("%s: %d", method, w.Code)
		}
	}
	if after := treeDigest(t, home); after != before {
		t.Fatal("the route changed something under the home directory")
	}
}

func TestAgentIntegrationRouteInstallsAndUninstallsFromTheUI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, st, _, _ := hooksFixture(t)
	cfg := *f.coll.ConfigView()
	cfg.SourcePath = filepath.Join(f.dir, "config.yaml")
	f.coll.PublishConfigView(&cfg)
	f.coll.Version = "ui-test"
	f.coll.MCP = NewMCPView(st, func() MCPHTTPState {
		return MCPHTTPState{Addr: "127.0.0.1:1", Owner: true}
	}, nil)
	s := NewServer(f.coll)

	w := uiCallControl(t, s.Handler(), "POST", "/agent/integration/install", `{"clients":["claude"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("install: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "cfs_") {
		t.Fatal("install response exposed the clear credential")
	}
	got := integration.Inspect(home, "claude")
	if !got.SkillInstalled || !got.MCPConfigured || !got.HooksInstalled {
		t.Fatalf("not installed: %+v", got)
	}
	if w := uiCallControl(t, s.Handler(), "POST", "/agent/integration/uninstall", `{"clients":["claude"]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed uninstall: %d", w.Code)
	}
	w = uiCallControl(t, s.Handler(), "POST", "/agent/integration/uninstall", `{"clients":["claude"],"confirm":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("uninstall: %d %s", w.Code, w.Body.String())
	}
	got = integration.Inspect(home, "claude")
	if got.SkillInstalled || got.MCPConfigured || got.HooksInstalled {
		t.Fatalf("not uninstalled: %+v", got)
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil || !strings.Contains(string(b), `"theme": "dark"`) {
		t.Fatalf("user settings lost: %v %s", err, b)
	}
	principals, err := st.Tokens(context.Background())
	if err != nil || len(principals) != 1 || principals[0].RevokedAt.IsZero() {
		t.Fatalf("credential not revoked: %+v %v", principals, err)
	}
}

func TestAgentIntegrationRouteEnablesHTTPWithoutACommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(configPath, []byte("remotes: {}\nmounts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	f.coll.PublishConfigView(cfg)
	s := NewServer(f.coll)
	w := uiCallControl(t, s.Handler(), "POST", "/agent/integration/enable-http", `{}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"restart_required":true`) {
		t.Fatalf("enable HTTP: %d %s", w.Code, w.Body.String())
	}
	saved, err := config.Load(configPath)
	if err != nil || saved.MCP.HTTP != "127.0.0.1:8765" {
		t.Fatalf("HTTP setting: %q %v", saved.MCP.HTTP, err)
	}
}

// treeDigest hashes every file under root with its path and mode.
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h.Write([]byte(p + "|" + info.Mode().String() + "\n"))
		if !d.IsDir() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h.Write(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(h.Sum(nil))
}
