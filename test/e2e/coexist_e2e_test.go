package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	"cloudfs/internal/fusefs"
	"cloudfs/internal/mcpsrv"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// coexistTemplate is configTemplate with the fake backend shared between
// every daemon the process opens on it: the two daemons of this test stand
// for `cloudfs mount` and a `cloudfs mcp` started beside it, which in
// production talk to the same account.
const coexistTemplate = `
cache:
  dir: %s
  max_size: 1GiB
  min_free: 1MiB
  block_size: 64KiB
proxy:
  rules:
    - FINAL,direct
remotes:
  demo: { type: fake, shared: %s }
mounts:
  - path: %s
    layout:
      /: { remote: demo, root: root, mode: writeback }
mcp:
  allow: []
`

// TestStdioBesideMountSharesWrites is the T-43 reproduction: `cloudfs
// mount` owns the cache and a `cloudfs mcp` (stdio) started beside it opens
// the same cache directory as a non-owner — its own VFS over the shared
// meta, cache and journal, with no uploader of its own. The second
// daemon.Open here is that process; the flock is per open file description,
// so a second open inside one process is refused the lock the same way a
// second process is, and the test checks that assumption rather than
// assuming it.
//
// Three things are asserted, and none may be weakened: (a) a file the stdio
// side writes is readable at the mount, with the same bytes, within five
// seconds; (b) once the owner's queue drains, the backend holds the file and
// exactly one upload was made for it; (c) after the stdio side exits and
// the owner restarts, the file is neither lost nor duplicated and the stdio
// session is finished rather than left active.
func TestStdioBesideMountSharesWrites(t *testing.T) {
	if ok, why := fusefs.Supported(); !ok {
		t.Skipf("FUSE unavailable: %s", why)
	}
	base := t.TempDir()
	cacheDir := filepath.Join(base, "cache")
	mountDir := filepath.Join(base, "mnt")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shared := fmt.Sprintf("coexist-%d", time.Now().UnixNano())
	cfgPath := filepath.Join(base, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(coexistTemplate, cacheDir, shared, mountDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(cacheDir, "agent")

	// The owner: what `cloudfs mount` is.
	ctx1, cancel1 := context.WithCancel(context.Background())
	d1, err := daemon.Open(ctx1, daemon.Options{Config: cfg, Version: "e2e"})
	if err != nil {
		cancel1()
		t.Fatal(err)
	}
	closeOwner := func() {
		cancel1()
		d1.Close()
	}
	fake := provider.Unwrap(d1.Providers["demo"]).(*fakeprovider.Fake)
	if fake != fakeprovider.Shared(shared, "demo") {
		closeOwner()
		t.Fatal("the shared fake was not the one the daemon resolved")
	}
	if !d1.Journal.Owner() {
		closeOwner()
		t.Fatal("the first daemon did not become the journal owner")
	}
	m, err := fusefs.MountFS(fusefs.MountOptions{
		Options: fusefs.Options{FS: d1.FS, AttrTimeout: time.Second, EntryTimeout: time.Second},
		Path:    mountDir,
	})
	if err != nil {
		closeOwner()
		t.Fatalf("mount: %v", err)
	}
	mounted := true
	unmount := func() {
		if mounted {
			mounted = false
			m.Unmount()
		}
	}
	defer func() {
		unmount()
		closeOwner()
	}()
	if err := os.Mkdir(filepath.Join(mountDir, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The stdio server beside it: a second open of the same cache directory
	// in this process, then the same mcpsrv.Options cmdMCP builds for a
	// non-owner, including the heartbeat cmdMCP writes.
	ctx2, cancel2 := context.WithCancel(context.Background())
	d2, err := daemon.Open(ctx2, daemon.Options{Config: cfg, Version: "e2e"})
	if err != nil {
		cancel2()
		t.Fatal(err)
	}
	stdioOpen := true
	closeStdio := func() {
		if stdioOpen {
			stdioOpen = false
			cancel2()
			d2.Close()
		}
	}
	defer closeStdio()
	if d2.Journal == nil || d2.Journal.Owner() {
		t.Fatal("the second open of the cache directory became the journal owner: the flock did not hold across file descriptions, so this test does not reproduce a stdio server beside a mount")
	}
	if d2.Agent.Owner() {
		t.Fatal("the second open of the cache directory became the agent.db owner")
	}
	if err := agent.WriteHeartbeat(agentDir, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	srv2, err := mcpsrv.New(mcpsrv.Options{FS: d2.FS, NonOwner: true, Sessions: d2.Sessions, Version: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = srv2.Run(ctx2, serverT) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-e2e", Version: "0"}, nil)
	cs, err := client.Connect(ctx2, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(ctx2, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res
	}

	// With the stdio server up, the owner's doctor must say so.
	if c := doctorOf(d1)["agent_stdio"]; c.Level != control.LevelWarn || !strings.Contains(c.Fix, "cloudfs mcp install --transport http") {
		t.Fatalf("doctor beside a live stdio server: %+v", c)
	}

	// (a) The stdio side writes; the shell at the mount reads it back. A
	// tool error is recorded rather than fatal so the poll below can say
	// what the mount saw of the write anyway — that is the evidence T-43
	// asks for.
	const content = "written over stdio beside the mount\n"
	if res := call("write_file", map[string]any{"path": "/demo/side.txt", "content": content}); res.IsError {
		t.Errorf("(a) stdio write_file returned an error: %s", toolText(res))
	}
	shellPath := filepath.Join(mountDir, "demo", "side.txt")
	var (
		deadline                  = time.Now().Add(5 * time.Second)
		started                   = time.Now()
		firstSeen   time.Duration = -1
		lastReadErr error
		readAt      time.Duration = -1
	)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(shellPath); err == nil && firstSeen < 0 {
			firstSeen = time.Since(started)
		}
		got, err := os.ReadFile(shellPath)
		if err == nil && string(got) == content {
			readAt = time.Since(started)
			break
		}
		if err != nil {
			lastReadErr = err
		} else {
			lastReadErr = fmt.Errorf("content %q", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if readAt < 0 {
		rows, _ := d1.Journal.All(context.Background())
		var queue []string
		for _, u := range rows {
			queue = append(queue, fmt.Sprintf("%s state=%s needs_publish=%v", u.Name, u.State, u.NeedsPublish))
		}
		t.Fatalf("(a) the shell never read the stdio side's file within 5 s: entry first visible after %v, last read error: %v; journal rows: %v", firstSeen, lastReadErr, queue)
	}
	t.Logf("(a) entry visible at the mount after %v, content readable after %v", firstSeen, readAt)

	// (b) The owner's uploader takes the row the stdio side queued: the
	// backend holds the file, uploaded exactly once.
	settleOwner := func(d *daemon.Daemon) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			st, err := d.Journal.Stats(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if st.Pending == 0 && st.Uploading == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		st, _ := d.Journal.Stats(context.Background())
		t.Fatalf("upload queue did not drain: %+v", st)
	}
	settleOwner(d1)
	if got, ok := fake.Content("demo/side.txt"); !ok || string(got) != content {
		t.Fatalf("(b) backend holds %q (%v), want the stdio side's content", got, ok)
	}
	if n := fake.Calls("BeginUpload"); n != 1 {
		t.Fatalf("(b) the file was uploaded %d times, want exactly once", n)
	}

	// The stdio process exits: its session is finished, its heartbeat gone,
	// and doctor stops warning.
	stdioSessions, _, err := d2.Sessions.List(ctx2, agent.ListQuery{State: "active"})
	if err != nil || len(stdioSessions) != 1 || stdioSessions[0].Transport != "stdio" {
		t.Fatalf("stdio session before exit: %+v %v", stdioSessions, err)
	}
	cs.Close()
	if n := srv2.FinishStdioSessions(context.Background()); n != 1 {
		t.Fatalf("exit finished %d sessions, want 1", n)
	}
	srv2.Close()
	if err := agent.RemoveHeartbeat(agentDir, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	closeStdio()
	if c := doctorOf(d1)["agent_stdio"]; c.Level != control.LevelOK {
		t.Fatalf("doctor after the stdio server left: %+v", c)
	}

	// (c) The owner restarts on the same cache: the file is still there,
	// once, with the same bytes, and the queue is clean.
	unmount()
	closeOwner()
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	d3, err := daemon.Open(ctx3, daemon.Options{Config: cfg, Version: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	defer d3.Close()
	if !d3.Journal.Owner() {
		t.Fatal("the restarted daemon did not become the owner")
	}
	settleOwner(d3)
	got, err := d3.FS.ReadFileRange(ctx3, "/demo/side.txt", 0, 0)
	if err != nil || string(got) != content {
		t.Fatalf("(c) after restart the file reads %q, %v", got, err)
	}
	entries, err := d3.FS.ReadDirPath(ctx3, "/demo")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	if len(names) != 1 || names[0] != "side.txt" {
		t.Fatalf("(c) after restart /demo lists %v, want [side.txt]", names)
	}
	if st, err := d3.Journal.Stats(ctx3); err != nil || st.Dead > 0 || st.Pending > 0 {
		t.Fatalf("(c) journal after restart: %+v %v", st, err)
	}
	if got, ok := fake.Content("demo/side.txt"); !ok || string(got) != content {
		t.Fatalf("(c) backend after restart holds %q (%v)", got, ok)
	}
	if n := fake.Calls("BeginUpload"); n != 1 {
		t.Fatalf("(c) restart uploaded the file again: %d uploads", n)
	}
	sess, err := d3.Sessions.Get(ctx3, stdioSessions[0].ID)
	if err != nil || sess.State != "finished" {
		t.Fatalf("(c) the stdio session after its process exited: %+v %v", sess, err)
	}
	if c := doctorOf(d3)["agent_db"]; c.Level != control.LevelOK {
		t.Fatalf("(c) agent.db after restart: %+v", c)
	}
}

// doctorOf runs the daemon's doctor and indexes the checks by name.
func doctorOf(d *daemon.Daemon) map[string]control.Check {
	byName := map[string]control.Check{}
	for _, c := range d.Doctor(nil, fusefs.Supported).Run(context.Background()) {
		byName[c.Name] = c
	}
	return byName
}
