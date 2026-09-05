//go:build linux || darwin

package main

import (
	"fmt"
	"os/exec"
	"runtime"
)

// unmountPath detaches a mount this process does not own, using whichever
// helper the platform provides.
func unmountPath(path string) error {
	var candidates [][]string
	if runtime.GOOS == "darwin" {
		candidates = [][]string{{"umount", path}, {"diskutil", "unmount", path}}
	} else {
		candidates = [][]string{{"fusermount3", "-u", path}, {"fusermount", "-u", path}, {"umount", path}}
	}
	var lastErr error
	var lastOut []byte
	for _, c := range candidates {
		bin, err := exec.LookPath(c[0])
		if err != nil {
			continue
		}
		out, err := exec.Command(bin, c[1:]...).CombinedOutput()
		if err == nil {
			fmt.Printf("unmounted %s\n", path)
			return nil
		}
		lastErr, lastOut = err, out
	}
	if lastErr == nil {
		return fmt.Errorf("no unmount helper found; install fuse3 (Linux) or use 'umount %s'", path)
	}
	return fmt.Errorf("unmount %s: %v: %s", path, lastErr, lastOut)
}
