package trigger

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
)

// TestExecArgvIsNeverAShell: the placeholder is replaced as a whole argv
// element and the child receives the path verbatim, shell metacharacters
// and all, because there is no shell between the runner and the program.
// (Config rejects "{path}; rm -rf /" as a template, so the metacharacters
// come from the path's value — the case an attacker controls.)
func TestExecArgvIsNeverAShell(t *testing.T) {
	a := config.ExecAction{Command: []string{self(), "-echo-argv", "{path}", "{kind}", "literal {path}"}, Timeout: 10 * time.Second}
	stdout, stderr, err := runExec(context.Background(), a, map[string]string{"{path}": "/work/a.txt; rm -rf /", "{kind}": "write"})
	if err != nil {
		t.Fatalf("err=%v stderr=%s", err, stderr)
	}
	lines := strings.Split(strings.TrimRight(string(stdout), "\n"), "\n")
	want := []string{"/work/a.txt; rm -rf /", "write", "literal {path}"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("argv = %q, want %q", lines, want)
	}
	// The placeholder that was only part of a longer element stayed as
	// written: substitution never splices a value into a string.
	if strings.Contains(string(stdout), "literal /work") {
		t.Fatal("a placeholder inside a longer argument was substituted")
	}
	for _, l := range lines {
		if l == "sh" || l == "-c" || strings.HasSuffix(l, "/sh") {
			t.Fatalf("a shell appeared in argv: %q", lines)
		}
	}
}

// TestExecEnvironmentIsMinimal: the child sees PATH, HOME and LANG from
// the daemon, the three CLOUDFS_* variables, and nothing else — no tokens,
// no proxy settings, no per-user surprises.
func TestExecEnvironmentIsMinimal(t *testing.T) {
	t.Setenv("CLOUDFS_TEST_SECRET", "leaked")
	t.Setenv("LANG", "C.UTF-8")
	a := config.ExecAction{Command: []string{self(), "-echo-env"}, Cwd: t.TempDir(), Timeout: 10 * time.Second}
	stdout, stderr, err := runExec(context.Background(), a, map[string]string{"{path}": "/work/a.txt", "{kind}": "write", "{uri}": "cloudfs://r/work/a.txt"})
	if err != nil {
		t.Fatalf("err=%v stderr=%s", err, stderr)
	}
	// os/exec adds PWD next to an absolute Dir on POSIX; that is the only
	// variable outside our list a child may see.
	allowed := map[string]bool{"PATH": true, "HOME": true, "LANG": true, "PWD": true, "CLOUDFS_PATH": true, "CLOUDFS_KIND": true, "CLOUDFS_URI": true}
	got := map[string]string{}
	for _, kv := range strings.Split(strings.TrimRight(string(stdout), "\n"), "\n") {
		k, v, _ := strings.Cut(kv, "=")
		if !allowed[k] {
			t.Errorf("unexpected variable %q reached the child", kv)
		}
		got[k] = v
	}
	if got["CLOUDFS_PATH"] != "/work/a.txt" || got["CLOUDFS_KIND"] != "write" || got["CLOUDFS_URI"] != "cloudfs://r/work/a.txt" {
		t.Fatalf("CLOUDFS_* = %v", got)
	}
	if got["LANG"] != "C.UTF-8" || got["PATH"] != os.Getenv("PATH") {
		t.Fatalf("PATH/LANG not copied: %v", got)
	}
	if _, leaked := got["CLOUDFS_TEST_SECRET"]; leaked {
		t.Fatal("an unrelated variable leaked into the child")
	}
}

// TestExecTimeoutKillsTheProcessGroup: `sh -c 'sleep 60 & wait'` leaves a
// grandchild that outlives a plain kill of sh. The runner kills the whole
// process group, so after the timeout no process with that group remains.
func TestExecTimeoutKillsTheProcessGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc to find the grandchild")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	a := config.ExecAction{Command: []string{"/bin/sh", "-c", "echo $$; sleep 60 & wait"}, Timeout: 300 * time.Millisecond}
	start := time.Now()
	stdout, _, err := runExec(context.Background(), a, nil)
	if err == nil || !errors.Is(err, errTimeout) {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the runner waited %v for a killed child", time.Since(start))
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(string(stdout)))
	if err != nil || pgid <= 0 {
		t.Fatalf("sh did not print its pid: %q", stdout)
	}
	// A killed grandchild may linger as a zombie until init reaps it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		alive := processesInGroup(t, pgid)
		if len(alive) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes still in group %d: %v", pgid, alive)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// processesInGroup lists live (non-zombie) pids whose process group is pgid.
func processesInGroup(t *testing.T, pgid int) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// "pid (comm) state ppid pgrp ..." — comm may hold spaces, so cut
		// after the closing parenthesis.
		rest := string(stat[bytes.LastIndexByte(stat, ')')+2:])
		fields := strings.Fields(rest)
		if len(fields) < 3 {
			continue
		}
		if fields[0] == "Z" {
			continue
		}
		if g, _ := strconv.Atoi(fields[2]); g == pgid {
			out = append(out, pid)
		}
	}
	return out
}

// TestExecOutputIsCapped: each stream is cut at outputCap and marked, so
// a chatty command cannot grow agent.db without bound.
func TestExecOutputIsCapped(t *testing.T) {
	a := config.ExecAction{Command: []string{self(), "-spew", "200"}, Timeout: 10 * time.Second}
	stdout, stderr, err := runExec(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string][]byte{"stdout": stdout, "stderr": stderr} {
		if len(out) > outputCap+len(outputTruncatedMarker) || !bytes.HasSuffix(out, []byte(outputTruncatedMarker)) {
			t.Fatalf("%s: %d bytes, marker present=%v", name, len(out), bytes.HasSuffix(out, []byte(outputTruncatedMarker)))
		}
	}
	// A short run is stored whole, without the marker.
	a.Command = []string{self(), "-spew", "1"}
	stdout, _, err = runExec(context.Background(), a, nil)
	if err != nil || len(stdout) != 1024 {
		t.Fatalf("short run: %d bytes err=%v", len(stdout), err)
	}
}

// TestExecReportsTheExitStatus: a non-zero exit is an error that carries
// the status, and the streams are still returned for the delivery row.
func TestExecReportsTheExitStatus(t *testing.T) {
	a := config.ExecAction{Command: []string{self(), "-exit", "3"}, Timeout: 10 * time.Second}
	_, stderr, err := runExec(context.Background(), a, nil)
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(string(stderr), "failing on purpose") {
		t.Fatalf("stderr = %q", stderr)
	}
}
