//go:build linux || darwin

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func currentUID() int { return os.Getuid() }

func mountpointMounted(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	parent, err := os.Stat(filepath.Dir(filepath.Clean(path)))
	if err != nil {
		return false, err
	}
	current, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("service: unsupported mount stat")
	}
	up, ok := parent.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("service: unsupported parent mount stat")
	}
	return current.Dev != up.Dev, nil
}
