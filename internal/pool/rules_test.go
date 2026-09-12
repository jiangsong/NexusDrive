package pool

import (
	"context"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/test/fakeprovider"
)

// TestRuleForTakesTheLongestWholeComponentPrefix: rules are chosen by
// longest prefix, and a prefix only covers whole path components — /photos
// must not claim /photoshop.
func TestRuleForTakesTheLongestWholeComponentPrefix(t *testing.T) {
	a := fakeprovider.New("a")
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1, Rules: []config.PoolRule{
		{Prefix: "/photos", Replicas: 3},
		{Prefix: "/photos/raw", Replicas: 4},
		{Prefix: "/", Replicas: 5},
	}}, a)
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/photos", "/photos"},
		{"/photos/2024/a.jpg", "/photos"},
		{"/photos/raw", "/photos/raw"},
		{"/photos/raw/a.dng", "/photos/raw"},
		{"/photoshop/a.psd", "/"},
		{"/other", "/"},
	} {
		r := p.ruleFor(tc.path)
		if r == nil || r.Prefix != tc.want {
			t.Fatalf("ruleFor(%q) = %v, want prefix %q", tc.path, r, tc.want)
		}
	}
	// With no rule covering it, the pool's own settings are the rule.
	p2 := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1, Rules: []config.PoolRule{{Prefix: "/photos", Replicas: 3}}}, a)
	if r := p2.ruleFor("/docs/a.txt"); r != nil {
		t.Fatalf("ruleFor outside every prefix = %v, want nil", r)
	}
	if got := p2.wantReplicas("/docs/a.txt"); got != 2 {
		t.Fatalf("wantReplicas outside every rule = %d, want the pool's 2", got)
	}
	if got := p2.wantReplicas("/photos/a.jpg"); got != 3 {
		t.Fatalf("wantReplicas under /photos = %d, want 3", got)
	}
}

// TestTargetForCapsEachPathByTheMembersInService: a rule asking for more
// replicas than the pool has members in service reports the cap rather
// than pretending the target is met.
func TestTargetForCapsEachPathByTheMembersInService(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 1, MinReplicas: 1, Rules: []config.PoolRule{
		{Prefix: "/photos", Replicas: 2},
		{Prefix: "/video", Replicas: 5},
	}}, a, b)
	for _, tc := range []struct {
		path       string
		want       int
		wantCapped bool
	}{
		{"/docs/a.txt", 1, false},
		{"/photos/a.jpg", 2, false},
		{"/video/a.mkv", 2, true},
	} {
		got, capped := p.targetFor(tc.path)
		if got != tc.want || capped != tc.wantCapped {
			t.Fatalf("targetFor(%q) = %d, capped %v; want %d, %v", tc.path, got, capped, tc.want, tc.wantCapped)
		}
	}
	// The pool-wide number ignores rules: it is what a status page means
	// by "this pool keeps N copies".
	if got, capped := p.replicaTarget(); got != 1 || capped {
		t.Fatalf("replicaTarget = %d, capped %v; want the pool's own 1", got, capped)
	}
	if !p.anyMultiReplica() {
		t.Fatal("a pool whose rules ask for more than one replica is a replicated pool")
	}
	p2 := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 1, MinReplicas: 1}, a, b)
	if p2.anyMultiReplica() {
		t.Fatal("a single-replica pool with no rules is not a replicated pool")
	}
}

// TestRepairFillsToThePrefixRuleNotThePoolDefault: the whole point of a
// rule is that /photos gets more copies than the pool default while the
// rest of the namespace does not pay for them.
func TestRepairFillsToThePrefixRuleNotThePoolDefault(t *testing.T) {
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 1, MinReplicas: 1, Rules: []config.PoolRule{
		{Prefix: "/photos", Replicas: 3},
	}}, a, b, c)
	ctx := context.Background()
	photos, _ := p.Mkdir(ctx, rootID, "photos")
	docs, _ := p.Mkdir(ctx, rootID, "docs")
	upload(t, ctx, p, photos.ID, "a.jpg", []byte("pixels"))
	upload(t, ctx, p, docs.ID, "a.txt", []byte("words"))

	queued, err := p.ScanOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("queued %d files, want only the one under /photos", queued)
	}
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := liveCount(t, p, "/photos/a.jpg"); got != 3 {
		t.Fatalf("/photos/a.jpg has %d replicas, want the rule's 3", got)
	}
	if got := liveCount(t, p, "/docs/a.txt"); got != 1 {
		t.Fatalf("/docs/a.txt has %d replicas, want the pool's 1", got)
	}
	if n, _, err := p.underReplicated(ctx); err != nil || n != 0 {
		t.Fatalf("underReplicated = %d, %v; want 0 once every path meets its own rule", n, err)
	}
}

