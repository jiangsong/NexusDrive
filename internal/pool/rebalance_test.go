package pool

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/test/fakeprovider"
)

// capFake is a plain backend that reports no quota of its own, so the
// pool sizes it from the configured capacity minus what it placed there —
// the member_usage total. That is the common case: most drives either
// report nothing or report the whole account, not the pool's share.
func capFake(name string) *fakeprovider.Fake { return fakeprovider.New(name) }

// fillPool writes n files of the given size and lets repair reach the
// replica target, so the pool starts out as an unbalanced one does.
func fillPool(t *testing.T, ctx context.Context, p *Pool, dirID string, n int, size int) {
	t.Helper()
	content := make([]byte, size)
	for i := range content {
		content[i] = byte('a' + i%26)
	}
	for i := 0; i < n; i++ {
		upload(t, ctx, p, dirID, fmt.Sprintf("f%02d.bin", i), content)
	}
	for i := 0; i < n*3; i++ {
		if _, err := p.RepairOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRebalancePlansAboutAThirdOntoANewDrive: a drive added to a pool
// that already holds data stays empty forever without this — placement
// only steers new writes.
func TestRebalancePlansAboutAThirdOntoANewDrive(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	a, b, c := capFake("a"), capFake("b"), capFake("c")
	p, err := New(Options{Name: "home", StateDir: t.TempDir(),
		Settings: config.Pool{Replicas: 2, MinReplicas: 1, TrimGrace: time.Hour},
		Members: []Member{
			{Name: "a", Provider: a, Adopt: true, Domain: "a", Capacity: 128 << 10},
			{Name: "b", Provider: b, Adopt: true, Domain: "b", Capacity: 128 << 10},
			{Name: "c", Provider: c, Adopt: true, Domain: "c", Capacity: 128 << 10},
		},
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	docs, _ := p.Mkdir(ctx, rootID, "docs")

	// c is out of service while the pool fills up, so everything lands on
	// a and b — the shape a pool has the moment a drive is added.
	if err := p.SetMemberState("c", "disabled"); err != nil {
		t.Fatal(err)
	}
	fillPool(t, ctx, p, docs.ID, 12, 4096)
	if err := p.SetMemberState("c", "enabled"); err != nil {
		t.Fatal(err)
	}
	if p.memberFiles(ctx, "c") != 0 {
		t.Fatal("the new drive should start empty")
	}

	plan, err := p.PlanRebalance(ctx, 0.05, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Moves) == 0 {
		t.Fatalf("nothing planned: %+v", plan)
	}
	if plan.Planned {
		t.Fatal("a dry run must not queue anything")
	}
	var queued int
	_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rebalance_queue`).Scan(&queued)
	if queued != 0 {
		t.Fatalf("a dry run queued %d moves", queued)
	}
	for _, mv := range plan.Moves {
		if mv.To != "c" {
			t.Fatalf("move %+v does not go to the empty drive", mv)
		}
	}

	// Running it for real levels the pool out.
	if _, err := p.PlanRebalance(ctx, 0.05, false); err != nil {
		t.Fatal(err)
	}
	before := p.Skew(ctx)
	for i := 0; i < 40; i++ {
		n, err := p.RebalanceOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	after := p.Skew(ctx)
	if after >= before {
		t.Fatalf("skew went from %.3f to %.3f", before, after)
	}
	if p.memberFiles(ctx, "c") == 0 {
		t.Fatal("the new drive is still empty after a rebalance")
	}
	// Every file still has its replicas: a move is a copy and then a
	// delete, never the other way round.
	for i := 0; i < 12; i++ {
		pth := fmt.Sprintf("/docs/f%02d.bin", i)
		if got := liveCount(t, p, pth); got < 2 {
			t.Fatalf("%s has %d replicas after the rebalance", pth, got)
		}
	}
	st, err := p.RebalanceStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Done == 0 || st.BytesMoved == 0 || st.Failed != 0 {
		t.Fatalf("status = %+v", st)
	}
}

func TestRebalancePlanReservesDestinationFreeSpace(t *testing.T) {
	ctx := context.Background()
	a, b := capFake("a"), capFake("b")
	p := newRulePool(t, config.Pool{Replicas: 1, MinReplicas: 1},
		Member{Name: "a", Provider: a, Adopt: true, Domain: "a", Capacity: 1024},
		Member{Name: "b", Provider: b, Adopt: true, Domain: "b", Capacity: 1024})
	docs, _ := p.Mkdir(ctx, rootID, "docs")
	if err := p.SetMemberState("b", "disabled"); err != nil {
		t.Fatal(err)
	}
	upload(t, ctx, p, docs.ID, "large.bin", make([]byte, 80))
	upload(t, ctx, p, docs.ID, "small.bin", make([]byte, 30))
	if err := p.SetMemberState("b", "enabled"); err != nil {
		t.Fatal(err)
	}
	// An overcommitted source can legitimately report Used > Total. Give the
	// destination exactly 100 bytes free: each file fits by itself, but both
	// do not.
	a.SetQuota(100, 300)
	b.SetQuota(100, 0)

	plan, err := p.PlanRebalance(ctx, 0.10, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Bytes > 100 {
		t.Fatalf("planned %d bytes into 100 bytes of free space: %+v", plan.Bytes, plan.Moves)
	}
	if len(plan.Moves) != 1 || plan.Moves[0].Size != 80 {
		t.Fatalf("moves = %+v, want only the largest file that fits", plan.Moves)
	}
}

// TestRebalanceCopiesBeforeItDeletes: the file must exist on both members
// in between, never on neither, so a crash mid-move costs a surplus copy
// rather than a replica.
func TestRebalanceCopiesBeforeItDeletes(t *testing.T) {
	ctx := context.Background()
	a, b := capFake("a"), capFake("b")
	p := newRulePool(t, config.Pool{Replicas: 1, MinReplicas: 1},
		Member{Name: "a", Provider: a, Adopt: true, Domain: "a", Capacity: 1 << 20},
		Member{Name: "b", Provider: b, Adopt: true, Domain: "b", Capacity: 1 << 20})
	docs, _ := p.Mkdir(ctx, rootID, "docs")
	upload(t, ctx, p, docs.ID, "one.bin", []byte("move me"))

	src, dst := p.byName["a"], p.byName["b"]
	if _, ok := b.Content("/docs/one.bin"); ok {
		src, dst = p.byName["b"], p.byName["a"]
	}
	// The delete fails: the copy must already be on the destination, and
	// the move must report the failure rather than pretend it worked.
	srcFake := a
	if src.name == "b" {
		srcFake = b
	}
	srcFake.SetFaults(func(f *fakeprovider.Faults) { f.FailOp = map[string]int{"Delete": 1} })
	err := p.moveReplica(ctx, "/docs/one.bin", src, dst)
	if err == nil {
		t.Fatal("a move whose delete failed must not report success")
	}
	if got := liveCount(t, p, "/docs/one.bin"); got != 2 {
		t.Fatalf("live = %d, want the file on both members after a failed delete", got)
	}
}

// TestRebalanceStandsAsideWhileTheMachineIsBusy: a move is a whole file
// in each direction; it must not compete with a read someone is waiting
// for.
func TestRebalanceStandsAsideWhileTheMachineIsBusy(t *testing.T) {
	ctx := context.Background()
	a, b := capFake("a"), capFake("b")
	p := newRulePool(t, config.Pool{Replicas: 1, MinReplicas: 1},
		Member{Name: "a", Provider: a, Adopt: true, Domain: "a", Capacity: 1 << 20},
		Member{Name: "b", Provider: b, Adopt: true, Domain: "b", Capacity: 1 << 20})
	docs, _ := p.Mkdir(ctx, rootID, "docs")
	upload(t, ctx, p, docs.ID, "one.bin", []byte("busy"))

	from, to := "a", "b"
	if _, ok := b.Content("/docs/one.bin"); ok {
		from, to = "b", "a"
	}
	if _, err := p.db.ExecContext(ctx, `INSERT INTO rebalance_queue(path, from_member, to_member, size, plan_id, created_at)
		VALUES(?, ?, ?, ?, 'plan', 1)`, "/docs/one.bin", from, to, 4); err != nil {
		t.Fatal(err)
	}
	busy := true
	p.SetBusy(func() bool { return busy })
	if n, err := p.RebalanceOnce(ctx); err != nil || n != 0 {
		t.Fatalf("moved %d while busy (%v)", n, err)
	}
	busy = false
	if n, err := p.RebalanceOnce(ctx); err != nil || n != 1 {
		t.Fatalf("moved %d when free (%v)", n, err)
	}
}

// TestTrimDropsTheWorstPlacedCopyNotTheLastDeclared: after a rebalance
// the newest copy is often on the last-declared member, and trimming by
// declaration order would immediately undo the move.
func TestTrimDropsTheWorstPlacedCopyNotTheLastDeclared(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	// The fuller member has room for one more byte; placement ranks it
	// last, and that is the copy trim must drop.
	full, room := capFake("full"), capFake("room")
	for _, f := range []*fakeprovider.Fake{full, room} {
		f.Seed("/f.txt", []byte("F"))
	}
	p, err := New(Options{Name: "home", StateDir: t.TempDir(),
		Settings: config.Pool{Replicas: 1, MinReplicas: 1, TrimGrace: time.Hour},
		Members: []Member{
			{Name: "full", Provider: full, Adopt: true, Domain: "full", Capacity: 2},
			{Name: "room", Provider: room, Adopt: true, Domain: "room", Capacity: 1 << 20},
		},
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, _, err := p.List(ctx, rootID, ""); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if n, err := p.TrimOnce(ctx); err != nil || n != 1 {
		t.Fatalf("trimmed %d (%v)", n, err)
	}
	if _, ok := full.Content("/f.txt"); ok {
		t.Fatal("trim kept the copy on the fullest member")
	}
	if _, ok := room.Content("/f.txt"); !ok {
		t.Fatal("trim dropped the copy placement wanted to keep")
	}
}

// TestBackfillPlansWhenADriveIsAddedEmpty: adding a drive should change
// something about the files that are already there, not only about the
// next write. Start plans that for an empty member on its own.
func TestBackfillPlansWhenADriveIsAddedEmpty(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	a, b := capFake("a"), capFake("b")
	p, err := New(Options{Name: "home", StateDir: t.TempDir(),
		Settings: config.Pool{Replicas: 1, MinReplicas: 1},
		Members: []Member{
			{Name: "a", Provider: a, Adopt: true, Domain: "a", Capacity: 64 << 10},
			{Name: "b", Provider: b, Adopt: true, Domain: "b", Capacity: 64 << 10},
		},
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	docs, _ := p.Mkdir(ctx, rootID, "docs")
	if err := p.SetMemberState("b", "disabled"); err != nil {
		t.Fatal(err)
	}
	fillPool(t, ctx, p, docs.ID, 6, 4096)
	if err := p.SetMemberState("b", "enabled"); err != nil {
		t.Fatal(err)
	}

	p.backfillIfNeeded(ctx)
	var queued int
	if err := p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rebalance_queue WHERE state = 'pending'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued == 0 {
		t.Fatal("adding an empty drive queued no backfill")
	}
	// Nothing to do once the pool is level: a second call must not queue
	// the same work again.
	for i := 0; i < 20; i++ {
		if n, err := p.RebalanceOnce(ctx); err != nil || n == 0 {
			break
		}
	}
	if err := p.ClearRebalance(ctx); err != nil {
		t.Fatal(err)
	}
	p.backfillIfNeeded(ctx)
	_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rebalance_queue`).Scan(&queued)
	if queued != 0 {
		t.Fatalf("a level pool queued %d more moves", queued)
	}
}

// TestRebalanceKeepsTheFileWhenTheDestinationDisappears: a member that
// goes away mid-plan must cost the move, not the file.
func TestRebalanceKeepsTheFileWhenTheDestinationDisappears(t *testing.T) {
	ctx := context.Background()
	a, b := capFake("a"), capFake("b")
	p := newRulePool(t, config.Pool{Replicas: 1, MinReplicas: 1, ProbeInterval: time.Hour},
		Member{Name: "a", Provider: a, Adopt: true, Domain: "a", Capacity: 64 << 10},
		Member{Name: "b", Provider: b, Adopt: true, Domain: "b", Capacity: 64 << 10})
	docs, _ := p.Mkdir(ctx, rootID, "docs")
	upload(t, ctx, p, docs.ID, "one.bin", []byte("keep me"))

	from, to := "a", "b"
	if _, ok := b.Content("/docs/one.bin"); ok {
		from, to = "b", "a"
	}
	if _, err := p.db.ExecContext(ctx, `INSERT INTO rebalance_queue(path, from_member, to_member, size, plan_id, created_at)
		VALUES(?, ?, ?, ?, 'plan', 1)`, "/docs/one.bin", from, to, 7); err != nil {
		t.Fatal(err)
	}
	dstFake := b
	if to == "a" {
		dstFake = a
	}
	dstFake.SetFaults(func(f *fakeprovider.Faults) { f.Down = true })

	if _, err := p.RebalanceOnce(ctx); err == nil {
		t.Fatal("a move to a member that is gone must report the failure")
	}
	if got := liveCount(t, p, "/docs/one.bin"); got != 1 {
		t.Fatalf("live = %d, want the source copy untouched", got)
	}
	var state, lastErr string
	if err := p.db.QueryRowContext(ctx, `SELECT state, last_error FROM rebalance_queue WHERE path = ?`, "/docs/one.bin").Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || lastErr == "" {
		t.Fatalf("queue row = %q %q, want it retried with the reason recorded", state, lastErr)
	}
}

// TestRebalanceHoldsItselfToTheConfiguredRate: housekeeping must not be
// the reason a drive is busy, so a move pays back the time its bytes
// would have cost at max_rate before the next one starts.
func TestRebalanceHoldsItselfToTheConfiguredRate(t *testing.T) {
	ctx := context.Background()
	a, b := capFake("a"), capFake("b")
	p := newRulePool(t, config.Pool{Replicas: 1, MinReplicas: 1,
		Rebalance: config.PoolRebalance{MaxRate: 1000}}, // 1000 bytes per second
		Member{Name: "a", Provider: a, Adopt: true, Domain: "a", Capacity: 64 << 10},
		Member{Name: "b", Provider: b, Adopt: true, Domain: "b", Capacity: 64 << 10})
	docs, _ := p.Mkdir(ctx, rootID, "docs")
	content := make([]byte, 200)
	for i := range content {
		content[i] = byte('a' + i%26)
	}
	upload(t, ctx, p, docs.ID, "one.bin", content)

	from, to := "a", "b"
	if _, ok := b.Content("/docs/one.bin"); ok {
		from, to = "b", "a"
	}
	if _, err := p.db.ExecContext(ctx, `INSERT INTO rebalance_queue(path, from_member, to_member, size, plan_id, created_at)
		VALUES(?, ?, ?, ?, 'plan', 1)`, "/docs/one.bin", from, to, len(content)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if n, err := p.RebalanceOnce(ctx); err != nil || n != 1 {
		t.Fatalf("moved %d (%v)", n, err)
	}
	// 200 bytes at 1000 bytes/s is 200ms of airtime.
	if took := time.Since(start); took < 150*time.Millisecond {
		t.Fatalf("the move took %s, faster than max_rate allows", took)
	}
}
