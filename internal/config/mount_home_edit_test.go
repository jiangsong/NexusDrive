package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTildeMountConfig writes a configuration whose mount path is the literal
// "~/CloudFS" the setup wizard puts there, and returns its path together with
// the expanded form every caller sees after Load.
func writeTildeMountConfig(t *testing.T) (configPath, expanded string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this host")
	}
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.yaml")
	body := "cache:\n  dir: " + filepath.Join(dir, "cache") +
		"\nremotes:\n  nas: {type: webdav, url: 'https://nas.local/dav'}\n" +
		"  gd: {type: webdav, url: 'https://gd.local/dav'}\n" +
		"mounts:\n  - path: ~/CloudFS\n    layout:\n      /: {remote: nas}\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, filepath.Join(home, "CloudFS")
}

// Parse expands a mount path, so every caller that read the configuration
// holds "/home/you/CloudFS" while the file still says "~/CloudFS". The mount
// editors match against the raw YAML node, so unless both sides are
// canonicalized the editors cannot find the mount the rest of the process is
// talking about. The wizard's default folder is exactly ~/CloudFS, which makes
// this the ordinary path rather than a corner case.
func TestTheMountEditorsFindAMountWrittenWithTheHomeShorthand(t *testing.T) {
	configPath, expanded := writeTildeMountConfig(t)

	if err := AddMount(configPath, expanded, "/gd", Layout{Remote: "gd"}); err != nil {
		t.Fatalf("AddMount: %v", err)
	}
	c, err := Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Mounts) != 1 {
		paths := make([]string, 0, len(c.Mounts))
		for _, m := range c.Mounts {
			paths = append(paths, m.Path)
		}
		t.Fatalf("mounts = %v; the drive was added to a second mount entry, and only the first one is ever mounted", paths)
	}
	if _, ok := c.Mounts[0].Layout["/gd"]; !ok {
		t.Fatalf("layout = %v, want the new prefix on the existing mount", c.Mounts[0].Layout)
	}

	if err := SetLayout(configPath, expanded, "/gd", Layout{Remote: "nas"}); err != nil {
		t.Fatalf("SetLayout: %v", err)
	}
	if err := RemoveMount(configPath, expanded, "/gd"); err != nil {
		t.Fatalf("RemoveMount: %v", err)
	}
	if c, err = Load(configPath); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Mounts[0].Layout["/gd"]; ok {
		t.Fatal("the layout is still there; the unbind in the browser reported success against a mount it never found")
	}
}

// The node keeps whatever the person wrote. Canonicalizing for the comparison
// must not rewrite ~/CloudFS into an absolute path in their file, or the
// configuration stops being portable between machines and accounts.
func TestEditingAMountLeavesTheHomeShorthandInTheFile(t *testing.T) {
	configPath, expanded := writeTildeMountConfig(t)
	if err := AddMount(configPath, expanded, "/gd", Layout{Remote: "gd"}); err != nil {
		t.Fatalf("AddMount: %v", err)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "~/CloudFS") {
		t.Fatalf("the file no longer says ~/CloudFS:\n%s", raw)
	}
}

// Two mount entries for one directory is never something a person wrote on
// purpose, and it is silently destructive: cloudfs mount takes Mounts[0], so
// everything bound under the second entry is invisible with no error anywhere.
func TestTwoMountEntriesForTheSameDirectoryAreRefused(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this host")
	}
	dir := t.TempDir()
	body := "cache:\n  dir: " + filepath.Join(dir, "cache") +
		"\nremotes:\n  nas: {type: webdav, url: 'https://nas.local/dav'}\n" +
		"mounts:\n  - path: ~/CloudFS\n    layout:\n      /: {remote: nas}\n" +
		"  - path: " + filepath.Join(home, "CloudFS") + "\n    layout:\n      /gd: {remote: nas}\n"
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(path)
	if err == nil {
		t.Fatal("a configuration with the same directory mounted twice was accepted")
	}
	if !strings.Contains(err.Error(), "mounted twice") {
		t.Fatalf("error = %v, want it to name the duplicate mount", err)
	}
}
