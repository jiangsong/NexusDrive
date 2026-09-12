package pool

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

// TestBeginUploadSkipsFullMembers: a drive with no room is not a failed
// drive. The write must land on the next candidate, and the full one must
// stay out of placement afterwards without being asked again per file.
func TestBeginUploadSkipsFullMembers(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.SetFaults(func(f *fakeprovider.Faults) { f.QuotaExceeded = true })
	p, err := New(Options{Name: "home", StateDir: t.TempDir(), Settings: config.Pool{Replicas: 1, MinReplicas: 1},
		Members: []Member{{Name: "a", Provider: a, Adopt: true}, {Name: "b", Provider: b, Adopt: true}},
		Now:     func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	docs, _ := p.Mkdir(ctx, rootID, "docs")
	upload(t, ctx, p, docs.ID, "one.txt", []byte("first"))
	if _, ok := b.Content("/docs/one.txt"); !ok {
		t.Fatal("the write did not fall through to the member with room")
	}
	if p.byName["a"].state() == provider.HealthOut {
		t.Fatal("a full member answered correctly and must not be marked unhealthy")
	}

	// Asking again per file is what the full backoff exists to avoid.
	before := a.Calls("BeginUpload")
	upload(t, ctx, p, docs.ID, "two.txt", []byte("second"))
	if got := a.Calls("BeginUpload") - before; got != 0 {
		t.Fatalf("the full member was asked %d more times within its backoff", got)
	}
	if got := memberNames(p.candidates(ctx, "/docs/three.txt")); got[len(got)-1] != "a" {
		t.Fatalf("candidates = %v, want the full member last", got)
	}
	if free := p.free(ctx, p.byName["a"]); free != 0 {
		t.Fatalf("a full member reports %d free bytes", free)
	}

	// The backoff ends, and a drive that got emptied is a candidate again.
	now = now.Add(quotaBackoff + time.Second)
	a.SetFaults(func(f *fakeprovider.Faults) { f.QuotaExceeded = false })
	if got := memberNames(p.candidates(ctx, "/docs/four.txt")); got[0] != "a" && got[0] != "b" {
		t.Fatalf("candidates after the backoff = %v", got)
	}
	if p.byName["a"].isFull(now) {
		t.Fatal("the member is still marked full after its backoff")
	}
}

// TestUploadPartOutOfSpaceAsksForARestart: a member that fills up
// mid-transfer cannot have its session resumed anywhere, so the pool says
// so explicitly — the uploader drops the parts and starts again, and
// placement then avoids the full member.
func TestUploadPartOutOfSpaceAsksForARestart(t *testing.T) {
	ctx := context.Background()
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	caps := a.Capabilities()
	caps.PartSize = 4
	a.SetCaps(caps)
	a.SetFaults(func(f *fakeprovider.Faults) { f.QuotaAfterBytes = 4 })
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 1, MinReplicas: 1}, a, b)

	docs, _ := p.Mkdir(ctx, rootID, "docs")
	sess, err := p.BeginUpload(ctx, docs.ID, "big.bin", 12, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := p.UploadPart(ctx, sess, 0, bytes.NewReader([]byte("aaaa")), 4); err != nil {
		t.Fatalf("first part: %v", err)
	}
	_, err = p.UploadPart(ctx, sess, 1, bytes.NewReader([]byte("bbbb")), 4)
	if !errors.Is(err, provider.ErrQuotaExceeded) || !errors.Is(err, provider.ErrRestartUpload) {
		t.Fatalf("part on a member that filled up = %v, want quota + restart", err)
	}
	if retry.Classify(err) != retry.ClassQuota {
		t.Fatalf("classified as %v, want quota", retry.Classify(err))
	}
	if !p.byName["a"].isFull(p.now()) {
		t.Fatal("the member that ran out was not marked full")
	}
	// The restart lands on the member with room.
	upload(t, ctx, p, docs.ID, "big.bin", []byte("cccccccccccc"))
	if _, ok := b.Content("/docs/big.bin"); !ok {
		t.Fatal("the restarted upload did not go to the member with room")
	}
}

// TestPoolWithNoRoomAnywhereReportsQuotaNotUnavailable: "every drive is
// full" and "every drive is unreachable" call for different answers —
// retrying fixes the second and never the first.
func TestPoolWithNoRoomAnywhereReportsQuotaNotUnavailable(t *testing.T) {
	ctx := context.Background()
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	for _, f := range []*fakeprovider.Fake{a, b} {
		f.SetFaults(func(fl *fakeprovider.Faults) { fl.QuotaExceeded = true })
	}
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 1, MinReplicas: 1}, a, b)
	docs, _ := p.Mkdir(ctx, rootID, "docs")
	_, err := p.BeginUpload(ctx, docs.ID, "x.txt", 3, nil)
	if !errors.Is(err, provider.ErrQuotaExceeded) {
		t.Fatalf("begin against a full pool = %v, want quota exceeded", err)
	}
	if errors.Is(err, provider.ErrUnavailable) {
		t.Fatal("a full pool is not an unreachable pool")
	}
}

