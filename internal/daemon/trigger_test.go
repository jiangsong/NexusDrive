package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/vfs"
)

// triggerConfig builds the rule with a resolved path to true(1). The binary
// lives in /bin on Linux and in /usr/bin on macOS, and a rule that names a
// path the host does not have fails the exec three times over and looks like
// a broken engine rather than a broken fixture.
func triggerConfig(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("true(1) not on PATH: %v", err)
	}
	return baseConfig + fmt.Sprintf(`
triggers:
  - name: inbox
    paths: ["/demo/inbox/**"]
    origins: [kernel, remote]
    debounce: 10ms
    action:
      exec: { command: [%q], timeout: 5s }
`, bin)
}

// TestTriggerEngineRunsOnlyInTheOwner: the engine exists in the process
// that owns agent.db and runs background work, never in a second process
// beside it and never for a one-shot command; and it is wired to the real
// change stream, so a write on the VFS leaves a delivery row.
func TestTriggerEngineRunsOnlyInTheOwner(t *testing.T) {
	cfg, _ := writeConfig(t, triggerConfig(t))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	owner, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if owner.Trigger == nil {
		t.Fatal("the owner should run the trigger engine")
	}
	other, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.Trigger != nil {
		t.Fatal("a non-owner must not run a second engine over the same queue")
	}
	// The control plane sees the engine exactly where it runs, and a nil
	// interface (not a typed nil) where it does not.
	if owner.Collector().Trigger == nil {
		t.Fatal("the owner's collector should serve /triggers")
	}
	if other.Collector().Trigger != nil {
		t.Fatal("the non-owner's collector must answer {enabled:false}")
	}

	kctx := vfs.FromKernel(ctx)
	parent, err := owner.FS.StatPath(kctx, "/demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.FS.Mkdir(kctx, parent.Ino, "inbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.FS.WriteFile(kctx, "/demo/inbox/a.txt", []byte("hello"), false); err != nil {
		t.Fatal(err)
	}
	q := owner.Agent.Deliveries()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, _, err := q.List(ctx, agent.DeliveryQuery{Rule: "inbox"})
		if err != nil {
			t.Fatal(err)
		}
		// The mkdir of /demo/inbox is a subtree change above the rule's
		// root and fires too; the file's own row is the one to wait for.
		done := 0
		for _, r := range rows {
			if r.Path == "/demo/inbox/a.txt" && r.State == agent.DeliveryDone && r.Origin == "kernel" {
				done++
			}
		}
		if done >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no done delivery: %+v", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestNoBackgroundSkipsTheTriggerEngine: an offline management command
// must not start running other people's commands.
func TestNoBackgroundSkipsTheTriggerEngine(t *testing.T) {
	cfg, _ := writeConfig(t, triggerConfig(t))
	d, err := Open(context.Background(), Options{Config: cfg, Version: "test", NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Trigger != nil {
		t.Fatal("NoBackground started the trigger engine")
	}
	plain, _ := writeConfig(t, baseConfig)
	d2, err := Open(context.Background(), Options{Config: plain, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if d2.Trigger != nil {
		t.Fatal("an engine was built with no rules and no agents")
	}
}
