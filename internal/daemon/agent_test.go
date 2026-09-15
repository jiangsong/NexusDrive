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
