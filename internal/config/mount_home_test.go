package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cache.dir and control.socket have always accepted a leading ~; mounts[].path
// did not, and nothing said so. A configuration written by hand — or by the
// setup flow, whose default is ~/CloudFS — then mounted the filesystem in a
// directory literally named "~" under whatever the daemon's working directory
// happened to be, while ~/CloudFS in the shell stayed empty.
func TestAMountPathExpandsTheHomeShorthandLikeEveryOtherPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this host")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "cache:\n  dir: " + filepath.Join(dir, "cache") +
		"\nremotes:\n  nas: {type: webdav, url: 'https://nas.local/dav'}\nmounts:\n  - path: ~/CloudFS\n    layout:\n      /: {remote: nas}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := c.Mounts[0].Path
	if strings.HasPrefix(got, "~") {
		t.Fatalf("mount path = %q; it would create a directory named ~ instead of one in the home directory", got)
	}
	if want := filepath.Join(home, "CloudFS"); got != want {
		t.Fatalf("mount path = %q, want %q", got, want)
	}
}
