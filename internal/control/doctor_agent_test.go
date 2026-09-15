package control

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/i18n"
)

// TestDoctorWarnsWhenStdioRunsBesideTheMount: a live heartbeat under the
// agent directory is a stdio MCP server with its own view of the files, and
// the warning names the HTTP transport as the fix; no heartbeat is a pass;
// a stale heartbeat is a pass that also removes the leftover, so a crashed
// server does not keep the diagnostics page yellow.
func TestDoctorWarnsWhenStdioRunsBesideTheMount(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	now := time.Now()
	d := &Doctor{AgentDir: dir, Now: func() time.Time { return now }}

	c, ok := checkByName(d.Run(context.Background()), "agent_stdio")
	if !ok || c.Level != LevelOK {
		t.Fatalf("without a heartbeat: %+v (%v)", c, ok)
	}

	if err := agent.WriteHeartbeat(dir, 31337); err != nil {
		t.Fatal(err)
	}
	c, _ = checkByName(d.Run(context.Background()), "agent_stdio")
	if c.Level != LevelWarn || !strings.Contains(c.Detail, "31337") || !strings.Contains(c.Fix, "cloudfs mcp install --transport http") {
		t.Fatalf("beside a live stdio server: %+v", c)
	}
	if zh := c.Localize(i18n.ZH); !strings.Contains(zh.Detail, "并存") || !strings.Contains(zh.Fix, "cloudfs mcp install --transport http") {
		t.Fatalf("did not localize: %+v", zh)
	}

	stale := now.Add(-agent.HeartbeatStale - time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "stdio-31337.hb"), stale, stale); err != nil {
		t.Fatal(err)
	}
	c, _ = checkByName(d.Run(context.Background()), "agent_stdio")
	if c.Level != LevelOK {
		t.Fatalf("a stale heartbeat still warns: %+v", c)
	}
	if _, err := os.Stat(filepath.Join(dir, "stdio-31337.hb")); !os.IsNotExist(err) {
		t.Fatalf("the stale heartbeat was not cleaned up: %v", err)
	}

	// A doctor with neither a store nor a directory has nothing to say.
	if _, ok := checkByName((&Doctor{}).Run(context.Background()), "agent_stdio"); ok {
		t.Fatal("agent_stdio was checked without an agent directory")
	}
}

// TestDoctorReportsAgentDB: an open store is reported with its schema
// version, and the store's own directory serves the heartbeat check when
// the Doctor was not told one.
func TestDoctorReportsAgentDB(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	st, err := agent.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := &Doctor{Agent: st}
	checks := d.Run(context.Background())
	db, ok := checkByName(checks, "agent_db")
	if !ok || db.Level != LevelOK || !strings.Contains(db.Detail, "schema v2") {
		t.Fatalf("agent_db: %+v (%v)", db, ok)
	}
	if zh := db.Localize(i18n.ZH); !strings.Contains(zh.Detail, "agent.db 正常") {
		t.Fatalf("did not localize: %+v", zh)
	}
	if c, ok := checkByName(checks, "agent_stdio"); !ok || c.Level != LevelOK {
		t.Fatalf("agent_stdio from the store's directory: %+v (%v)", c, ok)
	}
	if err := agent.WriteHeartbeat(dir, 4); err != nil {
		t.Fatal(err)
	}
	if c, _ := checkByName(d.Run(context.Background()), "agent_stdio"); c.Level != LevelWarn {
		t.Fatalf("a heartbeat in the store's directory was missed: %+v", c)
	}
	if _, ok := checkByName((&Doctor{}).Run(context.Background()), "agent_db"); ok {
		t.Fatal("agent_db was reported without a store")
	}
}
