package pool

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

var windowsRules = provider.Naming{CaseInsensitive: true, MaxNameBytes: 255, ForbiddenRunes: provider.WindowsForbiddenRunes, ReservedNames: provider.WindowsReservedNames, NoTrailingDotSpace: true}

func TestPlacementSkipsMemberThatCannotHoldTheName(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.SetNaming(windowsRules) // a comes first, but cannot spell the name
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1}, a, b)
	ctx := context.Background()
	e := upload(t, ctx, p, rootID, "what?.txt", []byte("question"))
	if _, ok := b.Content("/what?.txt"); !ok {
		t.Fatalf("b lacks the file: %v", b.Tree())
	}
	if a.Calls("BeginUpload") != 0 {
		t.Fatal("a member that cannot hold the name was asked to")
	}
	if got := readAll(t, p, e.ID); got != "question" {
		t.Fatalf("read = %q", got)
	}
	// Repair cannot make a second copy anywhere; it parks the file with
	// a reason instead of retrying forever.
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var reason string
	p.db.QueryRow(`SELECT reason FROM repair_queue WHERE path = '/what?.txt'`).Scan(&reason)
	if reason != "no-eligible-member" {
		t.Fatalf("queue reason = %q", reason)
	}
	// A name nobody can hold is refused with the reason.
	b.SetNaming(windowsRules)
	_, err := p.BeginUpload(ctx, rootID, "CON", 1, nil)
	if !errors.Is(err, provider.ErrBadName) {
		t.Fatalf("unholdable name = %v", err)
	}
}

func TestNamingDenialIsLearnedFromRejection(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.SetNaming(windowsRules)
	a.SetNamingAdvertised(false) // refuses, but does not say so up front
	dir := t.TempDir()
	p := newTestPoolWith(t, dir, config.Pool{Replicas: 1, MinReplicas: 1}, a, b)
	ctx := context.Background()
	upload(t, ctx, p, rootID, "a:b.txt", []byte("1"))
	if a.Calls("BeginUpload") != 1 {
		t.Fatalf("a should have been tried once, was %d", a.Calls("BeginUpload"))
	}
	if _, ok := b.Content("/a:b.txt"); !ok {
		t.Fatal("the refused upload did not land on b")
	}
	// The next name of the same kind is not even offered to a.
	upload(t, ctx, p, rootID, "c:d.txt", []byte("2"))
	if a.Calls("BeginUpload") != 1 {
		t.Fatalf("a was asked again after refusing a ':' name (%d)", a.Calls("BeginUpload"))
	}
	// And a plain name still goes to a first.
	upload(t, ctx, p, rootID, "plain.txt", []byte("3"))
	if _, ok := a.Content("/plain.txt"); !ok {
		t.Fatalf("a: %v", a.Tree())
	}
	// What was learnt survives a restart.
	p.Close()
	p2 := newTestPoolWith(t, dir, config.Pool{Replicas: 1, MinReplicas: 1}, a, b)
	upload(t, ctx, p2, rootID, "e:f.txt", []byte("4"))
	if a.Calls("BeginUpload") != 2 {
		t.Fatalf("learnt refusal forgotten after restart (%d)", a.Calls("BeginUpload"))
	}
}

func TestCaseInsensitiveMemberDoesNotHostBothCases(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	b.SetNaming(provider.Naming{CaseInsensitive: true})
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1}, a, b)
	ctx := context.Background()
	upload(t, ctx, p, rootID, "Readme.txt", []byte("upper"))
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Content("/Readme.txt"); !ok {
		t.Fatalf("b: %v", b.Tree())
	}
	upload(t, ctx, p, rootID, "readme.txt", []byte("lower"))
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(b.Tree(), ","); got != "/Readme.txt" {
		t.Fatalf("a case-folding member holds both cases: %s", got)
	}
	if got := strings.Join(a.Tree(), ","); got != "/Readme.txt,/readme.txt" {
		t.Fatalf("a = %s", got)
	}
	entries, _, _ := p.List(ctx, rootID, "")
	if got := strings.Join(names(entries), ","); got != "Readme.txt,readme.txt" {
		t.Fatalf("listing = %s", got)
	}
}

func TestPlacementPrefersFreeSpace(t *testing.T) {
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	a.SetQuota(100, 95)
	b.SetQuota(100, 10)
	// c reports nothing: known space wins over unknown.
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 1, MinReplicas: 1}, a, b, c)
	ctx := context.Background()
	upload(t, ctx, p, rootID, "f.txt", []byte("goes where there is room"))
	if _, ok := b.Content("/f.txt"); !ok {
		t.Fatalf("a=%v b=%v c=%v", a.Tree(), b.Tree(), c.Tree())
	}
	// A configured capacity stands in for a backend that cannot report.
	d := fakeprovider.New("d")
	p2, err := New(Options{Name: "p2", StateDir: t.TempDir(), Settings: config.Pool{Replicas: 1, MinReplicas: 1},
		Members: []Member{{Name: "a", Provider: a}, {Name: "d", Provider: d, Capacity: 1 << 30}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()
	upload(t, ctx, p2, rootID, "g.txt", []byte("capacity"))
	if _, ok := d.Content("/g.txt"); !ok {
		t.Fatalf("d: %v", d.Tree())
	}
}

func TestPlacementNeverPutsTwoReplicasOnOneMember(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 3, MinReplicas: 1}, a, b)
	ctx := context.Background()
	upload(t, ctx, p, rootID, "f.txt", []byte("x"))
	for i := 0; i < 3; i++ {
		if _, err := p.RepairOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []*fakeprovider.Fake{a, b} {
		if len(f.Tree()) != 1 {
			t.Fatalf("%s holds %v", f.Name(), f.Tree())
		}
	}
	var rows int
	p.db.QueryRow(`SELECT COUNT(*) FROM replicas WHERE path = '/f.txt'`).Scan(&rows)
	if rows != 2 {
		t.Fatalf("replica rows = %d", rows)
	}
}

func TestPoolQuotaAggregatesMembers(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.SetQuota(100, 40)
	b.SetQuota(100, 20)
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1}, a, b)
	q, err := p.Quota(context.Background())
	if err != nil || q.Total != 100 || q.Used != 30 {
		t.Fatalf("pool quota = %+v, %v", q, err)
	}
	c := fakeprovider.New("c")
	p2 := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1}, c)
	if _, err := p2.Quota(context.Background()); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("a pool of members without quota = %v", err)
	}
}
