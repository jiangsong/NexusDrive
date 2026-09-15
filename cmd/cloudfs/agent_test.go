package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/control"
)

func TestAuditFlagsBuildTheQuery(t *testing.T) {
	q, asJSON, err := auditQueryFromArgs([]string{"--session", "s1", "--tool", "delete", "--result", "denied", "--since", "1h", "--limit", "20", "--json"}, time.Unix(7200, 0))
	if err != nil {
		t.Fatal(err)
	}
	if q.Session != "s1" || q.Tool != "delete" || q.Result != "denied" || q.Limit != 20 || q.Since.Unix() != 3600 || !asJSON {
		t.Fatalf("%+v json=%v", q, asJSON)
	}
	q, _, err = auditQueryFromArgs([]string{"--since", "2026-09-15T00:00:00Z"}, time.Unix(7200, 0))
	if err != nil || !q.Since.Equal(time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("absolute since: %+v %v", q, err)
	}
	for _, bad := range [][]string{{"--result", "maybe"}, {"--since", "yesterday"}, {"--limit", "0"}, {"--bogus", "x"}, {"positional"}} {
		if _, _, err := auditQueryFromArgs(bad, time.Now()); err == nil {
			t.Fatalf("%v was accepted", bad)
		}
	}
}

// agentCLIStore seeds agent.db under the CLI config's cache directory with
// one session and two audit rows, the way a daemon would have left it.
func agentCLIStore(t *testing.T, cacheDir string) agent.Session {
	t.Helper()
	st, err := agent.Open(filepath.Join(cacheDir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := agent.NewSessions(st, agent.SessionOptions{})
	ctx := context.Background()
	p, err := m.EnsurePrincipal(ctx, "stdio", "local", agent.Scope{Read: []string{"/work"}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Resolve(ctx, agent.ConnInfo{Key: "stdio:1", Transport: "stdio", PrincipalID: p.ID, ClientName: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []agent.AuditRow{
		{SessionID: s.ID, Tool: "write_file", Paths: []string{"/work/a"}, Result: "ok", BytesIn: 12},
		{SessionID: s.ID, Tool: "delete", Paths: []string{"/gd/x"}, Result: "denied"},
	} {
		if _, err := st.AppendAudit(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestAuditAndSessionsCLIReadTheStoreOfflineAndOnline(t *testing.T) {
	cfg, p := uploadCLIConfig(t)
	s := agentCLIStore(t, cfg.Cache.Dir)
	ctx := context.Background()
	for _, online := range []bool{false, true} {
		if online {
			// The daemon's view over the same store: a second Open is the
			// non-owner, which is what a status process beside a mount is.
			st, err := agent.Open(filepath.Join(cfg.Cache.Dir, "agent"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			coll := &control.Collector{Agent: control.NewAgentView(st, agent.NewSessions(st, agent.SessionOptions{}), "")}
			srv, err := control.NewServer(coll).Start(ctx, cfg.Control.Socket, "")
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Close()
		}
		var out bytes.Buffer
		if err := runAudit(ctx, []string{"--result", "denied", "--config", p, "--json"}, &out); err != nil {
			t.Fatalf("online=%v: %v", online, err)
		}
		var audit control.AuditResponse
		if err := json.Unmarshal(out.Bytes(), &audit); err != nil || len(audit.Rows) != 1 || audit.Rows[0].Tool != "delete" || audit.Rows[0].Client != "codex" || audit.Rows[0].Paths[0] != "/gd/x" {
			t.Fatalf("online=%v audit: %s %v", online, out.String(), err)
		}
		out.Reset()
		if err := runAudit(ctx, []string{"--config", p}, &out); err != nil || !strings.Contains(out.String(), "write_file") || !strings.Contains(out.String(), "TOOL") {
			t.Fatalf("online=%v audit table: %s %v", online, out.String(), err)
		}
		out.Reset()
		if err := runSessions(ctx, []string{"list", "--config", p, "--json"}, &out); err != nil {
			t.Fatalf("online=%v: %v", online, err)
		}
		var list control.SessionsResponse
		if err := json.Unmarshal(out.Bytes(), &list); err != nil || len(list.Sessions) != 1 || list.Sessions[0].ID != s.ID || list.Sessions[0].Writes != 1 || list.Summary.Active != 1 {
			t.Fatalf("online=%v sessions: %s %v", online, out.String(), err)
		}
		out.Reset()
		if err := runSessions(ctx, []string{"show", s.ID, "--config", p}, &out); err != nil {
			t.Fatalf("online=%v: %v", online, err)
		}
		for _, want := range []string{"client: codex", "read /work", "recent calls:", "delete", "denied"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("online=%v show lacks %q:\n%s", online, want, out.String())
			}
		}
		out.Reset()
		if err := runSessions(ctx, []string{"list", "--config", p, "--state", "finished"}, &out); err != nil || !strings.Contains(out.String(), "no agent sessions") {
			t.Fatalf("online=%v finished filter: %s %v", online, out.String(), err)
		}
	}
	var out bytes.Buffer
	if err := runSessions(ctx, []string{"finish", s.ID, "--summary", "done", "--config", p}, &out); err != nil || !strings.Contains(out.String(), "finished") {
		t.Fatalf("online finish: %s %v", out.String(), err)
	}
}

func TestSessionsFinishRequiresTheDaemon(t *testing.T) {
	cfg, p := uploadCLIConfig(t)
	s := agentCLIStore(t, cfg.Cache.Dir)
	var out bytes.Buffer
	err := runSessions(context.Background(), []string{"finish", s.ID, "--config", p}, &out)
	if err == nil || !strings.Contains(err.Error(), "requires the running daemon") {
		t.Fatalf("offline finish: %v", err)
	}
	for _, bad := range [][]string{{"finish"}, {"show"}, {"dance"}, {"list", "extra"}, {"list", "--state", "paused"}, {"list", "--limit", "0"}} {
		if err := runSessions(context.Background(), append(bad, "--config", p), &out); err == nil {
			t.Fatalf("%v was accepted", bad)
		}
	}
}

func TestAuditAndSessionsCLIWithoutAStoreAreEmptyNotErrors(t *testing.T) {
	_, p := uploadCLIConfig(t)
	var out bytes.Buffer
	if err := runAudit(context.Background(), []string{"--config", p}, &out); err != nil || !strings.Contains(out.String(), "no audit rows") {
		t.Fatalf("audit: %s %v", out.String(), err)
	}
	out.Reset()
	if err := runSessions(context.Background(), []string{"--config", p}, &out); err != nil || !strings.Contains(out.String(), "no agent sessions") {
		t.Fatalf("sessions: %s %v", out.String(), err)
	}
	if err := runSessions(context.Background(), []string{"show", "nope", "--config", p}, &out); err == nil {
		t.Fatal("show without a store should fail")
	}
}
