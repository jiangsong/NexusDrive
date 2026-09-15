//go:build desktop && linux

package main

import (
	"os/exec"
	"strings"
)

func chooseDirectory() (string, error) {
	out, err := exec.Command("zenity", "--file-selection", "--directory", "--title=Choose an export destination").Output()
	if err != nil {
		if _, cancelled := err.(*exec.ExitError); cancelled {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
