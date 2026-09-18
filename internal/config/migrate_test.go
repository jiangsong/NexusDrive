package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacyTree lays down what an installation looked like before the single
// root: the configuration under ~/.config/cloudfs and everything else,
// unrecoverable state included, under ~/.cache/cloudfs.
func legacyTree(t *testing.T, home string) (configPath, cacheDir string) {
	t.Helper()
	configDir := filepath.Join(home, ".config", "cloudfs")
	cacheDir = filepath.Join(home, ".cache", "cloudfs")
	for _, d := range []string{configDir, cacheDir,
		filepath.Join(cacheDir, "journal"), filepath.Join(cacheDir, "agent"),
		filepath.Join(cacheDir, "pool"), filepath.Join(cacheDir, "blocks"),
		filepath.Join(cacheDir, "secrets")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []struct{ path, body string }{
		{filepath.Join(cacheDir, "meta.db"), "meta"},
		{filepath.Join(cacheDir, "index.db"), "index"},
		{filepath.Join(cacheDir, "exports.db"), "exports"},
		{filepath.Join(cacheDir, "journal", "journal.db"), "journal"},
		{filepath.Join(cacheDir, "agent", "agent.db"), "agent"},
		{filepath.Join(cacheDir, "pool", "pool-home.db"), "pool"},
		{filepath.Join(cacheDir, "blocks", "00"), "block"},
		{filepath.Join(cacheDir, "secrets", "ali"), "secret"},
	} {
		if err := os.WriteFile(f.path, []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	configPath = filepath.Join(configDir, "config.yaml")
	body := "# keep this comment\ncache:\n  dir: " + cacheDir + "\nremotes: {}\nmounts: []\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, cacheDir
}

// The state a person cannot re-download moves into the new root; the blocks,
// which can always be fetched again and may be tens of gigabytes, stay where
// they are and are named explicitly by the migrated configuration.
func TestMigrateMovesStateAndLeavesTheBlocksInPlace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, cacheDir := legacyTree(t, home)
	newPath := filepath.Join(home, ".cloudfs", "config.yaml")

	m, err := MigrateLegacyLayout(newPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Ran {
		t.Fatal("migration did not run although the legacy layout was present")
	}
	root := filepath.Join(home, ".cloudfs")
	for _, name := range []string{"config.yaml", "meta.db", "index.db", "exports.db",
		filepath.Join("journal", "journal.db"), filepath.Join("agent", "agent.db"),
		filepath.Join("pool", "pool-home.db"), filepath.Join("secrets", "ali")} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("%s did not move: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "blocks", "00")); err != nil {
		t.Errorf("blocks were moved; a large cache must stay put: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "blocks")); err == nil {
		t.Error("blocks were copied into the new root")
	}
	cfg, err := Load(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BlockCacheDir() != cacheDir {
		t.Errorf("cache.dir = %q, want the legacy cache %q; the blocks would be refetched", cfg.BlockCacheDir(), cacheDir)
	}
	b, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "keep this comment") {
		t.Error("the migrated configuration lost the person's comments")
	}
}

// The configuration moves last, so an interrupted migration is resumable:
// the absence of the new configuration is the flag that says "not done".
func TestMigrateResumesAfterAnInterruption(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, cacheDir := legacyTree(t, home)
	root := filepath.Join(home, ".cloudfs")
	// A previous attempt got as far as the journal and then died.
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(cacheDir, "journal"), filepath.Join(root, "journal")); err != nil {
		t.Fatal(err)
	}
	m, err := MigrateLegacyLayout(filepath.Join(root, "config.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Ran {
		t.Fatal("a half-finished migration was not resumed")
	}
	for _, name := range []string{"config.yaml", "meta.db", filepath.Join("journal", "journal.db")} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("%s missing after resume: %v", name, err)
		}
	}
}

// Nothing to migrate is not an error, and an installation that already has a
// configuration in the new root is never touched again.
func TestMigrateIsANoOpWhenThereIsNothingToDo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	newPath := filepath.Join(home, ".cloudfs", "config.yaml")

	m, err := MigrateLegacyLayout(newPath, nil)
	if err != nil || m.Ran {
		t.Fatalf("fresh install: ran=%v err=%v", m.Ran, err)
	}

	legacyPath, _ := legacyTree(t, home)
	if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("remotes: {}\nmounts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err = MigrateLegacyLayout(newPath, nil)
	if err != nil || m.Ran {
		t.Fatalf("already migrated: ran=%v err=%v", m.Ran, err)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Error("the legacy configuration was removed although nothing was migrated")
	}
}

// A configuration somewhere else entirely — CLOUDFS_CONFIG, or --config — is
// not the default root, and migrating into it would move a person's state
// under a path they chose for one command.
func TestMigrateOnlyRunsForTheDefaultRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacyTree(t, home)
	elsewhere := filepath.Join(t.TempDir(), "config.yaml")
	m, err := MigrateLegacyLayout(elsewhere, nil)
	if err != nil || m.Ran {
		t.Fatalf("migrated into a non-default path: ran=%v err=%v", m.Ran, err)
	}
}

// A running daemon refuses the migration. Renaming a journal out from under
// one leaves it writing into an unlinked directory, which is how unuploaded
// bytes disappear without anything reporting an error.
func TestMigrateRefusesWhileADaemonIsRunning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, cacheDir := legacyTree(t, home)
	newPath := filepath.Join(home, ".cloudfs", "config.yaml")

	var asked string
	m, err := MigrateLegacyLayout(newPath, func(socket string) bool { asked = socket; return true })
	if err == nil {
		t.Fatal("migrated while a daemon was answering")
	}
	if m.Ran {
		t.Error("reported a migration it refused to do")
	}
	if asked == "" {
		t.Error("never checked for a running daemon")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "meta.db")); err != nil {
		t.Errorf("state moved despite the refusal: %v", err)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Errorf("a configuration was written despite the refusal: %v", err)
	}
}

