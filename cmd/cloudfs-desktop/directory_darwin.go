//go:build desktop && darwin

package main

import (
	"os/exec"
	"strings"
)

func chooseDirectory() (string, error) {
	out, err := exec.Command("osascript", "-e", `POSIX path of (choose folder with prompt "Choose an export destination")`).Output()
	if err != nil {
		if _, cancelled := err.(*exec.ExitError); cancelled {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
