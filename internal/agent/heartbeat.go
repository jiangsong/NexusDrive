package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A stdio MCP server started while another process owns the cache is not
// the owner: it has its own VFS, no uploader, and a view the mount does not
// share (docs/mcp.md, "与挂载并存"). Nothing else in the system can see such
// a process — the journal lock says only that someone holds it — so the
// process announces itself with a heartbeat file, <cache.dir>/agent/
// stdio-<pid>.hb, touched every HeartbeatInterval and removed on exit.
// doctor and /mcp/connect read the directory to warn that the topology is
// the one the documentation advises against.
const (
	// HeartbeatInterval is how often a non-owner stdio process touches its
	// heartbeat.
	HeartbeatInterval = 30 * time.Second
	// HeartbeatStale is the age past which a heartbeat is the leftover of a
	// process that did not get to remove it (kill -9, power loss) and is
	// cleaned up by whoever reads it.
	HeartbeatStale = 2 * time.Minute
)

const heartbeatPrefix, heartbeatSuffix = "stdio-", ".hb"

func heartbeatPath(dir string, pid int) string {
	return filepath.Join(dir, heartbeatPrefix+strconv.Itoa(pid)+heartbeatSuffix)
}

// WriteHeartbeat creates or refreshes the heartbeat of the stdio process pid
// under dir. Rewriting the file is what moves its mtime; the content is the
// pid, for a person looking at the directory.
func WriteHeartbeat(dir string, pid int) error {
	if dir == "" {
		return errors.New("agent: a store directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("agent: heartbeat: %w", err)
	}
	if err := os.WriteFile(heartbeatPath(dir, pid), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return fmt.Errorf("agent: heartbeat: %w", err)
	}
	return nil
}

// RemoveHeartbeat deletes the heartbeat of pid. A heartbeat that is already
// gone is not an error: exit paths call this more than once.
func RemoveHeartbeat(dir string, pid int) error {
	if err := os.Remove(heartbeatPath(dir, pid)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("agent: heartbeat: %w", err)
	}
	return nil
}

// LiveStdioProcesses lists the pids whose heartbeat under dir is younger
// than HeartbeatStale at now, ascending. A stale heartbeat is deleted on
// the way, so a crashed process stops being reported after one look; a
// directory that does not exist yet has no processes in it.
func LiveStdioProcesses(dir string, now time.Time) []int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var live []int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, heartbeatPrefix) || !strings.HasSuffix(name, heartbeatSuffix) {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, heartbeatPrefix), heartbeatSuffix))
		if err != nil || pid <= 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > HeartbeatStale {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		live = append(live, pid)
	}
	sort.Ints(live)
	return live
}
