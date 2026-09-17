package control

import (
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/hooks"
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
	if resp.Context != "full" || resp.InstallCommand != "cloudfs hooks install" || resp.UninstallCommand != "cloudfs hooks uninstall" {
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
