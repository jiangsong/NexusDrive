package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/config"
)

// `cloudfs setup` has three jobs and has to tell them apart before doing
// anything: write a starter file when there is none, serve the setup screen
// while the configuration is not yet mountable, and get out of the way once it
// is. Deciding that from the configuration alone keeps the decision testable
// without standing a server up.
func TestSetupStageDependsOnWhatTheConfigurationAlreadyHas(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "absent.yaml")
	if got := stageFor(missing); got != setupWriteStarter {
		t.Errorf("with no file at all, stage = %v, want a starter file to be written", got)
	}

	bare := filepath.Join(dir, "bare.yaml")
	if err := config.WriteStarter(bare); err != nil {
		t.Fatal(err)
	}
	if got := stageFor(bare); got != setupServe {
		t.Errorf("with a starter file, stage = %v, want the setup screen to be served", got)
	}

	ready := filepath.Join(dir, "ready.yaml")
	body := "cache:\n  dir: " + filepath.Join(dir, "cache") +
		"\nremotes:\n  nas: {type: webdav, url: 'https://nas.local/dav'}\nmounts:\n  - path: /mnt/cloud\n    layout:\n      /: {remote: nas}\n"
	if err := os.WriteFile(ready, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := stageFor(ready); got != setupAlreadyUsable {
		t.Errorf("with a mountable configuration, stage = %v, want setup to step aside", got)
	}
}

// A configuration that names a mount whose layout points at a remote that does
// not exist is not usable, however complete it looks. Setup must still help
// rather than hand the person to a daemon that will refuse to start.
func TestAConfigurationWhoseMountNamesNoRealRemoteStillNeedsSetup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "cache:\n  dir: " + filepath.Join(dir, "cache") + "\nremotes: {}\nmounts: []\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := stageFor(path); got != setupServe {
		t.Errorf("stage = %v, want the setup screen to be served", got)
	}
}

// The wizard is normally run against a config that is not at the default path
// — that is most of why it exists — and the restart at the end re-execs into
// `cloudfs mount`. Dropping --config there sends the daemon to a different file
// from the one just configured: the drives are added, the browser says it is
// done, and the mount that comes back is of whatever happened to be at the
// default path.
func TestTheRestartCarriesTheConfigTheWizardWasGiven(t *testing.T) {
	argv := setupRestartArgv([]string{"/usr/local/bin/cloudfs"}, "/tmp/scratch/config.yaml")
	if len(argv) < 2 || argv[1] != "mount" {
		t.Fatalf("argv = %v, want it to come back as a mount", argv)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--config /tmp/scratch/config.yaml") {
		t.Fatalf("argv = %v; the restart does not name the configuration the wizard wrote", argv)
	}
	if argv[0] != "/usr/local/bin/cloudfs" {
		t.Fatalf("argv[0] = %q, want the executable that was running", argv[0])
	}
}

func TestSetupListenOverrideCanRunBesideTheConfiguredDaemon(t *testing.T) {
	cfg := &config.Config{}
	cfg.Control.Socket = "/tmp/cloudfs-live.sock"
	cfg.Control.Metrics = "127.0.0.1:9101"

	socket, tcp := setupControlEndpoints(cfg, "127.0.0.1:9202")
	if socket != "" || tcp != "127.0.0.1:9202" {
		t.Fatalf("override endpoints = (%q, %q), want no socket and recovery TCP", socket, tcp)
	}
	socket, tcp = setupControlEndpoints(cfg, "")
	if socket != cfg.Control.Socket || tcp != cfg.Control.Metrics {
		t.Fatalf("configured endpoints = (%q, %q)", socket, tcp)
	}
}
