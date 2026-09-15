package daemon

import (
	"context"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/vfs"
)

const triggerConfig = baseConfig + `
triggers:
  - name: inbox
    paths: ["/demo/inbox/**"]
    origins: [kernel, remote]
    debounce: 10ms
    action:
      exec: { command: ["/bin/true"], timeout: 5s }
`

// TestTriggerEngineRunsOnlyInTheOwner: the engine exists in the process
// that owns agent.db and runs background work, never in a second process
// beside it and never for a one-shot command; and it is wired to the real
// change stream, so a write on the VFS leaves a delivery row.
func TestTriggerEngineRunsOnlyInTheOwner(t *testing.T) {
	cfg, _ := writeConfig(t, triggerConfig)
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
	cfg, _ := writeConfig(t, triggerConfig)
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