// A legacy configuration that never named a cache directory relied on the old
// default. The migrated file has to name it, or every cached block is
// orphaned in a directory nothing reads any more.
func TestMigrateNamesTheOldDefaultCacheExplicitly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacyPath, cacheDir := legacyTree(t, home)
	if err := os.WriteFile(legacyPath, []byte("remotes: {}\nmounts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(home, ".cloudfs", "config.yaml")
	if _, err := MigrateLegacyLayout(newPath, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BlockCacheDir() != cacheDir {
		t.Errorf("cache.dir = %q, want %q", cfg.BlockCacheDir(), cacheDir)
	}
}

// A control socket that pointed inside the emptied cache directory is moved
// into the new root; one the person put somewhere else is left alone.
func TestMigrateRepointsOnlyASocketInsideTheOldCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".cloudfs")
	newPath := filepath.Join(root, "config.yaml")

	legacyPath, cacheDir := legacyTree(t, home)
	body := "cache:\n  dir: " + cacheDir + "\ncontrol:\n  socket: " + filepath.Join(cacheDir, "control.sock") + "\nremotes: {}\nmounts: []\n"
	if err := os.WriteFile(legacyPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateLegacyLayout(newPath, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ExpandHome(cfg.Control.Socket), filepath.Join(root, "control.sock"); got != want {
		t.Errorf("control.socket = %q, want %q", got, want)
	}

	// Second installation: the socket is the person's own choice.
	home2 := t.TempDir()
	t.Setenv("HOME", home2)
	legacyPath2, cacheDir2 := legacyTree(t, home2)
	chosen := filepath.Join(home2, "run", "cloudfs.sock")
	body = "cache:\n  dir: " + cacheDir2 + "\ncontrol:\n  socket: " + chosen + "\nremotes: {}\nmounts: []\n"
	if err := os.WriteFile(legacyPath2, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateLegacyLayout(filepath.Join(home2, ".cloudfs", "config.yaml"), nil); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(filepath.Join(home2, ".cloudfs", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if ExpandHome(cfg2.Control.Socket) != chosen {
		t.Errorf("control.socket = %q, want the chosen %q", cfg2.Control.Socket, chosen)
	}
}
