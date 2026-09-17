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

// TestHistoryAndHeatCLIReadTheStoreOfflineAndOnline: both commands print
// the same rows whether a daemon answers on the control socket or the
// store is read straight from disk; history pages by cursor and names a
// rename's source, heat splits reads by kind; bad flags are refused.
func TestHistoryAndHeatCLIReadTheStoreOfflineAndOnline(t *testing.T) {
	cfg, p := uploadCLIConfig(t)
	ctx := context.Background()
	seed, err := agent.Open(filepath.Join(cfg.Cache.Dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := seed.RecordChanges(ctx, []agent.Change{
		{TS: now.Add(-2 * time.Minute), Path: "/work/a.md", Kind: "write", Origin: "kernel", Reliable: true},
		{TS: now.Add(-time.Minute), Path: "/work/a.md", Kind: "write", Origin: "mcp", SessionID: "sess-0123456789", Principal: "p", Reliable: true},
		{TS: now, Path: "/work/b.md", From: "/work/a.md", Kind: "rename", Origin: "control", Reliable: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := seed.BumpReadHeat(ctx, []agent.ReadSample{
		{Path: "/work/a.md", ActorKind: agent.ReadByAgent, TS: now, Count: 4},
		{Path: "/work/a.md", ActorKind: agent.ReadByKernel, TS: now, Count: 1},
	}); err != nil {
		t.Fatal(err)
	}
	seed.Close()
	for _, online := range []bool{false, true} {
		if online {
			st, err := agent.Open(filepath.Join(cfg.Cache.Dir, "agent"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			coll := &control.Collector{Changes: st, HeatStore: st}
			coll.PublishConfigView(cfg)
			srv, err := control.NewServer(coll).Start(ctx, cfg.Control.Socket, "")
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Close()
		}
		var out bytes.Buffer
		if err := runHistory(ctx, []string{"/work", "--config", p, "--json", "--limit", "2"}, &out); err != nil {
			t.Fatalf("online=%v: %v", online, err)
		}
		var page control.ChangesResponse
		if err := json.Unmarshal(out.Bytes(), &page); err != nil || !page.Enabled || len(page.Changes) != 2 || page.Changes[0].Kind != "rename" || page.Changes[0].From != "/work/a.md" || page.NextCursor == "" {
			t.Fatalf("online=%v history page 1: %s %v", online, out.String(), err)
		}
		out.Reset()
		if err := runHistory(ctx, []string{"/work", "--config", p, "--json", "--limit", "2", "--cursor", page.NextCursor}, &out); err != nil {
			t.Fatal(err)
		}
		var rest control.ChangesResponse
		if err := json.Unmarshal(out.Bytes(), &rest); err != nil || len(rest.Changes) != 1 || rest.Changes[0].Origin != "kernel" || rest.NextCursor != "" {
			t.Fatalf("online=%v history page 2: %s %v", online, out.String(), err)
		}
		out.Reset()
		if err := runHistory(ctx, []string{"/work/a.md", "--config", p}, &out); err != nil || !strings.Contains(out.String(), "ORIGIN") || !strings.Contains(out.String(), "/work/a.md -> /work/b.md") || !strings.Contains(out.String(), "sess-012") {
			t.Fatalf("online=%v history table: %s %v", online, out.String(), err)
		}
		out.Reset()
		if err := runHeat(ctx, []string{"/work", "--config", p, "--json", "--days", "7"}, &out); err != nil {
			t.Fatalf("online=%v: %v", online, err)
		}
		var heat control.HeatResponse
		if err := json.Unmarshal(out.Bytes(), &heat); err != nil || !heat.Enabled || len(heat.Entries) != 1 || heat.Entries[0].Reads != 5 || heat.Entries[0].ByKind["agent"] != 4 {
			t.Fatalf("online=%v heat: %s %v", online, out.String(), err)
		}
		out.Reset()
		if err := runHeat(ctx, []string{"--config", p}, &out); err != nil || !strings.Contains(out.String(), "READS") || !strings.Contains(out.String(), "/work/a.md") {
			t.Fatalf("online=%v heat table: %s %v", online, out.String(), err)
		}
	}
	for _, bad := range [][]string{{"--config", p}, {"/work", "--config", p, "--limit", "0"}, {"/work", "--config", p, "--limit", "x"}} {
		if err := runHistory(ctx, bad, &bytes.Buffer{}); err == nil {
			t.Fatalf("history %v was accepted", bad)
		}
	}
	for _, bad := range [][]string{{"--config", p, "--days", "0"}, {"--config", p, "--limit", "5000"}} {
		if err := runHeat(ctx, bad, &bytes.Buffer{}); err == nil {
			t.Fatalf("heat %v was accepted", bad)
		}
	}
}
