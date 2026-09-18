package e2e

import (
	"context"
	"encoding/json"
	"errors"
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
	"cloudfs/internal/journal"
	"cloudfs/internal/mcpsrv"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
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

// TestStdioBesideMountRefusesWritesCleanly is the T-43 contract: `cloudfs
// mount` owns the cache and a `cloudfs mcp` (stdio) started beside it opens
// the same cache directory as a non-owner — its own VFS over the shared
// meta, cache and journal, with no uploader of its own. The second
// daemon.Open here is that process; the flock is per open file description,
// so a second open inside one process is refused the lock the same way a
// second process is, and the test checks that assumption rather than
// assuming it.
//
// The first version of this test (C0, 2026-09-15) asked such a server to
// write and recorded what happened: write_file failed with "publication
// requires storage ownership" after the node was already in shared meta,
// the mount saw the entry at once but read EIO from it, the journal kept a
// row nobody published, and the file "came back" on the owner's next
// start. Every one of those symptoms is a negative assertion below. The
// contract now: a non-owner refuses every mutation with one error that
// names the HTTP transport, before it touches meta, the journal or the
// backend; reads keep working; an owner restart resurrects nothing. The
// stdio→HTTP bridge that makes such a server write through the owner is
// the proper fix and is scheduled (see the phase-two plan's conclusions).
func TestStdioBesideMountRefusesWritesCleanly(t *testing.T) {
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
	agentDir := filepath.Join(cfg.StateDir(), "agent")

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

	// The shell writes a file through the mount; the owner uploads it. It
	// is what the stdio side must still be able to read, and the target
	// of the edit, move, copy and delete it must not be able to make.
	const shellContent = "written by the shell at the mount\n"
	if err := os.Mkdir(filepath.Join(mountDir, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	shellPath := filepath.Join(mountDir, "demo", "shell.txt")
	if err := os.WriteFile(shellPath, []byte(shellContent), 0o644); err != nil {
		t.Fatal(err)
	}
	settleOwner(d1)
	if got, ok := fake.Content("demo/shell.txt"); !ok || string(got) != shellContent {
		t.Fatalf("backend after the shell write holds %q (%v)", got, ok)
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
	call := func(name string, args map[string]any, out any) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(ctx2, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out != nil && !res.IsError && res.StructuredContent != nil {
			b, _ := json.Marshal(res.StructuredContent)
			if err := json.Unmarshal(b, out); err != nil {
				t.Fatalf("%s: decode: %v", name, err)
			}
		}
		return res
	}

	// With the stdio server up, the owner's doctor must say so.
	if c := doctorOf(d1)["agent_stdio"]; c.Level != control.LevelWarn || !strings.Contains(c.Fix, "cloudfs mcp install --transport http") {
		t.Fatalf("doctor beside a live stdio server: %+v", c)
	}

	// The baseline every mutation is measured against.
	rowsBefore, err := d1.Journal.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	uploadsBefore := fake.Calls("BeginUpload")
	callsBefore := fake.TotalCalls()

	// Every mutation over stdio is refused with the one owner error.
	const content = "written over stdio beside the mount\n"
	mutations := []struct {
		tool string
		args map[string]any
	}{
		{"write_file", map[string]any{"path": "/demo/side.txt", "content": content}},
		{"edit_file", map[string]any{"path": "/demo/shell.txt", "edits": []map[string]any{{"old_text": "shell", "new_text": "agent"}}}},
		{"create_directory", map[string]any{"path": "/demo/sub"}},
		{"move", map[string]any{"from": "/demo/shell.txt", "to": "/demo/moved.txt"}},
		{"copy", map[string]any{"from": "/demo/shell.txt", "to": "/demo/copy.txt"}},
		{"delete", map[string]any{"path": "/demo/shell.txt", "confirm": true}},
	}
	for _, mu := range mutations {
		res := call(mu.tool, mu.args, nil)
		body := toolText(res)
		if !res.IsError {
			t.Fatalf("stdio %s beside the mount succeeded: %s", mu.tool, body)
		}
		if !strings.Contains(body, "requires the storage owner") || !strings.Contains(body, "cloudfs mcp install --transport http") {
			t.Fatalf("stdio %s refused without naming the owner and the HTTP transport: %s", mu.tool, body)
		}
		// C0 saw "journal: publication requires storage ownership" — the
		// refusal of a write that had already reached meta.
		if strings.Contains(body, "publication requires storage ownership") {
			t.Fatalf("stdio %s reached the journal before being refused: %s", mu.tool, body)
		}
	}
	// The VFS behind the server refuses on its own too, so a tool that
	// forgot the fence could not get further.
	if _, err := d2.FS.WriteFile(ctx2, "/demo/vfs.txt", []byte("x"), false); !errors.Is(err, vfs.ErrNotOwner) {
		t.Fatalf("the non-owner VFS accepted a write: %v", err)
	}

	// Nothing reached the backend: C0 saw create_directory make the
	// directory on the remote and the write leave a row for the owner.
	if n := fake.TotalCalls(); n != callsBefore {
		t.Fatalf("refused stdio mutations made %d provider calls", n-callsBefore)
	}
	for _, p := range []string{"demo/side.txt", "demo/sub", "demo/moved.txt", "demo/copy.txt", "demo/vfs.txt"} {
		if _, ok := fake.Content(p); ok {
			t.Fatalf("backend holds %s after a refused stdio mutation", p)
		}
	}
	if got, ok := fake.Content("demo/shell.txt"); !ok || string(got) != shellContent {
		t.Fatalf("backend copy of shell.txt after refused edit/move/delete: %q (%v)", got, ok)
	}

	// No journal row: C0 found one stuck at pending, needs_publish=1.
	rowsAfter, err := d1.Journal.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsAfter) != len(rowsBefore) {
		t.Fatalf("refused stdio mutations left journal rows: before %d, after %d: %+v", len(rowsBefore), len(rowsAfter), rowsAfter)
	}
	for _, u := range rowsAfter {
		if u.NeedsPublish || u.State == journal.StatePending {
			t.Fatalf("journal row waiting on a publisher after a refused write: %+v", u)
		}
	}

	// No ghost at the mount: C0 saw the entry within a millisecond and
	// EIO on read. The stat is repeated for a while, because "not yet
	// visible" is not the same as "never written".
	ghosts := []string{"side.txt", "sub", "moved.txt", "copy.txt", "vfs.txt"}
	until := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(until) {
		for _, g := range ghosts {
			if _, err := os.Stat(filepath.Join(mountDir, "demo", g)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the mount shows %s after a refused stdio mutation: %v", g, err)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got, err := os.ReadFile(shellPath); err != nil || string(got) != shellContent {
		t.Fatalf("shell.txt at the mount after refused edit/move/delete: %q, %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Join(mountDir, "demo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "shell.txt" {
		t.Fatalf("mount lists %d entries in /demo after refused mutations, want [shell.txt]", len(entries))
	}

	// The stdio side does not believe its own refused write either: C0's
	// stdio stat reported "file, 6 bytes, local".
	if res := call("stat", map[string]any{"path": "/demo/side.txt"}, nil); !res.IsError || !strings.Contains(toolText(res), "does not exist") {
		t.Fatalf("stdio stat of the refused write: IsError=%v %s", res.IsError, toolText(res))
	}

	// Reads over stdio keep working on what the shell wrote: stat,
	// read_text, list_directory and search all see shell.txt and only it.
	var statOut struct {
		Size int64 `json:"size"`
	}
	if res := call("stat", map[string]any{"path": "/demo/shell.txt"}, &statOut); res.IsError || statOut.Size != int64(len(shellContent)) {
		t.Fatalf("stdio stat of the shell's file: %s %+v", toolText(res), statOut)
	}
	var readOut struct {
		Content string `json:"content"`
	}
	if res := call("read_text", map[string]any{"path": "/demo/shell.txt"}, &readOut); res.IsError || readOut.Content != shellContent {
		t.Fatalf("stdio read_text of the shell's file: %s %q", toolText(res), readOut.Content)
	}
	var listOut struct {
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	if res := call("list_directory", map[string]any{"path": "/demo"}, &listOut); res.IsError || len(listOut.Entries) != 1 || listOut.Entries[0].Name != "shell.txt" {
		t.Fatalf("stdio list_directory of /demo: %s %+v", toolText(res), listOut.Entries)
	}
	var searchOut struct {
		Hits []struct {
			Path string `json:"path"`
		} `json:"hits"`
	}
	if res := call("search", map[string]any{"query": "shell"}, &searchOut); res.IsError || len(searchOut.Hits) != 1 || searchOut.Hits[0].Path != "/demo/shell.txt" {
		t.Fatalf("stdio search for the shell's file: %s %+v", toolText(res), searchOut.Hits)
	}
	if res := call("search", map[string]any{"query": "side"}, &searchOut); res.IsError || len(searchOut.Hits) != 0 {
		t.Fatalf("stdio search finds the refused write: %s %+v", toolText(res), searchOut.Hits)
	}
	// A dry-run edit is a read and still works.
	if res := call("edit_file", map[string]any{"path": "/demo/shell.txt", "dry_run": true, "edits": []map[string]any{{"old_text": "shell", "new_text": "agent"}}}, nil); res.IsError {
		t.Fatalf("stdio dry-run edit_file: %s", toolText(res))
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

	// The owner restarts on the same cache: C0 saw RecoverPublications
	// publish and upload the refused write here. Now there is nothing to
	// recover: the shell's file is the only one, once, and the queue is
	// clean.
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
	if _, err := d3.FS.ReadFileRange(ctx3, "/demo/side.txt", 0, 0); !errors.Is(err, vfs.ErrNotFound) {
		t.Fatalf("after restart the refused write is back: %v", err)
	}
	got, err := d3.FS.ReadFileRange(ctx3, "/demo/shell.txt", 0, 0)
	if err != nil || string(got) != shellContent {
		t.Fatalf("after restart the shell's file reads %q, %v", got, err)
	}
	dirEntries, err := d3.FS.ReadDirPath(ctx3, "/demo")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range dirEntries {
		names = append(names, e.Name)
	}
	if len(names) != 1 || names[0] != "shell.txt" {
		t.Fatalf("after restart /demo lists %v, want [shell.txt]", names)
	}
	if st, err := d3.Journal.Stats(ctx3); err != nil || st.Dead > 0 || st.Pending > 0 {
		t.Fatalf("journal after restart: %+v %v", st, err)
	}
	for _, p := range []string{"demo/side.txt", "demo/sub", "demo/moved.txt", "demo/copy.txt", "demo/vfs.txt"} {
		if _, ok := fake.Content(p); ok {
			t.Fatalf("backend holds %s after the owner restarted", p)
		}
	}
	if n := fake.Calls("BeginUpload"); n != uploadsBefore {
		t.Fatalf("the restart uploaded something: %d uploads, was %d", n, uploadsBefore)
	}
	sess, err := d3.Sessions.Get(ctx3, stdioSessions[0].ID)
	if err != nil || sess.State != "finished" {
		t.Fatalf("the stdio session after its process exited: %+v %v", sess, err)
	}
	if c := doctorOf(d3)["agent_db"]; c.Level != control.LevelOK {
		t.Fatalf("agent.db after restart: %+v", c)
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
