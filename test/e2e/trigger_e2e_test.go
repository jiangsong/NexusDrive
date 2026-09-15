package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/control"
	"cloudfs/internal/vfs"
)

// Line E of phase two end to end (TODO.md T-41, T-42): a rule in the
// configuration file, a write at the kernel mount, and a delivery row the
// console can open. The exec is /usr/bin/printf rather than a shell — the
// runner never has one — with the placeholders as their own argv elements,
// so the row's output carries the virtual path and the change kind exactly
// as the child received them.

// triggerMarker is a literal argv element the rule passes through printf,
// so a rendered delivery panel can be told apart from the table row that
// merely names the path.
const triggerMarker = "e2e-trigger-marker-9b1c"

// triggerYAML is the inbox rule under test. origins names what each test
// needs: a kernel-only rule proves the agent's own writes are left out.
func triggerYAML(t *testing.T, origins string) string {
	t.Helper()
	printf, err := exec.LookPath("printf")
	if err != nil {
		t.Skipf("no printf binary for the exec rule: %v", err)
	}
	return fmt.Sprintf(`triggers:
  - name: inbox
    paths: ["/demo/inbox/**"]
    events: [create, write]
    origins: %s
    debounce: 100ms
    action:
      exec: { command: [%q, '%%s\n', "{path}", "{kind}", %q], timeout: 30s }
`, origins, printf, triggerMarker)
}

