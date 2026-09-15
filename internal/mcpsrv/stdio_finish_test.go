package mcpsrv

import (
	"context"
	"testing"

	"cloudfs/internal/agent"
)

// TestFinishStdioSessionsClosesTheProcessSession: a stdio process is its
// session. Once the transport has gone the SDK no longer lists the
// connection, so the server must remember the key itself; finishing is
// idempotent and does not touch a session another connection owns.
func TestFinishStdioSessionsClosesTheProcessSession(t *testing.T) {
	e, st := newAgentEnv(t, Options{Workspace: "/work/.agent"}, agent.Scope{Read: []string{"/work"}})
	ctx := context.Background()
	// Any call resolves the connection's session; a stat of a missing
	// path is the cheapest one.
	e.call(t, "stat", map[string]any{"path": "/work"}, nil)
	sessions := agent.NewSessions(st, agent.SessionOptions{})
	active, _, err := sessions.List(ctx, agent.ListQuery{State: "active"})
	if err != nil || len(active) != 1 || active[0].Transport != "stdio" {
		t.Fatalf("active sessions before: %+v %v", active, err)
	}
	// The transport ends the way a client exiting ends it.
	e.session.Close()
	if n := e.server.FinishStdioSessions(ctx); n != 1 {
		t.Fatalf("finished %d sessions, want 1", n)
	}
	got, err := sessions.Get(ctx, active[0].ID)
	if err != nil || got.State != "finished" {
		t.Fatalf("session after exit: %+v %v", got, err)
	}
	if n := e.server.FinishStdioSessions(ctx); n != 0 {
		t.Fatalf("a second pass finished %d sessions", n)
	}
}
