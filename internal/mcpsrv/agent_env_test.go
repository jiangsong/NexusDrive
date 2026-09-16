package mcpsrv

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/agent"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newAgentEnv is newEnv with a CloudFS session layer in front of the tools:
// every call resolves to a session whose scope is sc. A preimage store is
// wired too, the way the daemon wires one, so the write tools record
// session_ops rows and rollback_session is registered.
func newAgentEnv(t *testing.T, opt Options, sc agent.Scope) (*env, *agent.Store) {
	t.Helper()
	e, st, _ := newPreimageEnv(t, opt, sc)
	return e, st
}

// newPreimageEnv is newAgentEnv returning the preimage store as well.
func newPreimageEnv(t *testing.T, opt Options, sc agent.Scope) (*env, *agent.Store, *agent.Preimages) {
	t.Helper()
	return newPreimageEnvWithMax(t, opt, sc, 32<<20)
}

// newPreimageEnvWithMax is newPreimageEnv with the largest file a preimage
// is kept for.
func newPreimageEnvWithMax(t *testing.T, opt Options, sc agent.Scope, max int64) (*env, *agent.Store, *agent.Preimages) {
	t.Helper()
	dir := t.TempDir()
	st, err := agent.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	pre, err := agent.NewPreimages(st, filepath.Join(dir, "preimages"), nil, max)
	if err != nil {
		t.Fatal(err)
	}
	opt.Sessions = agent.NewSessions(st, agent.SessionOptions{Idle: 30 * time.Minute})
	opt.Scope = &sc
	opt.Preimages = pre
	return newEnv(t, opt), st, pre
}

// newAgentEnvWithoutPreimages is newAgentEnv with no preimage store: the
// session layer alone, as a process that cannot keep preimages runs.
func newAgentEnvWithoutPreimages(t *testing.T, opt Options, sc agent.Scope) (*env, *agent.Store) {
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
