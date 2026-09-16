package control

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/agent"
)

// The heat tab (docs/agent-first-design.md §6.3, ui-plan G6) is the
// console's read-heat × staleness view. These tests read the embedded
// sources the way the other agents-screen tests do, and drive the route
// behind it.
func TestHeatTabIsRoutedAndReadsTheHeatRoute(t *testing.T) {
	agents := webSource(t, "web/screens/agents.js")
	for _, want := range []string{"'heat'", "renderHeatTab", "agents_heat.js"} {
		if !strings.Contains(agents, want) {
			t.Errorf("agents.js lacks %s", want)
		}
	}
	heat := webSource(t, "web/screens/agents_heat.js")
	for _, want := range []string{"api.get('/agent/heat?", "data-quadrant", "hot_stale", "t('heat.kind.' + k)", "t('heat.quadrant.' + "} {
		if !strings.Contains(heat, want) {
			t.Errorf("agents_heat.js lacks %s", want)
		}
	}
	if strings.Contains(heat, "api.post(") {
		t.Error("the heat tab must only suggest; it has no mutation")
	}
	zh := tableKeys(t, webI18nSource(t), "zh")
	for _, k := range []string{"agents.tab.heat", "heat.quadrant.hot_stale", "heat.quadrant.warm_fresh", "heat.kind.agent", "heat.kind.kernel", "heat.disabled", "audit.result.oversize", "audit.result.forwarded", "rollback.pre.too_many", "rollback.pre.expired", "connect.guidance.title", "connect.install.http"} {
		if !zh[k] {
			t.Errorf("missing %s", k)
		}
	}
	for _, r := range heat {
		if r > 0x2E80 {
			t.Fatalf("agents_heat.js carries a non-ASCII character %q; text belongs in the catalogs", r)
		}
	}
}

// TestConnectPanelShowsGuidanceAndInstallTransport: the connect panel asks
// for the instructions text and shows what install will pick.
func TestConnectPanelShowsGuidanceAndInstallTransport(t *testing.T) {
	src := webSource(t, "web/connect_panel.js")
	for _, want := range []string{"api.get('/agent/prompt?kind=instructions')", "data-guidance", "t('connect.guidance.title', g.tokens", "c.install_transport"} {
		if !strings.Contains(src, want) {
			t.Errorf("connect_panel.js lacks %s", want)
		}
	}
}

// TestAgentHeatRouteQuadrantsFromMetaMTimes: the route joins the heat
// table with meta's mtimes, marks stale rows, and answers disabled
// without a store.
func TestAgentHeatRouteQuadrantsFromMetaMTimes(t *testing.T) {
	f, p := fsControl(t)
	s := NewServer(f.coll)
	w := call(t, s, "GET", "/agent/heat", "")
	var off HeatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &off); w.Code != 200 || err != nil || off.Enabled {
		t.Fatalf("without a store: %d %s", w.Code, w.Body.String())
	}
	f2, fake2 := fsControl(t)
	st, err := agent.Open(filepath.Join(f2.dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f2.coll.HeatStore = st
	// The FS fixture seeded /docs/b; make it old and read often, /other fresh.
	_ = p
	old := time.Now().Add(-40 * 24 * time.Hour)
	fake2.SetMTime("/docs/b", old)
	if _, err := f2.coll.FS.ReadDirPath(context.Background(), "/"); err != nil {
		t.Fatal(err)
	}
	if _, err := f2.coll.FS.ReadDirPath(context.Background(), "/docs"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.BumpReadHeat(context.Background(), []agent.ReadSample{
		{Path: "/docs/b", ActorKind: agent.ReadByAgent, TS: now, Count: 9},
		{Path: "/other", ActorKind: agent.ReadByKernel, TS: now, Count: 1},
	}); err != nil {
		t.Fatal(err)
	}
	s2 := NewServer(f2.coll)
	w = call(t, s2, "GET", "/agent/heat?days=7&path=/", "")
	var resp HeatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); w.Code != 200 || err != nil || !resp.Enabled || len(resp.Entries) != 2 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if e := resp.Entries[0]; e.Path != "/docs/b" || e.Quadrant != "hot_stale" || e.MTime == nil || e.ByKind["agent"] != 9 {
		t.Fatalf("hot stale: %+v", e)
	}
	if e := resp.Entries[1]; e.Path != "/other" || e.Quadrant != "warm_fresh" {
		t.Fatalf("warm fresh: %+v", e)
	}
	if w := call(t, s2, "GET", "/agent/heat?path=..%2F..", ""); w.Code == 200 {
		t.Fatal("an unclean path was accepted")
	}
}
