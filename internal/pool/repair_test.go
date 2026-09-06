package pool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

func liveCount(t *testing.T, p *Pool, pth string) int {
	t.Helper()
	row, ok, err := p.entryAt(context.Background(), pth)
	if err != nil || !ok {
		t.Fatalf("entry %s: %v %v", pth, ok, err)
	}
	live, err := p.liveReplicas(context.Background(), pth, row.ctoken)
	if err != nil {
		t.Fatal(err)
	}
	return len(live)
}

func TestRepairReachesTargetReplicas(t *testing.T) {
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 3, MinReplicas: 1}, a, b, c)
	ctx := context.Background()
	docs, _ := p.Mkdir(ctx, rootID, "docs")
	e := upload(t, ctx, p, docs.ID, "r.txt", []byte("replicate me"))
	if liveCount(t, p, "/docs/r.txt") != 1 {
		t.Fatal("a fresh upload has one replica")
	}
	made, err := p.RepairOnce(ctx)
	if err != nil || made != 2 {
		t.Fatalf("repair made %d, %v", made, err)
	}
	for _, f := range []*fakeprovider.Fake{a, b, c} {
		if got, ok := f.Content("/docs/r.txt"); !ok || string(got) != "replicate me" {
			t.Fatalf("%s: %q %v (tree %v)", f.Name(), got, ok, f.Tree())
		}
	}
	if liveCount(t, p, "/docs/r.txt") != 3 {
		t.Fatalf("live = %d", liveCount(t, p, "/docs/r.txt"))
	}
	if st, _ := p.RepairStatus(ctx); st.Queued != 0 {
		t.Fatalf("queue after repair: %+v", st)
	}
	// The copies carry the entry's token: listing sees one content, and
	// the version the VFS knows did not move.
	entries, _, _ := p.List(ctx, docs.ID, "")
	if len(entries) != 1 || entries[0].Version != e.Version {
		t.Fatalf("after repair: %+v (want version %s)", entries, e.Version)
	}
	if got := readAll(t, p, e.ID); got != "replicate me" {
		t.Fatalf("read = %q", got)
	}
}

func TestRepairPrefersLocalHold(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	dir := t.TempDir()
	p := newTestPoolWith(t, dir, config.Pool{Replicas: 2, MinReplicas: 1}, a, b)
	ctx := context.Background()
	blob := filepath.Join(dir, "blob")
	os.WriteFile(blob, []byte("from the hold"), 0o600)
	upload(t, provider_ctx(ctx, func(dst string) error { return os.Link(blob, dst) }), p, rootID, "h.txt", []byte("from the hold"))
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Content("/h.txt"); string(got) != "from the hold" {
		t.Fatalf("b = %q", got)
	}
	if a.Calls("ReadRange") != 0 {
		t.Fatalf("repair downloaded from a (%d reads) despite a local hold", a.Calls("ReadRange"))
	}
	// Target reached: the hold is released.
	if holds, _ := filepath.Glob(filepath.Join(p.holdsDir(), "*")); len(holds) != 0 {
		t.Fatalf("holds after reaching the target: %v", holds)
	}
}

func TestRepairStreamsFromALiveReplicaWithoutAHold(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/adopted.txt", []byte("came with the drive"))
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1}, a, b)
	ctx := context.Background()
	if _, _, err := p.List(ctx, rootID, ""); err != nil {
		t.Fatal(err)
	}
	queued, err := p.ScanOnce(ctx)
	if err != nil || queued != 1 {
		t.Fatalf("scan queued %d, %v", queued, err)
	}
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Content("/adopted.txt"); string(got) != "came with the drive" {
		t.Fatalf("b = %q", got)
	}
	if a.Calls("ReadRange") == 0 {
		t.Fatal("the copy had to be read from a")
	}
	if again, _ := p.ScanOnce(ctx); again != 0 {
		t.Fatalf("scan re-queued a fully replicated file: %d", again)
	}
}

func TestRepairSurvivesRestart(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	dir := t.TempDir()
	p := newTestPoolWith(t, dir, config.Pool{Replicas: 2, MinReplicas: 1}, a, b)
	ctx := context.Background()
	upload(t, ctx, p, rootID, "q.txt", []byte("queued"))
	p.Close()
	p2 := newTestPoolWith(t, dir, config.Pool{Replicas: 2, MinReplicas: 1}, a, b)
	if st, _ := p2.RepairStatus(ctx); st.Queued != 1 {
		t.Fatalf("queue after restart: %+v", st)
	}
	if _, err := p2.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Content("/q.txt"); string(got) != "queued" {
		t.Fatalf("b = %q", got)
	}
}

func TestRepairRefreshesStaleReplicaInPlace(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/f.txt", []byte("v1"))
	b.Seed("/f.txt", []byte("v1"))
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1}, a, b)
	ctx := context.Background()
	p.List(ctx, rootID, "")
	upload(t, ctx, p, rootID, "f.txt", []byte("v2, longer"))
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Content("/f.txt"); string(got) != "v2, longer" {
		t.Fatalf("b after repair = %q", got)
	}
	var stale int
	p.db.QueryRow(`SELECT COUNT(*) FROM replicas WHERE path = '/f.txt' AND state <> 'live'`).Scan(&stale)
	if stale != 0 || liveCount(t, p, "/f.txt") != 2 {
		t.Fatalf("stale=%d live=%d", stale, liveCount(t, p, "/f.txt"))
	}
	if b.Calls("Delete") != 0 {
		t.Fatal("refresh in place should not delete first")
	}
}

