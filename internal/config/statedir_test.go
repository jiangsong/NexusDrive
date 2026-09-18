package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The state directory follows the configuration file, never the block cache.
//
// meta.db, the journal and the agent database are not reproducible: the
// journal holds bytes that have been written and not yet uploaded. cache.dir
// is a setting a person is invited to point at an external disk. If state
// followed cache.dir, unplugging that disk would take unuploaded data with it.
func TestStateDirFollowsTheConfigFileNotTheCacheDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".cloudfs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "external-disk")
	path := filepath.Join(root, "config.yaml")
	body := "cache:\n  dir: " + elsewhere + "\nremotes: {}\nmounts: []\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.StateDir(); got != root {
		t.Errorf("StateDir() = %q, want %q", got, root)
	}
	if got := c.BlockCacheDir(); got != elsewhere {
		t.Errorf("BlockCacheDir() = %q, want %q", got, elsewhere)
	}
	if strings.HasPrefix(c.StateDir(), elsewhere) {
		t.Error("the state directory moved with cache.dir; unplugging that disk would take unuploaded data with it")
	}
}

// A configuration that names no cache directory caches inside the state
// directory, so a default install keeps everything under one root.
func TestBlockCacheDefaultsInsideTheStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".cloudfs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("remotes: {}\nmounts: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.BlockCacheDir(), filepath.Join(root, "cache"); got != want {
		t.Errorf("BlockCacheDir() = %q, want %q", got, want)
	}
}

// Both defaults live under the single root the product documents, so a person
// who never opens the file finds one directory rather than three.
func TestDefaultsLiveUnderOneRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	d := Default()
	// Cache.Dir is empty on purpose: BlockCacheDir resolves it beside the
	// configuration, so a config carried elsewhere takes its cache along.
	if d.Cache.Dir != "" {
		t.Errorf("Default().Cache.Dir = %q, want empty so it can follow the config file", d.Cache.Dir)
	}
	for _, tc := range []struct{ name, got, want string }{
		{"BlockCacheDir", d.BlockCacheDir(), filepath.Join(home, ".cloudfs", "cache")},
		{"StateDir", d.StateDir(), filepath.Join(home, ".cloudfs")},
		{"control.socket", ExpandHome(d.Control.Socket), filepath.Join(home, ".cloudfs", "control.sock")},
		{"DefaultConfigPath", DefaultConfigPath(), filepath.Join(home, ".cloudfs", "config.yaml")},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// CLOUDFS_CONFIG wins over the default, and a leading ~ in it is expanded:
// the variable reaches the process unexpanded from a launchd plist.
func TestDefaultConfigPathHonoursTheEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLOUDFS_CONFIG", "~/elsewhere/config.yaml")
	if got, want := DefaultConfigPath(), filepath.Join(home, "elsewhere", "config.yaml"); got != want {
		t.Errorf("DefaultConfigPath() = %q, want %q", got, want)
	}
}