// TestTrimUsesThePrefixRuleAsTheSurplusLine: a rule that asks for fewer
// copies than the pool default makes the extra copies surplus, and one
// that asks for more protects copies the default would have trimmed.
func TestTrimUsesThePrefixRuleAsTheSurplusLine(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	for _, f := range []*fakeprovider.Fake{a, b, c} {
		f.Seed("/tmp/scratch.bin", []byte("S"))
		f.Seed("/keep/master.bin", []byte("K"))
	}
	p, err := New(Options{Name: "home", StateDir: t.TempDir(),
		Settings: config.Pool{Replicas: 2, MinReplicas: 1, TrimGrace: time.Hour, Rules: []config.PoolRule{
			{Prefix: "/tmp", Replicas: 1},
			{Prefix: "/keep", Replicas: 3},
		}},
		Members: []Member{{Name: "a", Provider: a, Adopt: true}, {Name: "b", Provider: b, Adopt: true}, {Name: "c", Provider: c, Adopt: true}},
		Now:     func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	top, _, err := p.List(ctx, rootID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tmp", "keep"} {
		d, ok := find(top, name)
		if !ok {
			t.Fatalf("%s was not adopted: %v", name, names(top))
		}
		if _, _, err := p.List(ctx, d.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(2 * time.Hour)
	if _, err := p.TrimOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := liveCount(t, p, "/tmp/scratch.bin"); got != 1 {
		t.Fatalf("/tmp/scratch.bin has %d replicas, want the rule's 1", got)
	}
	if got := liveCount(t, p, "/keep/master.bin"); got != 3 {
		t.Fatalf("/keep/master.bin has %d replicas, want the rule's 3 kept", got)
	}
}

// memberNames is the candidate order as names, for the placement tests.
func memberNames(ms []*member) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.name
	}
	return out
}

func newRulePool(t *testing.T, settings config.Pool, members ...Member) *Pool {
	t.Helper()
	p, err := New(Options{Name: "home", StateDir: t.TempDir(), Settings: settings, Members: members})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// TestCandidatesRankByRequirePreferDomainAvoid: the rule's verdicts are
// the ordering, and only require excludes. A pool that dropped avoided
// members outright would refuse to place a file the operator only asked
// to keep off those drives when possible.
func TestCandidatesRankByRequirePreferDomainAvoid(t *testing.T) {
	ctx := context.Background()
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	members := []Member{
		{Name: "a", Provider: a, Adopt: true, Classes: []string{"slow", "cheap"}, Domain: "acct-1"},
		{Name: "b", Provider: b, Adopt: true, Classes: []string{"fast"}, Domain: "acct-1"},
		{Name: "c", Provider: c, Adopt: true, Classes: []string{"fast", "cheap"}, Domain: "acct-2"},
	}

	// prefer promotes, without excluding anyone.
	p := newRulePool(t, config.Pool{Replicas: 3, MinReplicas: 1, Rules: []config.PoolRule{
		{Prefix: "/video", Prefer: []string{"fast"}},
	}}, members...)
	got := memberNames(p.candidates(ctx, "/video/a.mkv"))
	if len(got) != 3 || got[0] == "a" {
		t.Fatalf("prefer fast = %v, want the fast members first and nobody dropped", got)
	}

	// avoid demotes, and also without excluding anyone.
	p = newRulePool(t, config.Pool{Replicas: 3, MinReplicas: 1, Rules: []config.PoolRule{
		{Prefix: "/video", Avoid: []string{"cheap"}},
	}}, members...)
	got = memberNames(p.candidates(ctx, "/video/a.mkv"))
	if len(got) != 3 || got[0] != "b" {
		t.Fatalf("avoid cheap = %v, want b first and the avoided members still available", got)
	}

	// require is the one hard rule.
	p = newRulePool(t, config.Pool{Replicas: 3, MinReplicas: 1, Rules: []config.PoolRule{
		{Prefix: "/video", Require: []string{"fast", "cheap"}},
	}}, members...)
	got = memberNames(p.candidates(ctx, "/video/a.mkv"))
	if len(got) != 1 || got[0] != "c" {
		t.Fatalf("require fast+cheap = %v, want only c", got)
	}

	// A path no rule covers ranks as it always did.
	if got := memberNames(p.candidates(ctx, "/docs/a.txt")); len(got) != 3 {
		t.Fatalf("outside every rule = %v, want every member", got)
	}
}

// TestCandidatesSpreadAcrossFailureDomains: two members of one account
// share a ban and a quota, so the second replica of a file belongs in the
// other account even though that member is declared last.
func TestCandidatesSpreadAcrossFailureDomains(t *testing.T) {
	ctx := context.Background()
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	p := newRulePool(t, config.Pool{Replicas: 2, MinReplicas: 1},
		Member{Name: "a", Provider: a, Adopt: true, Domain: "acct-1"},
		Member{Name: "b", Provider: b, Adopt: true, Domain: "acct-1"},
		Member{Name: "c", Provider: c, Adopt: true, Domain: "acct-2"})

	docs, _ := p.Mkdir(ctx, rootID, "docs")
	upload(t, ctx, p, docs.ID, "a.txt", []byte("spread me"))
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Content("/docs/a.txt"); !ok {
		t.Fatalf("the second replica stayed in the first account (a %v, b %v, c %v)",
			has(a, "/docs/a.txt"), has(b, "/docs/a.txt"), has(c, "/docs/a.txt"))
	}
	if _, ok := b.Content("/docs/a.txt"); ok {
		t.Fatal("b shares an account with the first replica and should not have been chosen")
	}
	// Spreading is a preference, not a rule: with the other account gone,
	// the third copy still lands rather than being refused.
	got := memberNames(p.candidates(ctx, "/docs/a.txt"))
	if len(got) != 3 {
		t.Fatalf("candidates = %v, want every member still available", got)
	}
	if got[len(got)-1] != "b" {
		t.Fatalf("candidates = %v, want the duplicate-domain member last", got)
	}
}

func has(f *fakeprovider.Fake, pth string) bool {
	_, ok := f.Content(pth)
	return ok
}
