//go:build !linux && !darwin

package main

import "fmt"

// unmountPath is unsupported outside Linux and macOS in this build.
func unmountPath(path string) error {
	return fmt.Errorf("unmounting is not supported on this platform; detach %s with the system tools", path)
}
