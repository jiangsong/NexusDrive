//go:build desktop && windows

package main

import (
	"os/exec"
	"strings"
)

func chooseDirectory() (string, error) {
	script := `Add-Type -AssemblyName System.Windows.Forms; $d = New-Object System.Windows.Forms.FolderBrowserDialog; $d.Description = 'Choose an export destination'; if ($d.ShowDialog() -eq 'OK') { $d.SelectedPath }`
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
