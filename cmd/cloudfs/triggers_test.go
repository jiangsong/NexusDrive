package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/trigger"

	"gopkg.in/yaml.v3"
)

// triggerCLIConfig is uploadCLIConfig with one exec rule, written through
// the same YAML round trip so the rule reaches the CLI the way a person's
// file would.
func triggerCLIConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	cfg, p := uploadCLIConfig(t)
	cfg.Triggers = []config.Trigger{{
		Name: "inbox", Paths: []string{"/work/inbox/**"}, Origins: []string{"kernel", "remote"},
		Action: config.TriggerAction{Exec: &config.ExecAction{Command: []string{"/bin/cat", "{path}"}}},
	}}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return loaded, p
}

// TestTriggersCLIListsAndRetries: list reads the rules from the file when
// no daemon runs and from the daemon when one does; deliveries and show
// read agent.db offline; test and retry change queue state the daemon
// owns and refuse to run without it.
func TestTriggersCLIListsAndRetries(t *testing.T) {
	cfg, p := triggerCLIConfig(t)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runTriggers(ctx, []string{"list", "--config", p}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "inbox") || !strings.Contains(s, "/bin/cat") || !strings.Contains(s, "{path}") || !strings.Contains(s, "offline") {
		t.Fatalf("offline list: %s", s)
	}
	out.Reset()
	if err := runTriggers(ctx, []string{"deliveries", "--config", p}, &out); err != nil || !strings.Contains(out.String(), "no deliveries") {
		t.Fatalf("offline deliveries without a store: %s %v", out.String(), err)
	}
	if err := runTriggers(ctx, []string{"retry", "1", "--config", p}, &out); err == nil || !strings.Contains(err.Error(), "requires the running daemon") {
		t.Fatalf("offline retry: %v", err)
	}
	if err := runTriggers(ctx, []string{"test", "inbox", "/work/inbox/a.txt", "--config", p}, &out); err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("test without --confirm: %v", err)
	}
	for _, bad := range [][]string{{}, {"dance"}, {"show"}, {"retry"}, {"retry", "x"}, {"test", "inbox"}, {"test", "inbox", "relative", "--confirm"}, {"deliveries", "--state", "odd"}, {"list", "extra"}} {
		if err := runTriggers(ctx, append(bad, "--config", p), &out); err == nil {
			t.Fatalf("%v was accepted", bad)
		}
	}

	// A daemon left a dead delivery behind.
	st, err := agent.Open(filepath.Join(cfg.StateDir(), "agent"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	q := st.Deliveries()
	dead, _, err := q.Enqueue(ctx, "inbox", "/work/inbox/b.txt", "write", "kernel", time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := q.Claim(ctx, "inbox", time.Now()); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := q.Dead(ctx, dead, "exit status 1", "boom from cat"); err != nil {
		t.Fatal(err)
	}
	deadArg := strconv.FormatInt(dead, 10)

	out.Reset()
	if err := runTriggers(ctx, []string{"deliveries", "--config", p, "--state", "dead"}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "offline") || !strings.Contains(s, "inbox") || !strings.Contains(s, "/work/inbox/b.txt") || !strings.Contains(s, "dead") || strings.Contains(s, "boom") {
		t.Fatalf("offline deliveries: %s", s)
	}
	out.Reset()
	if err := runTriggers(ctx, []string{"show", deadArg, "--config", p}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "boom from cat") || !strings.Contains(s, "exit status 1") || !strings.Contains(s, "state: dead") {
		t.Fatalf("offline show: %s", s)
	}

	// The daemon comes up over the same store, with the engine the routes
	// need; it is not run, so the rows stay where the CLI put them.
	eng := trigger.New(trigger.Options{Store: st, Rules: cfg.Triggers})
	defer eng.Close()
	coll := &control.Collector{Agent: control.NewAgentView(st, agent.NewSessions(st, agent.SessionOptions{}), "", control.RollbackDeps{}), Trigger: eng}
	srv, err := control.NewServer(coll).Start(ctx, cfg.Control.Socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	out.Reset()
	if err := runTriggers(ctx, []string{"list", "--config", p, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var rules control.TriggersResponse
	if err := json.Unmarshal(out.Bytes(), &rules); err != nil || !rules.Enabled || len(rules.Rules) != 1 || rules.Rules[0].Action.Exec.Command[1] != "{path}" {
		t.Fatalf("online list: %s %v", out.String(), err)
	}
	out.Reset()
	if err := runTriggers(ctx, []string{"list", "--config", p}, &out); err != nil || strings.Contains(out.String(), "offline") || !strings.Contains(out.String(), "inbox") {
		t.Fatalf("online list table: %s %v", out.String(), err)
	}
	out.Reset()
	if err := runTriggers(ctx, []string{"deliveries", "--config", p, "--rule", "inbox", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var list control.DeliveriesResponse
	if err := json.Unmarshal(out.Bytes(), &list); err != nil || len(list.Deliveries) != 1 || list.Deliveries[0].ID != dead || list.Deliveries[0].State != agent.DeliveryDead {
		t.Fatalf("online deliveries: %s %v", out.String(), err)
	}
	out.Reset()
	if err := runTriggers(ctx, []string{"retry", deadArg, "--config", p}, &out); err != nil || !strings.Contains(out.String(), "pending") {
		t.Fatalf("online retry: %s %v", out.String(), err)
	}
	if d, err := q.Get(ctx, dead); err != nil || d.State != agent.DeliveryPending {
		t.Fatalf("after retry: %+v %v", d, err)
	}
	if err := runTriggers(ctx, []string{"retry", deadArg, "--config", p}, &out); err == nil {
		t.Fatal("retrying a pending row was accepted")
	}
	out.Reset()
	if err := runTriggers(ctx, []string{"test", "inbox", "/work/inbox/a.txt", "--confirm", "--config", p}, &out); err != nil {
		t.Fatalf("online test: %s %v", out.String(), err)
	}
	var res control.TriggerActionResponse
	if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(out.String()), "queued delivery ")), &res.ID); err != nil || res.ID == 0 {
		t.Fatalf("test output: %s %v", out.String(), err)
	}
	if d, err := q.Get(ctx, res.ID); err != nil || d.Kind != trigger.KindTest || d.Path != "/work/inbox/a.txt" {
		t.Fatalf("test row: %+v %v", d, err)
	}
	out.Reset()
	if err := runTriggers(ctx, []string{"show", strconv.FormatInt(res.ID, 10), "--config", p, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var d agent.Delivery
	if err := json.Unmarshal(out.Bytes(), &d); err != nil || d.ID != res.ID || d.Origin != trigger.OriginConsole {
		t.Fatalf("online show: %s %v", out.String(), err)
	}
}
