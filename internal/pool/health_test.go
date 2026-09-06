package pool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

// TestDownMemberIsSkippedUntilProbe: once a member has failed enough to be
// down, operations stop waiting on it — listings use the snapshot, reads
// go to the other replicas — until a probe finds it answering again.
func TestDownMemberIsSkippedUntilProbe(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/f.txt", []byte("F"))
	b.Seed("/f.txt", []byte("F"))
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1, ProbeInterval: time.Hour, OutAfter: time.Hour}, a, b)
	ctx := context.Background()
	if _, _, err := p.List(ctx, rootID, ""); err != nil {
		t.Fatal(err)
	}
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	for i := 0; i < 3; i++ {
		if _, _, err := p.List(ctx, rootID, ""); err != nil {
			t.Fatal(err)
		}
	}
	if st := p.Status()[0]; st.Health.State != provider.HealthDown {
		t.Fatalf("a after three failed listings = %+v", st.Health)
	}
	refused := a.Calls("down")
	for i := 0; i < 5; i++ {
		if _, _, err := p.List(ctx, rootID, ""); err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, p, "/f.txt"); got != "F" {
			t.Fatalf("read = %q", got)
		}
	}
	if a.Calls("down") != refused {
		t.Fatalf("a down member was still asked %d times", a.Calls("down")-refused)
	}
	// It comes back; nothing notices until the probe runs.
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })
	if _, _, err := p.List(ctx, rootID, ""); err != nil {
		t.Fatal(err)
	}
	if st := p.Status()[0]; st.Health.State != provider.HealthDown {
		t.Fatalf("a should still be considered down before a probe: %s", st.Health.State)
	}
	p.ProbeOnce(ctx)
	if st := p.Status()[0]; st.Health.State != provider.HealthUp {
		t.Fatalf("a after the probe = %s", st.Health.State)
	}
	before := a.Calls("List")
	if _, _, err := p.List(ctx, rootID, ""); err != nil {
		t.Fatal(err)
	}
	if a.Calls("List") == before {
		t.Fatal("a recovered member is not consulted again")
	}
}

// TestRiskControlOnOneMemberDoesNotStopThePool: an account under risk
// control is down for the pool; reads and writes go to the others.
func TestRiskControlOnOneMemberDoesNotStopThePool(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/f.txt", []byte("F"))
	b.Seed("/f.txt", []byte("F"))
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1, ProbeInterval: time.Hour}, a, b)
	ctx := context.Background()
	if _, _, err := p.List(ctx, rootID, ""); err != nil {
		t.Fatal(err)
	}
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.RiskControlAfter = 1 })
	for i := 0; i < 4; i++ {
		if got := readAll(t, p, "/f.txt"); got != "F" {
			t.Fatalf("read %d = %q", i, got)
		}
	}
	if st := p.Status()[0]; st.Health.State == provider.HealthUp {
		t.Fatalf("a under risk control still counts as up")
	}
	if a.Calls("ReadRange") != 1 || b.Calls("ReadRange") != 4 {
		t.Fatalf("reads went a=%d b=%d; a should be avoided after its refusal", a.Calls("ReadRange"), b.Calls("ReadRange"))
	}
	e := upload(t, ctx, p, rootID, "new.txt", []byte("goes to b"))
	if got, ok := b.Content("/new.txt"); !ok || string(got) != "goes to b" {
		t.Fatalf("b = %q, %v", got, ok)
	}
	if _, ok := a.Content("/new.txt"); ok {
		t.Fatal("a member under risk control received a write")
	}
	_ = e
}

// TestReadsPreferTheFasterReplica: with both replicas healthy, reads go
// where they come back soonest.
func TestReadsPreferTheFasterReplica(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/f.txt", []byte("F"))
	b.Seed("/f.txt", []byte("F"))
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Latency = 15 * time.Millisecond })
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()
	if _, _, err := p.List(ctx, rootID, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		readAll(t, p, "/f.txt")
	}
	if a.Calls("ReadRange") >= b.Calls("ReadRange") {
		t.Fatalf("slow a served %d reads, fast b %d", a.Calls("ReadRange"), b.Calls("ReadRange"))
	}
}

// TestStatusNamesEveryMemberInOrder: the control plane reads members from
// here; declaration order is the order a person configured them in.
func TestStatusNamesEveryMemberInOrder(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	p := newTestPool(t, t.TempDir(), a, b)
	var names []string
	for _, m := range p.Status() {
		names = append(names, m.Name+":"+string(m.Health.State))
	}
	if strings.Join(names, ",") != "a:up,b:up" {
		t.Fatalf("status = %v", names)
	}
	if _, err := p.ReadRange(context.Background(), "/nope", "", 0, 1); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("stat: %v", err)
	}
}
