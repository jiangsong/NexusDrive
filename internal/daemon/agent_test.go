package daemon

import (
	"context"
	"testing"
	"time"

	"cloudfs/internal/agent"
)

// TestOpenWiresTheAgentStoreForOwnerAndNonOwner: every process that opens
// the daemon gets the agent store, including the one that is not the
// storage owner, because a stdio MCP server beside a mount is exactly the
// process whose tool calls must land in the shared audit trail. Only one of
// them owns the store, so retention runs once.
func TestOpenWiresTheAgentStoreForOwnerAndNonOwner(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	other, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.Journal == nil || other.Journal.Owner() {
		t.Fatal("the second daemon should be the non-owner of the journal")
	}
	for name, d := range map[string]*Daemon{"owner": owner, "other": other} {
		if d.Agent == nil || d.Sessions == nil {
			t.Fatalf("%s: agent store not wired", name)
		}
		if d.Collector().Agent == nil {
			t.Fatalf("%s: collector lacks the agent view", name)
		}
	}
	if !owner.Agent.Owner() || other.Agent.Owner() {
		t.Fatalf("ownership: owner=%v other=%v", owner.Agent.Owner(), other.Agent.Owner())
	}
	// The non-owner appends; the owner reads the row back through its own
	// handle, which is what the console will do.
	if _, err := other.Agent.AppendAudit(ctx, agent.AuditRow{Tool: "stat", Paths: []string{"/demo"}, Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	rows, _, err := owner.Collector().Agent.Audit(ctx, agent.AuditQuery{})
	if err != nil || len(rows) != 1 || rows[0].Tool != "stat" {
		t.Fatalf("shared trail: %+v %v", rows, err)
	}
}

// TestHeatOffRecordsNothing: with mcp.heat.enabled false the owner
// installs no read observer, starts no observer loop, and hands the
// console no heat store — the routes say disabled — while the change
// record and the hooks keep working as before.
func TestHeatOffRecordsNothing(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig)
	off := false
	cfg.MCP.Heat.Enabled = &off
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.ReadHeat != nil {
		t.Fatal("a read observer was started with heat off")
	}
	col := d.Collector()
	if col.HeatStore != nil || col.ReadHeat != nil {
		t.Fatal("the collector serves heat with heat off")
	}
	if col.HookStore == nil || col.Changes == nil {
		t.Fatal("the change record went away with heat")
	}
	on, _ := writeConfig(t, baseConfig)
	if !on.MCP.Heat.On() || on.MCP.Heat.Retention() != 400 {
		t.Fatalf("defaults: on=%v retention=%d", on.MCP.Heat.On(), on.MCP.Heat.Retention())
	}
}
