package service

import (
	"path/filepath"
	"strconv"
	"strings"
)

// CWDHolder is a process whose current working directory belongs to a mount.
// Such a process cannot follow an unmount/remount at the same pathname: its
// cwd references the old mount object in the kernel.
type CWDHolder struct {
	PID     int
	Command string
	Path    string
}

func parseLsofCWD(out []byte, root string) []CWDHolder {
	root = filepath.Clean(root)
	var pid int
	var command string
	var holders []CWDHolder
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
		case 'c':
			command = line[1:]
		case 'n':
			path := line[1:]
			if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
				holders = append(holders, CWDHolder{PID: pid, Command: command, Path: path})
			}
		}
	}
	return holders
}
