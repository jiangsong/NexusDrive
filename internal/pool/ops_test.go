package pool

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

func pendingCount(t *testing.T, p *Pool, member string) int {
	t.Helper()
	n, err := p.PendingOps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n[member]
}

func TestPendingOpsReplayIsIdempotent(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/docs/f.txt", []byte("F"))
	b.Seed("/docs/f.txt", []byte("F"))
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()
	root, _, _ := p.List(ctx, rootID, "")
	docs, _ := find(root, "docs")
	inner, _, _ := p.List(ctx, docs.ID, "")
	f, _ := find(inner, "f.txt")

	b.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	if _, err := p.Rename(ctx, f.ID, "g.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Mkdir(ctx, docs.ID, "sub"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Rename(ctx, docs.ID, "papers"); err != nil {
		t.Fatal(err)
	}
	if pendingCount(t, p, "b") != 3 {
		t.Fatalf("pending for b = %d", pendingCount(t, p, "b"))
	}
	// While b is down nothing replays.
	if n, _ := p.ReplayOnce(ctx); n != 0 {
		t.Fatalf("replayed %d ops on a down member", n)
	}
	b.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })
	if n, err := p.ReplayOnce(ctx); err != nil || n != 3 {
		t.Fatalf("replay = %d, %v", n, err)
	}
	if got := strings.Join(b.Tree(), ","); got != "/papers/,/papers/g.txt,/papers/sub/" {
		t.Fatalf("b after replay = %s", got)
	}
	if pendingCount(t, p, "b") != 0 {
		t.Fatal("ops left after replay")
	}
	// The renamed file still reads from either member.
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	if got := readAll(t, p, f.ID); got != "F" {
		t.Fatalf("read from b after replay = %q", got)
	}
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })

	// Replaying an op the member already saw applied elsewhere is a no-op:
	// rename again with b down, apply it on b by hand, then replay.
	b.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	if _, err := p.Rename(ctx, f.ID, "h.txt"); err != nil {
		t.Fatal(err)
	}
	b.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })
	id, _ := b.IDOf("/papers/g.txt")
	if _, err := b.Rename(ctx, id, "h.txt"); err != nil {
		t.Fatal(err)
	}
	if n, err := p.ReplayOnce(ctx); err != nil || n != 1 {
		t.Fatalf("replay of an already-applied rename = %d, %v", n, err)
	}
	if _, ok := b.Content("/papers/h.txt"); !ok {
		t.Fatalf("b = %v", b.Tree())
	}
	if divs, _ := p.Divergences(ctx, 10); len(divs) != 0 {
		t.Fatalf("an already-applied op should not be a divergence: %+v", divs)
	}
	papers, _, _ := p.List(ctx, docs.ID, "")
	if got := strings.Join(names(papers), ","); got != "h.txt,sub" {
		t.Fatalf("listing after replays = %s", got)
	}
}

func TestPendingOpsNeverDeleteChangedContent(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/f.txt", []byte("F"))
	b.Seed("/f.txt", []byte("F"))
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()
	entries, _, _ := p.List(ctx, rootID, "")
	f, _ := find(entries, "f.txt")
	b.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	if err := p.Delete(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Content("/f.txt"); ok {
		t.Fatal("a still has the file")
	}
	// Someone edits b's copy in the vendor app before b is back.
	b.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })
	b.Seed("/f.txt", []byte("edited on b while the pool thought it deleted"))
	if n, err := p.ReplayOnce(ctx); err != nil || n != 1 {
		t.Fatalf("replay = %d, %v", n, err)
	}
	if got, ok := b.Content("/f.txt"); !ok || !strings.HasPrefix(string(got), "edited") {
		t.Fatalf("changed content was deleted: %q %v", got, ok)
	}
	divs, _ := p.Divergences(ctx, 10)
	if len(divs) != 1 || divs[0].Kind != "delete-skipped" || divs[0].Member != "b" {
		t.Fatalf("divergences = %+v", divs)
	}
	// It reappears as an adopted file.
	entries, _, _ = p.List(ctx, rootID, "")
	if got := strings.Join(names(entries), ","); got != "f.txt" {
		t.Fatalf("listing = %s", got)
	}
	if got := readAll(t, p, "/f.txt"); !strings.HasPrefix(got, "edited") {
		t.Fatalf("read = %q", got)
	}
	// And a plain replay of a delete on unchanged content does delete.
	c := fakeprovider.New("c")
	c.Seed("/g.txt", []byte("G"))
	p2 := newTestPool(t, t.TempDir(), a, c)
	a.Seed("/g.txt", []byte("G"))
	entries, _, _ = p2.List(ctx, rootID, "")
	g, _ := find(entries, "g.txt")
	c.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	if err := p2.Delete(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	c.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })
	if n, _ := p2.ReplayOnce(ctx); n != 1 {
		t.Fatalf("replay = %d", n)
	}
	if _, ok := c.Content("/g.txt"); ok {
		t.Fatal("unchanged content survived the replayed delete")
	}
}

