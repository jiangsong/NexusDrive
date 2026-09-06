package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

// TestDoctorSeesThePool: doctor reports what a person must act on in a
// pool — a member that is out, files below target, a missing marker — and
// says what to do.
func TestDoctorSeesThePool(t *testing.T) {
	f := newPoolFixture(t)
	f.a.Seed("/single.txt", []byte("one copy"))
	ctx := context.Background()
	if _, err := f.srv.collector.FS.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	doc := &Doctor{Pools: f.srv.collector.Pools, MemberProviders: map[string]provider.Provider{"a": f.a, "b": f.b}, HoldMaxBytes: 1 << 30, Now: time.Now}
	byName := func(checks []Check) map[string]Check {
		m := map[string]Check{}
		for _, c := range checks {
			m[c.Name] = c
		}
		return m
	}
	m := byName(doc.checkPools(ctx))
	if c := m["pool/home/replicas"]; c.Level != LevelWarn || !strings.Contains(c.Detail, "1 files below") || c.Fix == "" {
		t.Fatalf("replicas check = %+v", c)
	}
	if c := m["pool/home/marker/a"]; c.Level != LevelWarn || !strings.Contains(c.Detail, "no .cloudfs-pool.json") {
		t.Fatalf("marker check before markers = %+v", c)
	}
	if err := f.pool.WriteMarkers(ctx); err != nil {
		t.Fatal(err)
	}
	m = byName(doc.checkPools(ctx))
	if c := m["pool/home/marker/a"]; c.Level != LevelOK {
		t.Fatalf("marker check after markers = %+v", c)
	}
	// b dies.
	f.b.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	for i := 0; i < 3; i++ {
		f.srv.collector.FS.DropCaches(ctx)
		f.srv.collector.FS.ReadDirPath(ctx, "/")
	}
	m = byName(doc.checkPools(ctx))
	if c := m["pool/home/member/b"]; c.Level != LevelWarn || !strings.HasPrefix(c.Detail, "down") {
		t.Fatalf("down member = %+v", c)
	}
	if c := m["pool/home/member/a"]; c.Level != LevelOK {
		t.Fatalf("healthy member = %+v", c)
	}
	if _, ok := m["pool/home/marker/b"]; ok {
		t.Fatal("a down member should not be asked for its marker")
	}
}
