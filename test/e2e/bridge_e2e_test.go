package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/daemon"
	"cloudfs/internal/fusefs"
	"cloudfs/internal/mcpsrv"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestStdioBesideMountWritesThroughTheBridge is the positive half of the
// T-43 contract (T-50): with the owner serving HTTP and its bridge secret
// in the agent directory, a stdio server beside the mount forwards its
// writes; the file appears at the mount, uploads exactly once, and the
// owner's audit names the bridged call while the stdio side's says
// forwarded. The negative half, a stdio server with no bridge, stays in
// TestStdioBesideMountRefusesWritesCleanly.
func TestStdioBesideMountWritesThroughTheBridge(t *testing.T) {
	if ok, why := fusefs.Supported(); !ok {
		t.Skipf("FUSE unavailable: %s", why)
	}
	base := t.TempDir()
	cacheDir := filepath.Join(base, "cache")
	mountDir := filepath.Join(base, "mnt")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shared := fmt.Sprintf("bridge-%d", time.Now().UnixNano())
	cfgPath := filepath.Join(base, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(coexistTemplate, cacheDir, shared, mountDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(cacheDir, "agent")

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	d1, err := daemon.Open(ctx1, daemon.Options{Config: cfg, Version: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	defer d1.Close()
	fake := provider.Unwrap(d1.Providers["demo"]).(*fakeprovider.Fake)
	m, err := fusefs.MountFS(fusefs.MountOptions{Options: fusefs.Options{FS: d1.FS, AttrTimeout: time.Second, EntryTimeout: time.Second}, Path: mountDir})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	defer m.Unmount()

	// The owner's HTTP transport, as serveMCPHTTPWith builds it: the
	// bridge secret is written to the agent directory and accepted from
	// loopback.
	owner, err := mcpsrv.New(mcpsrv.Options{FS: d1.FS, Sessions: d1.Sessions, Preimages: d1.Preimages, Version: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	tok, err := mcpsrv.WriteBridgeToken(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(mcpsrv.NewHTTPHandler(owner, mcpsrv.HTTPAuth{Bridge: tok, Verify: d1.Agent.VerifyToken}))
	defer ts.Close()

	if err := os.Mkdir(filepath.Join(mountDir, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The stdio server beside it, with the bridge cmdMCP would build from
	// the secret on disk.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	d2, err := daemon.Open(ctx2, daemon.Options{Config: cfg, Version: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if d2.Journal.Owner() {
		t.Fatal("the second open became the owner")
	}
	secret := mcpsrv.ReadBridgeToken(d2.Agent.Dir())
	if secret != tok {
		t.Fatalf("the stdio side read a different secret: %q", secret)
	}
	srv2, err := mcpsrv.New(mcpsrv.Options{FS: d2.FS, NonOwner: true, Sessions: d2.Sessions, Version: "e2e",
		Bridge: &mcpsrv.BridgeOptions{URL: ts.URL, Token: secret}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Close()
	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = srv2.Run(ctx2, serverT) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-e2e", Version: "0"}, nil)
	cs, err := client.Connect(ctx2, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(name string, args map[string]any, out any) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(ctx2, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out != nil && !res.IsError && res.StructuredContent != nil {
			b, _ := json.Marshal(res.StructuredContent)
			_ = json.Unmarshal(b, out)
		}
		return res
	}

	uploadsBefore := fake.Calls("BeginUpload")
	const content = "written over stdio through the bridge\n"
	var w struct {
		Path       string `json:"path"`
		State      string `json:"state"`
		Reversible bool   `json:"reversible"`
	}
	if res := call("write_file", map[string]any{"path": "/demo/bridged.txt", "content": content}, &w); res.IsError {
		t.Fatalf("bridged write refused: %s", toolText(res))
	}
	if w.Path != "/demo/bridged.txt" || !w.Reversible {
		t.Fatalf("owner's answer: %+v", w)
	}
	if res := call("create_directory", map[string]any{"path": "/demo/sub"}, nil); res.IsError {
		t.Fatalf("bridged mkdir refused: %s", toolText(res))
	}
	// Visible at the mount, once the owner has it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := os.ReadFile(filepath.Join(mountDir, "demo", "bridged.txt"))
		if err == nil && string(got) == content {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mount does not show the bridged write: %q %v", got, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Uploaded exactly once, by the owner.
	for time.Now().Before(deadline) {
		if st, _ := d1.Journal.Stats(context.Background()); st.Pending == 0 && st.Uploading == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got, ok := fake.Content("demo/bridged.txt"); !ok || string(got) != content {
		t.Fatalf("backend after the bridged write: %q (%v)", got, ok)
	}
	if n := fake.Calls("BeginUpload") - uploadsBefore; n != 1 {
		t.Fatalf("the bridged write uploaded %d times", n)
	}
	rows, err := d2.Journal.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range rows {
		if u.NeedsPublish {
			t.Fatalf("a journal row waits on a publisher: %+v", u)
		}
	}
	// The stdio side's read sees the file through the shared meta.
	var st struct {
		Size int64 `json:"size"`
	}
	if res := call("stat", map[string]any{"path": "/demo/bridged.txt"}, &st); res.IsError || st.Size != int64(len(content)) {
		t.Fatalf("stdio stat after the bridged write: %s %+v", toolText(res), st)
	}
	// Two audit rows tell the story: forwarded here, ok on the owner.
	ownerRows, _, err := d1.Agent.Audit(context.Background(), agent.AuditQuery{Tool: "write_file"})
	if err != nil {
		t.Fatal(err)
	}
	stdioRows, _, err := d2.Agent.Audit(context.Background(), agent.AuditQuery{Tool: "write_file"})
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, r := range append(ownerRows, stdioRows...) {
		results = append(results, r.Transport+"="+r.Result)
	}
	joined := strings.Join(results, ",")
	if !strings.Contains(joined, "http-bridge=ok") || !strings.Contains(joined, "stdio=forwarded") {
		t.Fatalf("audit rows: %s", joined)
	}
}