func TestOpLogTruncationSchedulesFullScrub(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/f.txt", []byte("F"))
	b.Seed("/f.txt", []byte("F"))
	p, err := New(Options{Name: "home", StateDir: t.TempDir(), Settings: config.Pool{Replicas: 2, MinReplicas: 1, OpTTL: time.Hour, ScrubSample: 1},
		Members: []Member{{Name: "a", Provider: a}, {Name: "b", Provider: b}}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	entries, _, _ := p.List(ctx, rootID, "")
	f, _ := find(entries, "f.txt")
	b.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	if _, err := p.Rename(ctx, f.ID, "g.txt"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	b.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })
	if n, _ := p.ReplayOnce(ctx); n != 0 {
		t.Fatalf("an expired op was replayed: %d", n)
	}
	if pendingCount(t, p, "b") != 0 {
		t.Fatal("expired op still pending")
	}
	if !p.byName["b"].needsScrub {
		t.Fatal("the member that missed the log is not flagged for a scrub")
	}
	// The scrub re-lists everything; b's stale copy under the old name
	// becomes visible as what it is: b never applied the rename.
	if _, err := p.ScrubOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if p.byName["b"].needsScrub {
		t.Fatal("scrub flag not cleared")
	}
	entries, _, _ = p.List(ctx, rootID, "")
	got := strings.Join(names(entries), ",")
	if !strings.Contains(got, "g.txt") {
		t.Fatalf("listing after scrub = %s", got)
	}
}

func TestTrimOnlyWhenHashesAgreeAfterGrace(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	for _, f := range []*fakeprovider.Fake{a, b, c} {
		f.Seed("/f.txt", []byte("F"))
	}
	p, err := New(Options{Name: "home", StateDir: t.TempDir(), Settings: config.Pool{Replicas: 2, MinReplicas: 1, TrimGrace: time.Hour},
		Members: []Member{{Name: "a", Provider: a}, {Name: "b", Provider: b}, {Name: "c", Provider: c}}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	p.List(ctx, rootID, "")
	if n, _ := p.TrimOnce(ctx); n != 0 {
		t.Fatalf("trimmed %d within the grace period", n)
	}
	now = now.Add(2 * time.Hour)
	if n, _ := p.TrimOnce(ctx); n != 1 {
		t.Fatalf("trimmed %d after the grace period, want 1", n)
	}
	if _, ok := c.Content("/f.txt"); ok {
		t.Fatal("the surplus should come off the last-declared member")
	}
	if liveCount(t, p, "/f.txt") != 2 {
		t.Fatalf("live = %d", liveCount(t, p, "/f.txt"))
	}

	// Without hashes two copies cannot be proven identical: never trimmed.
	d, e, g := fakeprovider.New("d"), fakeprovider.New("e"), fakeprovider.New("g")
	for _, f := range []*fakeprovider.Fake{d, e, g} {
		f.SetReportHashes(false)
		f.Seed("/h.txt", []byte("H"))
		f.SetMTime("/h.txt", now)
	}
	p2, _ := New(Options{Name: "home2", StateDir: t.TempDir(), Settings: config.Pool{Replicas: 2, MinReplicas: 1, TrimGrace: time.Hour},
		Members: []Member{{Name: "d", Provider: d}, {Name: "e", Provider: e}, {Name: "g", Provider: g}}, Now: func() time.Time { return now }})
	defer p2.Close()
	p2.List(ctx, rootID, "")
	now = now.Add(2 * time.Hour)
	if n, _ := p2.TrimOnce(ctx); n != 0 {
		t.Fatalf("trimmed %d hashless copies", n)
	}
}

func TestDrainMovesEverythingOffThenIsEmpty(t *testing.T) {
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1}, a, b, c)
	ctx := context.Background()
	for i, name := range []string{"/x.txt", "/d/y.txt", "/d/z.txt"} {
		dir := rootID
		if strings.HasPrefix(name, "/d/") {
			if _, _, err := p.List(ctx, rootID, ""); err != nil {
				t.Fatal(err)
			}
			if _, ok, _ := p.entryAt(ctx, "/d"); !ok {
				if _, err := p.Mkdir(ctx, rootID, "d"); err != nil {
					t.Fatal(err)
				}
			}
			dir = "/d"
		}
		upload(t, ctx, p, dir, name[strings.LastIndex(name, "/")+1:], []byte("content "+string(rune('0'+i))))
	}
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// Everything landed on a as primary, replicated to b.
	if len(a.Tree()) == 0 || len(c.Tree()) > 1 {
		t.Fatalf("a=%v c=%v", a.Tree(), c.Tree())
	}
	if err := p.SetMemberState("a", "draining"); err != nil {
		t.Fatal(err)
	}
	var empty bool
	for i := 0; i < 5 && !empty; i++ {
		var err error
		_, empty, err = p.DrainOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !empty {
		t.Fatalf("a not drained: %v", a.Tree())
	}
	for _, name := range []string{"/x.txt", "/d/y.txt", "/d/z.txt"} {
		if _, ok := a.Content(name); ok {
			t.Fatalf("a still holds %s", name)
		}
		if _, ok := b.Content(name); !ok {
			t.Fatalf("b lacks %s", name)
		}
		if _, ok := c.Content(name); !ok {
			t.Fatalf("c lacks %s: %v", name, c.Tree())
		}
		if got := readAll(t, p, name); !strings.HasPrefix(got, "content") {
			t.Fatalf("read %s = %q", name, got)
		}
	}
	// Writes never go to the draining member.
	upload(t, ctx, p, rootID, "late.txt", []byte("late"))
	if _, ok := a.Content("/late.txt"); ok {
		t.Fatal("a draining member received a write")
	}
}

func TestListReportsOutOfBandRenameAsDivergence(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/f.txt", []byte("same bytes"))
	b.Seed("/f.txt", []byte("same bytes"))
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()
	p.List(ctx, rootID, "")
	id, _ := b.IDOf("/f.txt")
	if _, err := b.Rename(ctx, id, "renamed-in-app.txt"); err != nil {
		t.Fatal(err)
	}
	entries, _, _ := p.List(ctx, rootID, "")
	if got := strings.Join(names(entries), ","); got != "f.txt,renamed-in-app.txt" {
		t.Fatalf("listing = %s", got)
	}
	divs, _ := p.Divergences(ctx, 10)
	if len(divs) != 1 || divs[0].Kind != "renamed-out-of-band" || divs[0].Path != "/f.txt" || divs[0].Member != "b" {
		t.Fatalf("divergences = %+v", divs)
	}
	if err := p.ClearDivergence(ctx, "/f.txt", "b", "renamed-out-of-band"); err != nil {
		t.Fatal(err)
	}
	if divs, _ = p.Divergences(ctx, 10); len(divs) != 0 {
		t.Fatalf("not cleared: %+v", divs)
	}
	_ = provider.KindFile
}

// TestMoveRefusedByOneMemberDoesNotDuplicateTheFile: a member can be
// perfectly reachable and still refuse a move — something already occupies
// the name over there, or the name breaks a rule that member enforces. The
// pool used to note the refusal and move on, which left the member's copy
// where it was with nothing scheduled to fix it: the next listing of the
// old directory found a file no index row claimed and published it a second
// time, under a second id. The refusal is now an operation the member owes,
// and until it is settled the copy the member kept is not a new file.
func TestMoveRefusedByOneMemberDoesNotDuplicateTheFile(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	p, err := New(Options{Name: "home", StateDir: t.TempDir(), Settings: config.Pool{Replicas: 2, MinReplicas: 1, OpTTL: 7 * 24 * time.Hour},
		Members: []Member{{Name: "a", Provider: a}, {Name: "b", Provider: b}}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	oldDir, err := p.Mkdir(ctx, rootID, "old")
	if err != nil {
		t.Fatal(err)
	}
	newDir, err := p.Mkdir(ctx, rootID, "new")
	if err != nil {
		t.Fatal(err)
	}
	moved := upload(t, ctx, p, oldDir.ID, "x.txt", []byte("ours"))
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// Someone else's file already sits at the destination name on b, so b
	// answers the move with ErrExists while a applies it.
	b.Seed("/new/x.txt", []byte("theirs"))

	if _, err := p.Move(ctx, moved.ID, newDir.ID); err != nil {
		t.Fatalf("move: %v", err)
	}
	entries, _, err := p.List(ctx, oldDir.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(entries), ","); got != "" {
		t.Fatalf("the old directory still lists %q; the copy b kept was published as a new file", got)
	}
	if pendingCount(t, p, "b") != 1 {
		t.Fatal("the member that refused the move owes the pool nothing")
	}

	// It keeps refusing. The op is parked, and the row that claimed b holds
	// the moved entry goes with it: the entry is short a replica, which is
	// the truth, and b is flagged for a scrub.
	for i := 0; i < opMaxAttempts; i++ {
		now = now.Add(time.Hour)
		if _, err := p.ReplayOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if pendingCount(t, p, "b") != 0 {
		t.Fatal("a refused op retries forever")
	}
	if n := liveCount(t, p, "/new/x.txt"); n != 1 {
		t.Fatalf("live replicas of the moved entry = %d, want only the member that applied it", n)
	}
	if !p.byName["b"].needsScrub {
		t.Fatal("the member that refused is not flagged for a scrub")
	}
	// The bytes are never touched: what b kept is still on b, for a person
	// to deal with, with the divergence beside it.
	if _, ok := b.Content("/old/x.txt"); !ok {
		t.Fatal("the pool deleted the copy the member refused to move")
	}
	divs, err := p.Divergences(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(divs) == 0 {
		t.Fatal("nothing tells a person why b still holds the old copy")
	}
}

// TestMoveWithAMemberDownDoesNotDuplicateTheFile: the same shape, with the
// member unreachable rather than refusing. It comes back before the op log
// is replayed, so a listing of the old directory sees its leftover file
// first — and must not mistake it for a new one.
func TestMoveWithAMemberDownDoesNotDuplicateTheFile(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	p, err := New(Options{Name: "home", StateDir: t.TempDir(), Settings: config.Pool{Replicas: 2, MinReplicas: 1, OpTTL: 7 * 24 * time.Hour},
		Members: []Member{{Name: "a", Provider: a}, {Name: "b", Provider: b}}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	oldDir, _ := p.Mkdir(ctx, rootID, "old")
	newDir, _ := p.Mkdir(ctx, rootID, "new")
	moved := upload(t, ctx, p, oldDir.ID, "x.txt", []byte("ours"))
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	b.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	p.ProbeOnce(ctx)
	if _, err := p.Move(ctx, moved.ID, newDir.ID); err != nil {
		t.Fatal(err)
	}
	b.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })
	p.ProbeOnce(ctx)

	entries, _, _ := p.List(ctx, oldDir.ID, "")
	if got := strings.Join(names(entries), ","); got != "" {
		t.Fatalf("the old directory lists %q before the op log was replayed", got)
	}
	entries, _, _ = p.List(ctx, newDir.ID, "")
	if got := strings.Join(names(entries), ","); got != "x.txt" {
		t.Fatalf("the new directory lists %q", got)
	}
	// The replay puts b's copy where the entry now lives.
	now = now.Add(time.Hour)
	if _, err := p.ReplayOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Content("/new/x.txt"); !ok {
		t.Fatalf("the replay did not move b's copy: %v", b.Tree())
	}
	if n := liveCount(t, p, "/new/x.txt"); n != 2 {
		t.Fatalf("live replicas after the replay = %d", n)
	}
}