func TestRepairCapsAtTheMembersItHas(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 3, MinReplicas: 1}, a, b)
	ctx := context.Background()
	upload(t, ctx, p, rootID, "f.txt", []byte("two is all we have"))
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if liveCount(t, p, "/f.txt") != 2 {
		t.Fatalf("live = %d", liveCount(t, p, "/f.txt"))
	}
	if st, _ := p.RepairStatus(ctx); st.Queued != 0 {
		t.Fatalf("a capped target should not spin in the queue: %+v", st)
	}
	if n, _ := p.ScanOnce(ctx); n != 0 {
		t.Fatalf("scan re-queues a file at the capped target: %d", n)
	}
	target, capped := p.replicaTarget()
	if target != 2 || !capped {
		t.Fatalf("target = %d capped=%v", target, capped)
	}
}

func TestOutMemberTriggersReReplication(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	a.Seed("/f.txt", []byte("F"))
	b.Seed("/f.txt", []byte("F"))
	opt := Options{Name: "home", StateDir: t.TempDir(), Settings: config.Pool{Replicas: 2, MinReplicas: 1, OutAfter: 10 * time.Minute, ProbeInterval: time.Hour},
		Members: []Member{{Name: "a", Provider: a}, {Name: "b", Provider: b}, {Name: "c", Provider: c}}, Now: func() time.Time { return now }}
	p, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	p.List(ctx, rootID, "")
	if n, _ := p.ScanOnce(ctx); n != 0 {
		t.Fatalf("fully replicated file queued: %d", n)
	}
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	for i := 0; i < 3; i++ {
		p.List(ctx, rootID, "")
	}
	if st := p.Status()[0].Health.State; st != provider.HealthDown {
		t.Fatalf("a = %s", st)
	}
	// Down is not out: nothing is rebuilt yet.
	if n, _ := p.ScanOnce(ctx); n != 0 {
		t.Fatalf("a merely down member triggered %d repairs", n)
	}
	now = now.Add(11 * time.Minute)
	if st := p.Status()[0].Health.State; st != provider.HealthOut {
		t.Fatalf("a after out_after = %s", st)
	}
	if n, _ := p.ScanOnce(ctx); n != 1 {
		t.Fatalf("an out member should trigger repair, queued %d", n)
	}
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Content("/f.txt"); string(got) != "F" {
		t.Fatalf("c = %q; tree %v", got, c.Tree())
	}
	if liveCount(t, p, "/f.txt") != 2 {
		t.Fatalf("live = %d", liveCount(t, p, "/f.txt"))
	}
	if got := readAll(t, p, "/f.txt"); got != "F" {
		t.Fatalf("read = %q", got)
	}
	// a returns with its old copy: three replicas, and the surplus is a
	// matter for trimming, not for conflict.
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })
	p.ProbeOnce(ctx)
	entries, _, _ := p.List(ctx, rootID, "")
	if got := strings.Join(names(entries), ","); got != "f.txt" {
		t.Fatalf("after a returned: %s", got)
	}
	if liveCount(t, p, "/f.txt") != 3 {
		t.Fatalf("live after return = %d", liveCount(t, p, "/f.txt"))
	}
}

func TestHoldsAreReconciledAtStart(t *testing.T) {
	a := fakeprovider.New("a")
	dir := t.TempDir()
	p := newTestPoolWith(t, dir, config.Pool{Replicas: 2, MinReplicas: 1}, a)
	ctx := context.Background()
	blob := filepath.Join(dir, "blob")
	os.WriteFile(blob, []byte("held"), 0o600)
	upload(t, provider_ctx(ctx, func(dst string) error { return os.Link(blob, dst) }), p, rootID, "h.txt", []byte("held"))
	// An orphan file left by an older process, and a row without a file.
	// A hold being linked right now is younger than the grace and stays.
	orphan := filepath.Join(p.holdsDir(), "orphan")
	os.WriteFile(orphan, []byte("x"), 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(orphan, old, old)
	os.WriteFile(filepath.Join(p.holdsDir(), "in-flight"), []byte("y"), 0o600)
	p.db.Exec(`INSERT INTO holds(hold_path, path, ctoken, size, created_at) VALUES(?, '/ghost', '', 1, 0)`, filepath.Join(p.holdsDir(), "gone"))
	if n := p.reconcileHolds(ctx); n != 1 {
		t.Fatalf("orphans removed = %d", n)
	}
	holds, _ := filepath.Glob(filepath.Join(p.holdsDir(), "*"))
	if len(holds) != 2 { // the real hold and the one still being linked
		t.Fatalf("holds after reconcile = %v", holds)
	}
	var rows int
	p.db.QueryRow(`SELECT COUNT(*) FROM holds`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("hold rows = %d", rows)
	}
}
