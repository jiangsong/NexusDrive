// Package e2e drives the assembled system the way a user does: a config file
// produces a daemon, the daemon produces a real FUSE mount and a real MCP
// server, and the two views agree with each other and with the remote.
package e2e

import (
	"bytes"
	"cloudfs/internal/provider"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	"cloudfs/internal/fusefs"
	"cloudfs/internal/mcpsrv"
	"cloudfs/test/fakeprovider"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	_ "cloudfs/internal/provider/webdav"

	"cloudfs/internal/i18n"
)

type stack struct {
	d       *daemon.Daemon
	mount   *fusefs.Mount
	dir     string
	fake    *fakeprovider.Fake
	session *mcp.ClientSession
	cfg     *config.Config
}

const configTemplate = `
cache:
  dir: %s
  max_size: 1GiB
  min_free: 1MiB
  block_size: 64KiB
proxy:
  rules:
    - FINAL,direct
remotes:
  demo: { type: fake }
mounts:
  - path: %s
    layout:
      /: { remote: demo, root: root, mode: %s }
mcp:
  allow: []
`

func newStack(t *testing.T, mode string) *stack {
	t.Helper()
	if ok, why := fusefs.Supported(); !ok {
		t.Skipf("FUSE unavailable: %s", why)
	}
	base := t.TempDir()
	cacheDir := filepath.Join(base, "cache")
	mountDir := filepath.Join(base, "mnt")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(base, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(configTemplate, cacheDir, mountDir, mode)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	d, err := daemon.Open(ctx, daemon.Options{Config: cfg, Version: "e2e"})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	fake, _ := provider.Unwrap(d.Providers["demo"]).(*fakeprovider.Fake)
	if fake == nil {
		cancel()
		d.Close()
		t.Fatal("the demo remote did not resolve to the fake provider")
	}

	m, err := fusefs.MountFS(fusefs.MountOptions{
		Options: fusefs.Options{FS: d.FS, AttrTimeout: time.Second, EntryTimeout: time.Second},
		Path:    mountDir,
	})
	if err != nil {
		cancel()
		d.Close()
		t.Fatalf("mount: %v", err)
	}

	// The same options cmd/cloudfs hands the server: sessions, preimages
	// and the workspace come from the daemon, so every call is audited and
	// every write tool records what it is about to change.
	srv, err := mcpsrv.New(mcpsrv.Options{FS: d.FS, Version: "e2e", Sessions: d.Sessions, Preimages: d.Preimages, Workspace: cfg.MCP.Workspace})
	if err != nil {
		m.Unmount()
		cancel()
		d.Close()
		t.Fatal(err)
	}
	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		m.Unmount()
		cancel()
		d.Close()
		t.Fatal(err)
	}

	s := &stack{d: d, mount: m, dir: mountDir, fake: fake, session: session, cfg: cfg}
	t.Cleanup(func() {
		session.Close()
		m.Unmount()
		cancel()
		d.Close()
	})
	return s
}

// settle waits for the upload queue to drain.
func (s *stack) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := s.d.Journal.Stats(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st.Pending == 0 && st.Uploading == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := s.d.Journal.Stats(context.Background())
	t.Fatalf("upload queue did not drain: %+v", st)
}