// deliveriesOf lists the inbox rule's rows through the engine, newest
// first, the way the control plane reads them.
func deliveriesOf(t *testing.T, s *stack) []agent.Delivery {
	t.Helper()
	if s.d.Trigger == nil {
		t.Fatal("the daemon runs no trigger engine although the configuration has a rule")
	}
	rows, _, err := s.d.Trigger.Deliveries(context.Background(), agent.DeliveryQuery{Rule: "inbox"})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// waitDone polls until the inbox rule has a done row for path, or fails
// after the deadline; the row is returned.
func waitDone(t *testing.T, s *stack, path string, within time.Duration) agent.Delivery {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		for _, d := range deliveriesOf(t, s) {
			if d.Path == path && d.State == agent.DeliveryDone {
				return d
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no done delivery for %s within %s: %+v", path, within, deliveriesOf(t, s))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestKernelWriteFiresAnExecTrigger needs a kernel mount: run on Linux
// with /dev/fuse. A file written through the mount by an ordinary
// program reaches the exec rule within five seconds as one done delivery
// whose output holds the virtual path and the change kind the child was
// given as separate arguments.
func TestKernelWriteFiresAnExecTrigger(t *testing.T) {
	s := newStackWith(t, "writeback", stackOptions{extraYAML: triggerYAML(t, "[kernel]")})
	if err := os.MkdirAll(filepath.Join(s.dir, "demo", "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	// echo > mnt/demo/inbox/a.txt
	if err := os.WriteFile(filepath.Join(s.dir, "demo", "inbox", "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := waitDone(t, s, "/demo/inbox/a.txt", 5*time.Second)
	if d.Origin != "kernel" || (d.Kind != "create" && d.Kind != "write") || d.Attempts != 1 {
		t.Fatalf("delivery = %+v", d)
	}
	// printf '%s\n' <path> <kind> <marker>: one line each.
	want := "/demo/inbox/a.txt\n" + d.Kind + "\n" + triggerMarker + "\n"
	if d.Output != want {
		t.Fatalf("output = %q, want %q", d.Output, want)
	}
	// The create and the FLUSH of one write merged into that single row.
	if rows := deliveriesOf(t, s); len(rows) != 1 {
		t.Fatalf("one kernel write should be one delivery: %+v", rows)
	}
}

// TestMCPWriteDoesNotFireWhenAPIIsExcluded needs a kernel mount. With
// origins: [kernel], a write_file over MCP — the agent's own write, which
// would otherwise loop an agent-driven rule — leaves no delivery, and a
// shell write to the same directory right after leaves exactly one.
func TestMCPWriteDoesNotFireWhenAPIIsExcluded(t *testing.T) {
	s := newStackWith(t, "writeback", stackOptions{extraYAML: triggerYAML(t, "[kernel]")})
	if err := os.MkdirAll(filepath.Join(s.dir, "demo", "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if res := s.callTool(t, "write_file", map[string]any{"path": "/demo/inbox/agent.txt", "content": "from the agent"}, nil); res.IsError {
		t.Fatalf("write_file: %s", toolText(res))
	}
	s.settle(t)
	quiet := time.Now().Add(2 * time.Second)
	for time.Now().Before(quiet) {
		if rows := deliveriesOf(t, s); len(rows) != 0 {
			t.Fatalf("an MCP write fired a kernel-only rule: %+v", rows)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got, err := os.ReadFile(filepath.Join(s.dir, "demo", "inbox", "agent.txt")); err != nil || string(got) != "from the agent" {
		t.Fatalf("the agent's file is not at the mount: %q %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "demo", "inbox", "shell.txt"), []byte("from the shell\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := waitDone(t, s, "/demo/inbox/shell.txt", 5*time.Second)
	if d.Origin != "kernel" || !strings.HasPrefix(d.Output, "/demo/inbox/shell.txt\n") {
		t.Fatalf("delivery = %+v", d)
	}
	if rows := deliveriesOf(t, s); len(rows) != 1 {
		t.Fatalf("only the shell write should have a row: %+v", rows)
	}
}

// TestTriggersScreenInTheBrowser renders the triggers screen in headless
// Chromium (CLOUDFS_BROWSER=1): the rule card is there, the delivery the
// engine made is a row with its id and state, and the deep link
// #/triggers?delivery=<id> opens the panel with the exec's output as text.
func TestTriggersScreenInTheBrowser(t *testing.T) {
	s := newUnmountedStack(t, stackOptions{extraYAML: triggerYAML(t, "[kernel, api, remote]")})
	ctx := vfs.WithOrigin(context.Background(), "control")
	root, err := s.d.FS.StatPath(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	demo, err := s.d.FS.Mkdir(ctx, root.Ino, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.d.FS.Mkdir(ctx, demo.Ino, "inbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.d.FS.WriteFile(ctx, "/demo/inbox/a.txt", []byte("hello"), false); err != nil {
		t.Fatal(err)
	}
	d := waitDone(t, s, "/demo/inbox/a.txt", 5*time.Second)
	if d.Origin != "api" || !strings.Contains(d.Output, triggerMarker) {
		t.Fatalf("delivery = %+v", d)
	}
	id := strconv.FormatInt(d.ID, 10)

	// The control API the screen reads: the rule without its command's
	// secrets, the row, and the detail with its output.
	h := control.NewServer(s.d.Collector()).Handler()
	var rules struct {
		Enabled bool `json:"enabled"`
		Rules   []struct {
			Name string `json:"name"`
		} `json:"rules"`
	}
	if w := uiCall(t, h, "GET", "/triggers", ""); json.Unmarshal(w.Body.Bytes(), &rules) != nil || !rules.Enabled || len(rules.Rules) != 1 || rules.Rules[0].Name != "inbox" {
		t.Fatalf("/triggers: %s", w.Body.String())
	}
	if w := uiCall(t, h, "GET", "/triggers/deliveries/"+id, ""); w.Code != 200 || !strings.Contains(w.Body.String(), triggerMarker) {
		t.Fatalf("/triggers/deliveries/%s: %d %s", id, w.Code, w.Body.String())
	}

	chrome := requireBrowser(t)
	base := startControlUI(t, s.d.Collector())
	wants := []string{`data-rule="inbox"`, `data-delivery="` + id + `"`, `data-state="done"`, "/demo/inbox/**"}
	dom := renderedDOM(t, chrome, base+"/?lang=en#/triggers", wants...)
	for _, want := range wants {
		if !strings.Contains(dom, want) {
			t.Fatalf("the triggers screen lacks %s:\n%s", want, dom)
		}
	}
	// The rule card shows the marker as an argv cell; the exec's output —
	// the three lines printf wrote — appears only once the panel is open.
	outputText := "/demo/inbox/a.txt\n" + d.Kind + "\n" + triggerMarker
	if strings.Contains(dom, outputText) {
		t.Fatalf("the table shows output that belongs in the panel:\n%s", dom)
	}
	// The nav lists the screen after #/index, as docs/ui-plan.md orders them.
	index, triggers := strings.Index(dom, `href="#/index"`), strings.Index(dom, `href="#/triggers"`)
	if index < 0 || triggers < 0 || index > triggers {
		t.Fatalf("the nav does not list #/index before #/triggers:\n%s", dom)
	}
	wants = []string{"Delivery #" + id, outputText}
	dom = renderedDOM(t, chrome, base+"/?lang=en#/triggers?delivery="+id, wants...)
	for _, want := range wants {
		if !strings.Contains(dom, want) {
			t.Fatalf("the deep link did not open the delivery panel with %s:\n%s", want, dom)
		}
	}
}
