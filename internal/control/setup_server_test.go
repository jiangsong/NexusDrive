package control

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"cloudfs/internal/config"
)

// Before there is anything to mount there is nothing to serve, and that was the
// first wall a new person hit: the control plane is where drives get added, but
// the daemon refuses to start without a configuration that already names a
// mount and a remote. The setup flow serves this same control plane with no
// filesystem, no journal and no cache behind it, so every screen it needs must
// answer without them rather than panic on a nil field.
func TestTheControlServerServesTheSetupScreenWithoutAMountedFilesystem(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "cache:\n  dir: " + filepath.Join(dir, "cache") + "\nremotes: {}\nmounts: []\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(&Collector{Config: cfg, Version: "setup-test"})
	srv.EnableUI()

	for _, target := range []string{"/status", "/accounts", "/pool/status", "/mounts", "/ui/app.js", "/"} {
		rr := accountRequest(t, srv, http.MethodGet, target, nil)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s = %d %s", target, rr.Code, rr.Body)
		}
	}
}
