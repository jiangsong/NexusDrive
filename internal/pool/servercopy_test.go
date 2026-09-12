package pool

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/test/fakeprovider"
)

func serverCopyFake(name string) *fakeprovider.Fake {
	f := fakeprovider.New(name)
	caps := f.Capabilities()
	caps.ServerCopy = true
	f.SetCaps(caps)
	return f
}

// TestRepairCopiesServerSideWithinOneAccount: two members rooted in one
// account share the backend's id space, so it can duplicate a file
// without the bytes crossing this machine. Repair must use that.
func TestRepairCopiesServerSideWithinOneAccount(t *testing.T) {
	ctx := context.Background()
	acct := serverCopyFake("acct")
	acct.Seed("/one/keep", nil)
	acct.Seed("/two/keep", nil)
	p := newRulePool(t, config.Pool{Replicas: 2, MinReplicas: 1},
		Member{Name: "one", Provider: acct, Root: "/one", Adopt: true, Domain: "acct-1"},
		Member{Name: "two", Provider: acct, Root: "/two", Adopt: true, Domain: "acct-1"})
	docs, _ := p.Mkdir(ctx, rootID, "docs")
	upload(t, ctx, p, docs.ID, "r.txt", []byte("copy me server-side"))

	partsBefore := acct.Calls("UploadPart")
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := liveCount(t, p, "/docs/r.txt"); got != 2 {
		t.Fatalf("live = %d", got)
	}
	if copies := acct.Calls("Copy"); copies != 1 {
		t.Fatalf("server-side copies = %d, want 1", copies)
	}
	if sent := acct.Calls("UploadPart") - partsBefore; sent != 0 {
		t.Fatalf("repair sent %d parts through this machine", sent)
	}
	for _, root := range []string{"/one", "/two"} {
		if got, ok := acct.Content(root + "/docs/r.txt"); !ok || string(got) != "copy me server-side" {
			t.Fatalf("%s: %q %v", root, got, ok)
		}
	}
}

// TestRepairSendsBytesAcrossAccounts: a server-side copy only works
// inside one account, so a pool that spreads across two must still send
// the bytes for the second replica.
func TestRepairSendsBytesAcrossAccounts(t *testing.T) {
	ctx := context.Background()
	a, b := serverCopyFake("a"), serverCopyFake("b")
	p := newRulePool(t, config.Pool{Replicas: 2, MinReplicas: 1},
		Member{Name: "a", Provider: a, Adopt: true, Domain: "acct-1"},
		Member{Name: "b", Provider: b, Adopt: true, Domain: "acct-2"})
	docs, _ := p.Mkdir(ctx, rootID, "docs")
	upload(t, ctx, p, docs.ID, "r.txt", []byte("across accounts"))
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := liveCount(t, p, "/docs/r.txt"); got != 2 {
		t.Fatalf("live = %d", got)
	}
	if copies := a.Calls("Copy") + b.Calls("Copy"); copies != 0 {
		t.Fatalf("a cross-account copy was attempted server-side (%d calls)", copies)
	}
}

// TestUnconfirmedServerCopyScrubsBeforeTryingAgain: a copy that timed out
// may or may not have landed. Sending the file again would make a second
// one, so the next pass re-lists first and adopts whatever is there.
func TestUnconfirmedServerCopyScrubsBeforeTryingAgain(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	acct := serverCopyFake("acct")
	acct.Seed("/one/keep", nil)
	acct.Seed("/two/keep", nil)
	p, err := New(Options{Name: "home", StateDir: t.TempDir(), Settings: config.Pool{Replicas: 2, MinReplicas: 1},
		Members: []Member{
			{Name: "one", Provider: acct, Root: "/one", Adopt: true, Domain: "acct-1"},
			{Name: "two", Provider: acct, Root: "/two", Adopt: true, Domain: "acct-1"},
		},
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	docs, _ := p.Mkdir(ctx, rootID, "docs")
	e := upload(t, ctx, p, docs.ID, "r.txt", []byte("unconfirmed"))

	// The copy lands, and then the answer is lost: the next call the pool
	// makes against the backend fails.
	src, dst := "/one", "/two"
	if _, ok := acct.Content("/two/docs/r.txt"); ok {
		src, dst = "/two", "/one"
	}
	data, _ := acct.Content(src + "/docs/r.txt")
	acct.Seed(dst+"/docs/r.txt", data)
	acct.SetFaults(func(f *fakeprovider.Faults) { f.FailOp = map[string]int{"Copy": 1} })

	if made, _ := p.RepairOnce(ctx); made != 0 {
		t.Fatalf("made %d replicas out of an unconfirmed copy", made)
	}
	// The queue carries the reason that makes the next pass look before
	// it writes anything.
	var reason string
	if err := p.db.QueryRowContext(ctx, `SELECT reason FROM repair_queue WHERE path = ?`, "/docs/r.txt").Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, reasonCopyUnsure) {
		t.Fatalf("queue reason = %q, want it to ask for a re-list", reason)
	}

	now = now.Add(2 * time.Hour)
	copiesBefore := acct.Calls("Copy")
	if _, err := p.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := liveCount(t, p, "/docs/r.txt"); got != 2 {
		t.Fatalf("live = %d, want the landed copy adopted", got)
	}
	if copies := acct.Calls("Copy") - copiesBefore; copies != 0 {
		t.Fatalf("the file was copied %d more times after one already landed", copies)
	}
	if got := readAll(t, p, e.ID); got != "unconfirmed" {
		t.Fatalf("read = %q", got)
	}
}
