package mcpsrv

import (
	"context"
	"testing"
	"time"

	"cloudfs/internal/agent"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newAgentEnv is newEnv with a CloudFS session layer in front of the tools:
// every call resolves to a session whose scope is sc.
func newAgentEnv(t *testing.T, opt Options, sc agent.Scope) (*env, *agent.Store) {
	t.Helper()
	st, err := agent.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	opt.Sessions = agent.NewSessions(st, agent.SessionOptions{Idle: 30 * time.Minute})
	opt.Scope = &sc
	return newEnv(t, opt), st
}

// secondClient connects another in-memory client to e's server, so two
// connections share one VFS, provider and store but resolve to different
// sessions.
func secondClient(t *testing.T, e *env) *env {
	t.Helper()
	clientT, serverT := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = e.server.Run(ctx, serverT) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(); cancel() })
	return &env{server: e.server, fs: e.fs, fake: e.fake, up: e.up, j: e.j, session: session}
}
