package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The guard compares the shell's $PWD against the registry. A shell started
// with no inherited PWD takes it from getcwd(2), which returns the path with
// every symlink resolved — so a mount recorded under its unresolved name never
// matches, the guard exits 0, and every hook is silently disabled. Nothing
// reports it: exit 0 is also what "you are not in a mount" looks like.
//
// This is not a corner case on macOS, where /var and /tmp are symlinks into
// /private, nor anywhere a home directory is reached through one.

// TestMountsRegistryRecordsTheResolvedPathToo pins the fix at the layer that
// can see the filesystem. The guard stays pure shell, so it cannot resolve a
// symlink itself without spawning the process it exists to avoid.
func TestMountsRegistryRecordsTheResolvedPathToo(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "cloud"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	viaLink := filepath.Join(link, "cloud")
	viaReal := filepath.Join(real, "cloud")

	path := filepath.Join(base, "mounts")
	if err := WriteMounts(path, []string{viaLink}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMounts(path)
	if err != nil {
		t.Fatal(err)
	}
	has := func(want string) bool {
		for _, g := range got {
			if g == want {
				return true
			}
		}
		return false
	}
	if !has(viaLink) {
		t.Errorf("registry dropped the configured path %q: %v", viaLink, got)
	}
	// EvalSymlinks resolves the whole path, so the expected entry is what the
	// shell would see; comparing against viaReal directly would itself be
	// wrong if the temp directory is reached through a link.
	wantResolved, err := filepath.EvalSymlinks(viaReal)
	if err != nil {
		t.Fatal(err)
	}
	if !has(wantResolved) {
		t.Errorf("registry has no resolved form of %q; the guard compares getcwd(2) and would never match: %v", viaLink, got)
	}
}

// TestGuardMatchesAMountReachedThroughASymlink is the end-to-end version: it
// runs the real guard under sh with PWD unset, which is how a hook actually
// starts, and requires it to reach the command.
func TestGuardMatchesAMountReachedThroughASymlink(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	real := filepath.Join(home, "real")
	deep := filepath.Join(real, "cloud", "project", "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// The mount is configured, and so recorded, through the symlink.
	if err := WriteMounts(MountsPath(home), []string{filepath.Join(link, "cloud")}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "cloudfs"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", Guard()+`echo REACHED`)
	cmd.Dir = deep
	// No PWD in the environment: a fresh shell derives it from getcwd(2), the
	// fully resolved path. That is the condition the bug needs, and it is the
	// condition a real hook runs under.
	cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh: %v: %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "REACHED" {
		t.Fatalf("the guard did not recognise a mount reached through a symlink, and said nothing about it: %q", out)
	}
}
