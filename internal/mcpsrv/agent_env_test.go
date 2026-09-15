package mcpsrv

import (
	"testing"
	"time"

	"cloudfs/internal/agent"
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