// TestRepairSkipsFullMembersWithoutRecordingDivergence: a member with no
// room did not disagree about the file's content, so repair must move on
// rather than file a divergence for an operator to read.
func TestRepairSkipsFullMembersWithoutRecordingDivergence(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	p, err := New(Options{Name: "home", StateDir: t.TempDir(), Settings: config.Pool{Replicas: 2, MinReplicas: 1},
		Members: []Member{{Name: "a", Provider: a, Adopt: true}, {Name: "b", Provider: b, Adopt: true}, {Name: "c", Provider: c, Adopt: true}},
		Now:     func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	docs, _ := p.Mkdir(ctx, rootID, "docs")
	upload(t, ctx, p, docs.ID, "r.txt", []byte("replicate me"))

	// Whichever member did not take the write, make one of them full.
	var full *fakeprovider.Fake
	for _, f := range []*fakeprovider.Fake{a, b, c} {
		if _, ok := f.Content("/docs/r.txt"); !ok {
			full = f
			break
		}
	}
	full.SetFaults(func(fl *fakeprovider.Faults) { fl.QuotaExceeded = true })
	// A failed candidate costs the file one pass through the fill loop —
	// the second pass is where the full member is already demoted.
	for i := 0; i < 2 && liveCount(t, p, "/docs/r.txt") < 2; i++ {
		if _, err := p.RepairOnce(ctx); err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Minute)
	}
	if got := liveCount(t, p, "/docs/r.txt"); got != 2 {
		t.Fatalf("live = %d, want repair to have used a member with room", got)
	}
	if _, ok := full.Content("/docs/r.txt"); ok {
		t.Fatal("the full member took a copy")
	}
	divs, err := p.Divergences(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(divs) != 0 {
		t.Fatalf("a full drive was recorded as a divergence: %+v", divs)
	}
}

// TestMemberUsageTracksWhatThePoolPlaced: free space over a configured
// capacity comes from a running total, which must survive writes, repair
// and deletes without a full table scan.
func TestMemberUsageTracksWhatThePoolPlaced(t *testing.T) {
	ctx := context.Background()
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1}, a, b)
	docs, _ := p.Mkdir(ctx, rootID, "docs")
	e := upload(t, ctx, p, docs.ID, "r.txt", []byte("0123456789"))
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if got := p.memberBytes(ctx, name); got != 10 {
			t.Fatalf("member %s holds %d bytes, want 10", name, got)
		}
		if got := p.memberFiles(ctx, name); got != 1 {
			t.Fatalf("member %s holds %d files, want 1", name, got)
		}
	}
	if err := p.Delete(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if got := p.memberBytes(ctx, name); got != 0 {
			t.Fatalf("member %s still holds %d bytes after the delete", name, got)
		}
		if got := p.memberFiles(ctx, name); got != 0 {
			t.Fatalf("member %s still holds %d files after the delete", name, got)
		}
	}
}
