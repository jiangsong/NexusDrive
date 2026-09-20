package journal

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cloudfs/internal/testx"
)

// TestClaimStaysCheapWithAQueueOfDeletes: rm -rf of a large tree queues
// thousands of delete rows, and every Claim asks whether an unfinished
// delete holds a row back. That question must be an index probe, not a scan
// of the queue per candidate: Claim runs inside the journal's write
// transaction, and a Claim that took a second per call held every close(2)
// and every unlink on the mount behind it for as long as the queue drained.
func TestClaimStaysCheapWithAQueueOfDeletes(t *testing.T) {
	if testx.RaceEnabled {
		t.Skip("wall-clock bound; the race detector slows SQLite several times over")
	}
	j, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx := context.Background()
	const n = 6000
	for i := 0; i < n; i++ {
		u := Upload{ID: NewID(), Kind: KindDelete, Remote: "r", RemoteParentID: fmt.Sprintf("dir%d", i%50), Name: fmt.Sprintf("f%d", i), RemoteID: fmt.Sprintf("id%d", i)}
		if err := j.Commit(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	const claims = 200
	for i := 0; i < claims; i++ {
		rows, err := j.Claim(ctx, "r", 1)
		if err != nil || len(rows) != 1 {
			t.Fatalf("claim %d: %v %v", i, rows, err)
		}
		if err := j.Succeed(ctx, rows[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	per := time.Since(start) / claims
	// Mostly two fsyncs. The bound is loose for slow disks and still an
	// order of magnitude under what reading the whole due set per Claim
	// cost at this size, let alone the scan per candidate before it.
	t.Logf("claim + succeed: %v each", per)
	if per > 20*time.Millisecond {
		t.Fatalf("claim + succeed took %v each with %d delete rows queued", per, n)
	}
}

// TestClaimWalksBlockedRowsWithoutHoldingTheWriteLock: rm -rf of a tree
// and then a copy of the same tree, while the deletes are still draining,
// queues hundreds of writes that every Claim has to walk past — each held
// back by the delete of its name. That walk must be cheap and, above all,
// must not happen inside the journal's write transaction: the foreground
// commits (close(2), unlink) wait for that lock, and the shape here paced
// an rm -rf at the workers' speed.
func TestClaimWalksBlockedRowsWithoutHoldingTheWriteLock(t *testing.T) {
	if testx.RaceEnabled {
		t.Skip("wall-clock bound; the race detector slows SQLite several times over")
	}
	j, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx := context.Background()
	const blocked = 1500
	// The deletes first, then a write of every deleted name: all the
	// writes are held back, and the claimable rows are the deletes, which
	// the walk reaches only after the writes (queued earlier by rowid in a
	// real rm -rf + cp -r they are not; here the point is the walk).
	for i := 0; i < blocked; i++ {
		u := Upload{ID: NewID(), Remote: "r", RemoteParentID: "dir", Name: fmt.Sprintf("f%d", i), BlobPath: "", Size: 0, Ino: uint64(1000 + i)}
		if err := j.Commit(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < blocked; i++ {
		u := Upload{ID: NewID(), Kind: KindDelete, Remote: "r", RemoteParentID: "dir", Name: fmt.Sprintf("f%d", i), RemoteID: fmt.Sprintf("id%d", i)}
		if err := j.Commit(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	rows, err := j.Claim(ctx, "r", 1)
	if err != nil || len(rows) != 1 || rows[0].Kind != KindDelete {
		t.Fatalf("claim = %+v %v, want the first delete", rows, err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("a Claim past %d held-back rows took %v", blocked, d)
	}
	// Workers claiming continuously must not stall a foreground commit.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			rows, err := j.Claim(ctx, "r", 1)
			if err == nil && len(rows) == 1 {
				_ = j.Succeed(ctx, rows[0].ID)
			}
		}
	}()
	var worst time.Duration
	for i := 0; i < 20; i++ {
		u := Upload{ID: NewID(), Kind: KindDelete, Remote: "r", RemoteParentID: "other", Name: fmt.Sprintf("g%d", i), RemoteID: fmt.Sprintf("gid%d", i)}
		t0 := time.Now()
		if err := j.Commit(ctx, u); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(t0); d > worst {
			worst = d
		}
	}
	close(stop)
	<-done
	t.Logf("worst commit latency under a claiming worker: %v", worst)
	if worst > 200*time.Millisecond {
		t.Fatalf("a foreground commit waited %v behind the workers' claims", worst)
	}
}

// TestStatsStaysCheapWithAQueueUnderQueuedDirectories: the status document
// asks for Stats every second, and a copy of a tree queues thousands of
// files under hundreds of directories the backend does not have yet.
// Counting the rows a dead directory holds back joined the table to itself
// on a computed key — a full scan per row — and with twenty thousand rows
// queued one Stats took longer than the second it had, the collector
// stacked up behind itself and the daemon spent three cores on it.
func TestStatsStaysCheapWithAQueueUnderQueuedDirectories(t *testing.T) {
	if testx.RaceEnabled {
		t.Skip("wall-clock bound; the race detector slows SQLite several times over")
	}
	j, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx := context.Background()
	const dirs, perDir = 400, 40
	for d := 0; d < dirs; d++ {
		dir := Upload{ID: NewID(), Kind: KindMkdir, Remote: "r", RemoteParentID: "root", Name: fmt.Sprintf("d%d", d), Ino: uint64(10 + d)}
		if err := j.Commit(ctx, dir); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < perDir; i++ {
			f := Upload{ID: NewID(), Remote: "r", RemoteParentID: LocalIDPrefix + dir.ID, Name: fmt.Sprintf("f%d", i), Ino: uint64(100000 + d*perDir + i)}
			if err := j.Commit(ctx, f); err != nil {
				t.Fatal(err)
			}
		}
	}
	start := time.Now()
	st, err := j.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	t.Logf("stats over %d rows: %v (blocked=%d)", dirs*perDir+dirs, took, st.Blocked)
	if st.Pending != dirs*perDir+dirs || st.Blocked != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if took > 300*time.Millisecond {
		t.Fatalf("Stats took %v with %d rows queued", took, dirs*perDir+dirs)
	}
	// One directory dead: everything under it is blocked, and the count
	// is still an index walk, not a scan.
	rows, err := j.Claim(ctx, "r", 1)
	if err != nil || len(rows) != 1 || !rows[0].IsMkdir() {
		t.Fatalf("claim: %+v %v", rows, err)
	}
	if err := j.Fail(ctx, rows[0].ID, fmt.Errorf("gone")); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	st, err = j.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 300*time.Millisecond {
		t.Fatalf("Stats took %v with a dead directory", took)
	}
	if st.Blocked != perDir {
		t.Fatalf("blocked = %d, want %d", st.Blocked, perDir)
	}
}