func (s *stack) callTool(t *testing.T, name string, args any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
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

func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// TestShellAndAgentSeeTheSameFilesystem is the core promise of the system: a
// person working in a terminal and an agent working over MCP operate on one
// filesystem, and each sees the other's writes.
func TestShellAndAgentSeeTheSameFilesystem(t *testing.T) {
	s := newStack(t, "writeback")

	// The shell creates a file.
	shellFile := filepath.Join(s.dir, "from-shell.txt")
	if err := os.WriteFile(shellFile, []byte("written in the terminal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.settle(t)

	// The agent reads it.
	var read struct {
		Content string `json:"content"`
	}
	res := s.callTool(t, "read_text", map[string]any{"path": "/from-shell.txt"}, &read)
	if res.IsError {
		t.Fatalf("agent could not read the shell's file: %s", toolText(res))
	}
	if read.Content != "written in the terminal\n" {
		t.Fatalf("agent read %q", read.Content)
	}

	// The agent writes a file.
	res = s.callTool(t, "write_file", map[string]any{
		"path": "/from-agent.txt", "content": "written by the agent\n",
	}, nil)
	if res.IsError {
		t.Fatalf("agent write failed: %s", toolText(res))
	}
	s.settle(t)

	// The shell reads it back through the kernel.
	got, err := os.ReadFile(filepath.Join(s.dir, "from-agent.txt"))
	if err != nil {
		t.Fatalf("shell could not read the agent's file: %v", err)
	}
	if string(got) != "written by the agent\n" {
		t.Fatalf("shell read %q", got)
	}

	// Both files reached the remote.
	entries, _, err := s.fake.List(context.Background(), fakeprovider.RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["from-shell.txt"] || !names["from-agent.txt"] {
		t.Fatalf("remote is missing files: %v", names)
	}
}

// TestAgentEditsAFileTheShellThenCompiles mirrors the workflow the MCP server
// exists for: an agent edits code in cloud storage and a local tool consumes it.
func TestAgentEditsThenShellReads(t *testing.T) {
	s := newStack(t, "writeback")
	s.fake.Seed("src/config.json", []byte(`{"mode":"draft","retries":1}`))

	res := s.callTool(t, "edit_file", map[string]any{
		"path": "/src/config.json",
		"edits": []map[string]string{
			{"old_text": `"mode":"draft"`, "new_text": `"mode":"release"`},
		},
	}, nil)
	if res.IsError {
		t.Fatalf("edit failed: %s", toolText(res))
	}
	s.settle(t)

	raw, err := os.ReadFile(filepath.Join(s.dir, "src", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("agent produced invalid JSON: %v (%s)", err, raw)
	}
	if parsed["mode"] != "release" {
		t.Fatalf("edit did not apply: %s", raw)
	}
	if parsed["retries"] != float64(1) {
		t.Fatalf("edit damaged the rest of the file: %s", raw)
	}
}

func TestCrashRecoveryKeepsWrites(t *testing.T) {
	if ok, why := fusefs.Supported(); !ok {
		t.Skipf("FUSE unavailable: %s", why)
	}
	base := t.TempDir()
	cacheDir := filepath.Join(base, "cache")
	mountDir := filepath.Join(base, "mnt")
	os.MkdirAll(mountDir, 0o755)
	cfgPath := filepath.Join(base, "config.yaml")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf(configTemplate, cacheDir, mountDir, "writeback")), 0o600)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// First run: write a file, then tear the daemon down before the upload.
	ctx1, cancel1 := context.WithCancel(context.Background())
	d1, err := daemon.Open(ctx1, daemon.Options{Config: cfg, Version: "e2e"})
	if err != nil {
		cancel1()
		t.Fatal(err)
	}
	d1.Uploader.Stop() // nothing will be uploaded
	if _, err := d1.FS.WriteFile(ctx1, "/survivor.txt", []byte("must survive a crash"), false); err != nil {
		cancel1()
		d1.Close()
		t.Fatal(err)
	}
	st, err := d1.Journal.Stats(ctx1)
	if err != nil || st.Pending != 1 {
		cancel1()
		d1.Close()
		t.Fatalf("expected one queued upload, got %+v (%v)", st, err)
	}
	cancel1()
	d1.Close()

	// Second run against the same cache directory: recovery requeues the write
	// and the new uploader completes it.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	d2, err := daemon.Open(ctx2, daemon.Options{Config: cfg, Version: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := d2.Journal.Stats(ctx2)
		if err != nil {
			t.Fatal(err)
		}
		if st.Pending == 0 && st.Uploading == 0 && st.Done >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	fake2 := provider.Unwrap(d2.Providers["demo"]).(*fakeprovider.Fake)
	entries, _, err := fake2.List(ctx2, fakeprovider.RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	// The second run gets a fresh in-memory fake, so the remote will not have
	// the file; what matters is that the write was never lost locally and the
	// queue drained rather than dead-lettering.
	final, err := d2.Journal.Stats(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	if final.Dead > 0 {
		t.Fatalf("recovery dead-lettered the write: %+v", final)
	}
	if final.Done == 0 {
		t.Fatalf("recovered upload never completed: %+v (remote has %d entries)", final, len(entries))
	}
	// The data is still readable locally through the VFS.
	got, err := d2.FS.ReadFileRange(ctx2, "/survivor.txt", 0, 0)
	if err != nil {
		t.Fatalf("recovered file is not readable: %v", err)
	}
	if string(got) != "must survive a crash" {
		t.Fatalf("recovered content = %q", got)
	}
}

func TestReadOnlyMountRefusesBothInterfaces(t *testing.T) {
	s := newStack(t, "readonly")
	s.fake.Seed("locked.txt", []byte("read only"))

	// Reads work from both sides.
	got, err := os.ReadFile(filepath.Join(s.dir, "locked.txt"))
	if err != nil || string(got) != "read only" {
		t.Fatalf("shell read = %q, %v", got, err)
	}
	var read struct {
		Content string `json:"content"`
	}
	if res := s.callTool(t, "read_text", map[string]any{"path": "/locked.txt"}, &read); res.IsError {
		t.Fatalf("agent read failed: %s", toolText(res))
	}

	// Writes are refused from both sides.
	if err := os.WriteFile(filepath.Join(s.dir, "new.txt"), []byte("x"), 0o644); err == nil {
		t.Fatal("shell write to a read-only mount should fail")
	}
	res := s.callTool(t, "write_file", map[string]any{"path": "/new.txt", "content": "x"}, nil)
	if !res.IsError {
		t.Fatal("agent write to a read-only mount should fail")
	}
	if !strings.Contains(toolText(res), "read-only") {
		t.Fatalf("error should say the mount is read-only: %q", toolText(res))
	}
}

func TestStatusAndMetricsReflectRealWork(t *testing.T) {
	s := newStack(t, "writeback")
	s.fake.Seed("big.bin", bytes.Repeat([]byte("x"), 300_000))

	// Read it so the cache fills.
	if _, err := os.ReadFile(filepath.Join(s.dir, "big.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "written.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.settle(t)

	st := s.d.Collector().Collect(context.Background(), i18n.EN)
	if st.Cache.Blocks == 0 && st.Cache.HydratedFiles == 0 {
		t.Fatalf("cache reports nothing after a 300 KB read: %+v", st.Cache)
	}
	if st.Uploads.Done == 0 {
		t.Fatalf("uploads report nothing after a write: %+v", st.Uploads)
	}
	if len(st.Mounts) != 1 || st.Mounts[0].Remote != "demo" {
		t.Fatalf("mounts = %+v", st.Mounts)
	}
	if len(st.Warnings) != 0 {
		t.Fatalf("a healthy run produced warnings: %v", st.Warnings)
	}

	// The metrics endpoint serves the same numbers.
	srv := control.NewServer(s.d.Collector())
	req, _ := http.NewRequest("GET", "/metrics", nil)
	rec := &recorder{header: http.Header{}}
	srv.Handler().ServeHTTP(rec, req)
	body := rec.body.String()
	for _, want := range []string{"cloudfs_cache_hits_total", "cloudfs_uploads_pending", "cloudfs_meta_nodes"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

type recorder struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(c int)           { r.code = c }

func TestDoctorOnALiveSystem(t *testing.T) {
	s := newStack(t, "writeback")
	checks := s.d.Doctor(nil, fusefs.Supported).Run(context.Background())
	if len(checks) == 0 {
		t.Fatal("doctor produced no checks")
	}
	byName := map[string]control.Check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	for _, name := range []string{"platform", "cache_dir", "metadata_db", "upload_queue", "agent_db", "agent_stdio"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("doctor is missing the %s check", name)
		}
	}
	if c := byName["upload_queue"]; c.Level != control.LevelOK {
		t.Errorf("a healthy queue should pass: %+v", c)
	}
	// One owner and no stdio server beside it: both agent checks pass.
	for _, name := range []string{"agent_db", "agent_stdio"} {
		if c := byName[name]; c.Level != control.LevelOK {
			t.Errorf("%s should pass on a lone owner: %+v", name, c)
		}
	}
	if c := byName["cache_dir"]; c.Level != control.LevelOK {
		t.Errorf("the cache dir should be usable: %+v", c)
	}
}

// TestGrepAcrossTheMount checks the user-facing claim that ordinary tools work
// on the mount and that a warm tree does not re-hit the provider.
func TestGrepAcrossTheMount(t *testing.T) {
	if _, err := exec.LookPath("grep"); err != nil {
		t.Skip("grep unavailable")
	}
	s := newStack(t, "writeback")
	s.fake.Seed("proj/a/main.go", []byte("package main\n// TODO: handle retries\n"))
	s.fake.Seed("proj/b/util.go", []byte("package main\n// nothing\n"))
	s.fake.Seed("proj/c/notes.md", []byte("# TODO: write docs\n"))

	out, err := exec.Command("grep", "-rl", "TODO", s.dir).CombinedOutput()
	if err != nil {
		t.Fatalf("grep failed: %v (%s)", err, out)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) != 2 {
		t.Fatalf("grep found %d files, want 2: %s", len(lines), out)
	}

	// A second pass over the warm tree makes no provider calls.
	before := s.fake.TotalCalls()
	if _, err := exec.Command("grep", "-rl", "TODO", s.dir).CombinedOutput(); err != nil {
		t.Fatal(err)
	}
	if after := s.fake.TotalCalls(); after != before {
		t.Fatalf("warm grep made %d provider calls", after-before)
	}
}

func TestSearchToolFindsWhatTheShellSees(t *testing.T) {
	s := newStack(t, "writeback")
	s.fake.Seed("docs/architecture.md", []byte("# Architecture\n"))
	s.fake.Seed("docs/deployment.md", []byte("# Deployment\n"))
	s.fake.Seed("src/architecture_test.go", []byte("package src\n"))

	// Warm the tree the way a first listing would.
	if _, err := s.d.FS.Warm(context.Background(), "/", -1); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Hits []struct {
			Path string `json:"path"`
		} `json:"hits"`
	}
	res := s.callTool(t, "search", map[string]any{"query": "architecture"}, &out)
	if res.IsError {
		t.Fatalf("search failed: %s", toolText(res))
	}
	if len(out.Hits) != 2 {
		t.Fatalf("search found %d, want 2: %+v", len(out.Hits), out.Hits)
	}
	// Every hit is a path that really exists on the mount.
	for _, h := range out.Hits {
		if _, err := os.Stat(filepath.Join(s.dir, strings.TrimPrefix(h.Path, "/"))); err != nil {
			t.Errorf("search returned %s but the mount does not have it: %v", h.Path, err)
		}
	}
}
