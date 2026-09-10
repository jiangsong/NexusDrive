package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Nothing serves the control plane until a configuration exists, and the
// control plane is where drives are added — so a first-time user was told to
// write YAML by hand, from a design document, before anything could help them.
// A starter file breaks that circle. It has no mounts and no remotes on
// purpose: a placeholder mount would become the wrong mount permanently,
// because `cloudfs mount` takes the first one in the file.
func TestWriteStarterProducesAConfigurationTheDaemonCanRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := WriteStarter(path); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("the starter file does not load: %v", err)
	}
	if c.Cache.Dir == "" {
		t.Error("the starter file names no cache directory")
	}
	if len(c.Mounts) != 0 {
		t.Errorf("the starter file declares mounts: %+v", c.Mounts)
	}
	if len(c.Remotes) != 0 {
		t.Errorf("the starter file declares remotes: %+v", c.Remotes)
	}
	if c.Control.Socket == "" {
		t.Error("the starter file names no control socket, so nothing can reach the setup screen")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileMode(t, path); perm != 0o600 {
		t.Errorf("the starter file is mode %o; a configuration can grow to hold secret references", perm)
	}
	if !strings.Contains(string(b), "remotes") {
		t.Errorf("the starter file has no remotes key to add accounts under:\n%s", b)
	}
}

// Running setup again must never destroy what is already configured.
func TestWriteStarterRefusesToOverwriteAnExistingConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := "cache:\n  dir: " + filepath.Join(dir, "cache") + "\nremotes: {}\nmounts: []\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteStarter(path); err == nil {
		t.Fatal("an existing configuration was overwritten")
	}
	b, _ := os.ReadFile(path)
	if string(b) != original {
		t.Fatalf("the existing configuration was modified:\n%s", b)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// Every other path into this package expands a leading ~ — Load does, and so
// does editConfig — so a starter written from a path that still has one lands
// somewhere none of them will look. CLOUDFS_CONFIG set in a launchd plist or a
// systemd unit is not expanded by a shell, which is how the tilde survives to
// get here: the file is created in a directory literally named ~ under the
// working directory, and the read-back, which does expand, fails with a file
// that is not there.
func TestAStarterPathExpandsTheHomeShorthandLikeEveryOtherPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := WriteStarter("~/cloudfs-starter/config.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "cloudfs-starter", "config.yaml")); err != nil {
		t.Fatalf("the starter is not in the home directory: %v", err)
	}
	if entries, err := os.ReadDir("."); err == nil {
		for _, e := range entries {
			if e.Name() == "~" {
				t.Fatal(`a directory literally named "~" was created in the working directory`)
			}
		}
	}
}
