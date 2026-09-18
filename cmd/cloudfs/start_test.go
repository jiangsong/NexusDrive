package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeStartConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The no-argument entry point is the product's front door, so it has to make
// sense from all three starting states rather than printing usage.
func TestStartActionFollowsTheSetupStage(t *testing.T) {
	dir := t.TempDir()
	mount := filepath.Join(dir, "mnt")
	usable := "remotes:\n  demo: { type: fake }\nmounts:\n  - path: " + mount +
		"\n    layout:\n      /: { remote: demo, root: root }\n"
	for _, tc := range []struct {
		name string
		path string
		want startAction
	}{
		{"no configuration at all", filepath.Join(dir, "absent", "config.yaml"), startSetup},
		{"a configuration with nothing mountable", writeStartConfig(t, t.TempDir(), "remotes: {}\nmounts: []\n"), startSetup},
		{"a configuration that names a real remote", writeStartConfig(t, t.TempDir(), usable), startMount},
	} {
		if got := startActionFor(tc.path); got != tc.want {
			t.Errorf("%s: startActionFor = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// `cloudfs mount` is what launchd and systemd run. A service that opens a
// browser on start is a defect, so the mount entry point never does, while the
// interactive entry points do.
func TestOnlyInteractiveEntriesOpenABrowser(t *testing.T) {
	for _, tc := range []struct {
		entry entry
		want  bool
	}{
		{entryMount, false},
		{entryStart, true},
	} {
		if got := opensBrowser(tc.entry); got != tc.want {
			t.Errorf("opensBrowser(%v) = %v, want %v", tc.entry, got, tc.want)
		}
	}
}

// Starting with no arguments migrates a pre-single-root installation before
// it does anything else, and says so, because the files a person had are
// somewhere new afterwards.
func TestStartMigratesTheLegacyLayoutAndReportsIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLOUDFS_CONFIG", "")
	legacyDir := filepath.Join(home, ".config", "cloudfs")
	legacyCache := filepath.Join(home, ".cache", "cloudfs")
	for _, d := range []string{legacyDir, filepath.Join(legacyCache, "journal")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(legacyCache, "journal", "journal.db"), []byte("j"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "config.yaml"),
		[]byte("cache:\n  dir: "+legacyCache+"\nremotes: {}\nmounts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	path, err := startConfigPath(parseFlags(nil), &out)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".cloudfs", "config.yaml"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".cloudfs", "journal", "journal.db")); err != nil {
		t.Errorf("the journal did not migrate: %v", err)
	}
	if !strings.Contains(out.String(), ".cloudfs") {
		t.Errorf("the move was not reported: %q", out.String())
	}
}

// An explicit --config is a path chosen for one command. Nothing is migrated
// underneath it, and the report stays silent.
func TestStartLeavesAnExplicitConfigAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLOUDFS_CONFIG", "")
	chosen := writeStartConfig(t, t.TempDir(), "remotes: {}\nmounts: []\n")
	var out strings.Builder
	path, err := startConfigPath(parseFlags([]string{"--config", chosen}), &out)
	if err != nil {
		t.Fatal(err)
	}
	if path != chosen {
		t.Errorf("path = %q, want %q", path, chosen)
	}
	if out.String() != "" {
		t.Errorf("reported a migration for an explicit --config: %q", out.String())
	}
}

// `cloudfs mount` is what a service unit runs, with no --config. It resolves
// the default path, which is now in the new root, so it has to perform the
// same one-time move; otherwise a person whose daemon is started by systemd
// or launchd wakes up to "no such file", with their real configuration still
// sitting in the old location.
func TestMountEntryMigratesTheLegacyLayoutToo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLOUDFS_CONFIG", "")
	legacyDir := filepath.Join(home, ".config", "cloudfs")
	legacyCache := filepath.Join(home, ".cache", "cloudfs")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(legacyCache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyCache, "meta.db"), []byte("m"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "config.yaml"),
		[]byte("cache:\n  dir: "+legacyCache+"\nremotes: {}\nmounts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The mount itself cannot succeed here — the configuration declares no
	// mounts — but the migration runs before anything is loaded, which is the
	// property under test.
	_ = runMount(t.Context(), nil, entryMount)
	for _, name := range []string{"config.yaml", "meta.db"} {
		if _, err := os.Stat(filepath.Join(home, ".cloudfs", name)); err != nil {
			t.Errorf("%s did not migrate from the mount entry: %v", name, err)
		}
	}
}

// A flag with no subcommand is the front door with an option, not an unknown
// command. The first smoke test of this entry point ran `cloudfs --no-open`
// and got the full usage text plus `unknown command "--no-open"`, for a flag
// the front door itself documents.
func TestFlagsWithNoSubcommandReachTheFrontDoor(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want bool
	}{
		{[]string{"cloudfs"}, true},
		{[]string{"cloudfs", "--no-open"}, true},
		{[]string{"cloudfs", "--config", "/tmp/x.yaml"}, true},
		{[]string{"cloudfs", "mount"}, false},
		{[]string{"cloudfs", "setup"}, false},
		// --help prints usage on purpose; that is the switch's job.
		{[]string{"cloudfs", "--help"}, false},
		{[]string{"cloudfs", "-h"}, false},
		{[]string{"cloudfs", "help"}, false},
	} {
		if got := isFrontDoor(tc.argv); got != tc.want {
			t.Errorf("isFrontDoor(%v) = %v, want %v", tc.argv[1:], got, tc.want)
		}
	}
}

// The front door rebuilds the command line for whichever handler it chose.
// parseFlags gives an unrecognized "--name" the next token as its value, so
// forwarding the raw arguments would let `cloudfs --force /mnt/drives` reach
// the mount path as a --force flag holding the mount point, and the mount
// would land at the configured default instead of where it was asked to.
func TestTheFrontDoorRebuildsArgumentsPerHandler(t *testing.T) {
	f := parseFlags([]string{"--force", "--no-open", "--listen", "127.0.0.1:1", "/mnt/drives"}, "force", "no-open")
	mount := mountArgs(f, "/cfg.yaml")
	if got := parseFlags(mount, "allow-other", "debug", "read-only", "foreground", "no-open"); got.arg(0) != "/mnt/drives" {
		t.Errorf("the mount point did not survive: %v -> %q", mount, got.arg(0))
	}
	for _, unwanted := range []string{"--force", "--listen"} {
		for _, a := range mount {
			if a == unwanted {
				t.Errorf("the mount path was handed %s, which it does not declare: %v", unwanted, mount)
			}
		}
	}
	setup := parseFlags(setupArgs(f, "/cfg.yaml"), "force", "no-open")
	if !setup.bool("force") || setup.str("listen", "") != "127.0.0.1:1" || setup.str("config", "") != "/cfg.yaml" {
		t.Errorf("the setup flags did not survive: %v", setupArgs(f, "/cfg.yaml"))
	}
}
