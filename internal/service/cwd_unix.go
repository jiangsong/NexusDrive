//go:build linux || darwin

package service

import (
	"errors"
	"os/exec"
	"path/filepath"
)

// MountCWDHolders reports processes whose cwd is the mount or one of its
// descendants. lsof's field format is stable across Linux and macOS and does
// not require recursively walking the remote filesystem.
func MountCWDHolders(path string) ([]CWDHolder, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	}
	out, err := exec.Command("lsof", "-nP", "-a", "-d", "cwd", "-Fpcn").Output()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil, err
	}
	return parseLsofCWD(out, root), nil
}
