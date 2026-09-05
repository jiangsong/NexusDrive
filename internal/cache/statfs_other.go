//go:build !linux && !darwin

package cache

import "errors"

// FreeSpace is unsupported on this platform; MinFree checks are skipped.
func FreeSpace(string) (int64, error) { return 0, errors.New("cache: statfs unsupported") }
