package pool

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

func newTestPool(t *testing.T, dir string, fakes ...*fakeprovider.Fake) *Pool {
	t.Helper()
	opt := Options{Name: "home", StateDir: dir, Settings: config.Pool{Replicas: 2, MinReplicas: 1}}
	for _, f := range fakes {
		opt.Members = append(opt.Members, Member{Name: f.Name(), Provider: f, Adopt: true})
	}
	p, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func names(entries []provider.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

func find(entries []provider.Entry, name string) (provider.Entry, bool) {
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	return provider.Entry{}, false
}

func readAll(t *testing.T, p *Pool, id string) string {
	t.Helper()
	rc, err := p.ReadRange(context.Background(), id, "", 0, 1<<20)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestListMergesMembersByName(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/only-a.txt", []byte("A"))
	b.Seed("/only-b.txt", []byte("B"))
	a.Seed("/both.txt", []byte("same"))
	b.Seed("/both.txt", []byte("same"))
	a.Seed("/docs/readme.md", []byte("docs on a"))
	b.Seed("/pics/cat.jpg", []byte("cat"))
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()

	entries, _, err := p.List(ctx, rootID, "")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names(entries), ",")
	if got != "both.txt,docs,only-a.txt,only-b.txt,pics" {
		t.Fatalf("merged root = %s", got)
	}
	both, _ := find(entries, "both.txt")
	if both.Kind != provider.KindFile || both.Size != 4 || both.Version == "" || !strings.HasPrefix(both.Version, "h1:") {
		t.Fatalf("both.txt = %+v", both)
	}
	reps, err := p.replicasOf(ctx, "/both.txt", both.Version)
	if err != nil || len(reps) != 2 {
		t.Fatalf("both.txt replicas = %d, %v", len(reps), err)
	}
	// A directory only one member holds is still a directory of the pool,
	// and listing it costs nothing on the member that does not have it.
	docs, _ := find(entries, "docs")
	inner, _, err := p.List(ctx, docs.ID, "")
	if err != nil || strings.Join(names(inner), ",") != "readme.md" {
		t.Fatalf("/docs = %v, %v", names(inner), err)
	}
	if got := readAll(t, p, inner[0].ID); got != "docs on a" {
		t.Fatalf("readme = %q", got)
	}
	if got := readAll(t, p, both.ID); got != "same" {
		t.Fatalf("both = %q", got)
	}
}

func TestListNewestMTimeWinsAndLoserSurfacesAsConflictCopy(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/notes.md", []byte("old on a"))
	b.Seed("/notes.md", []byte("new on b"))
	old := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	a.SetMTime("/notes.md", old)
	b.SetMTime("/notes.md", old.Add(time.Hour))
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()

	entries, _, err := p.List(ctx, rootID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(entries), ","); got != "notes (conflict 2026-09-01 a).md,notes.md" {
		t.Fatalf("root = %s", got)
	}
	win, _ := find(entries, "notes.md")
	if got := readAll(t, p, win.ID); got != "new on b" {
		t.Fatalf("winner content = %q", got)
	}
	lose, _ := find(entries, "notes (conflict 2026-09-01 a).md")
	if got := readAll(t, p, lose.ID); got != "old on a" {
		t.Fatalf("conflict copy content = %q", got)
	}
	if win.Version == lose.Version {
		t.Fatal("two contents share a version")
	}
	// The conflict name is stable across listings: same date, same member.
	again, _, _ := p.List(ctx, rootID, "")
	if strings.Join(names(again), ",") != strings.Join(names(entries), ",") {
		t.Fatalf("second listing differs: %v", names(again))
	}
	lose2, _ := find(again, lose.Name)
	if lose2.ID != lose.ID || lose2.Version != lose.Version {
		t.Fatalf("conflict copy identity drifted: %+v vs %+v", lose, lose2)
	}
}

func TestListIsSideEffectFree(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/x.txt", []byte("1"))
	b.Seed("/x.txt", []byte("22"))
	a.SetMTime("/x.txt", time.Now().Add(-time.Hour))
	p := newTestPool(t, t.TempDir(), a, b)
	for i := 0; i < 3; i++ {
		if _, _, err := p.List(context.Background(), rootID, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []*fakeprovider.Fake{a, b} {
		for _, op := range []string{"Rename", "Move", "Delete", "Mkdir", "BeginUpload", "UploadPart", "CompleteUpload"} {
			if n := f.Calls(op); n != 0 {
				t.Fatalf("listing performed %s on %s %d times", op, f.Name(), n)
			}
		}
	}
	if a.Calls("List") != 3 || b.Calls("List") != 3 {
		t.Fatalf("list calls: a=%d b=%d", a.Calls("List"), b.Calls("List"))
	}
}

func TestIDsAreStableAcrossRestart(t *testing.T) {
	a := fakeprovider.New("a")
	a.Seed("/docs/readme.md", []byte("hi"))
	dir := t.TempDir()
	p := newTestPool(t, dir, a)
	ctx := context.Background()
	entries, _, _ := p.List(ctx, rootID, "")
	docs, _ := find(entries, "docs")
	inner, _, _ := p.List(ctx, docs.ID, "")
	readme := inner[0]
	p.Close()

	p2 := newTestPool(t, dir, a)
	entries2, _, _ := p2.List(ctx, rootID, "")
	docs2, _ := find(entries2, "docs")
	inner2, _, _ := p2.List(ctx, docs2.ID, "")
	if docs2.ID != docs.ID || inner2[0].ID != readme.ID {
		t.Fatalf("ids changed across restart: %s/%s vs %s/%s", docs.ID, readme.ID, docs2.ID, inner2[0].ID)
	}
	if !strings.HasPrefix(readme.ID, idPrefix) || strings.HasPrefix(readme.ID, "cloudfs-local:") {
		t.Fatalf("bad id shape %q", readme.ID)
	}
	// A file recreated at the same path keeps its id and gets a new version.
	a.Seed("/docs/readme.md", []byte("changed"))
	inner3, _, _ := p2.List(ctx, docs2.ID, "")
	if inner3[0].ID != readme.ID || inner3[0].Version == readme.Version {
		t.Fatalf("rewrite: %+v vs %+v", readme, inner3[0])
	}
}

func TestPathIDAcceptedAsID(t *testing.T) {
	a := fakeprovider.New("a")
	a.Seed("/docs/readme.md", []byte("hi"))
	p := newTestPool(t, t.TempDir(), a)
	ctx := context.Background()
	e, err := p.Stat(ctx, "/docs/readme.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.ID, idPrefix) || e.Name != "readme.md" || e.Size != 2 {
		t.Fatalf("stat by path = %+v", e)
	}
	if got := readAll(t, p, "/docs/readme.md"); got != "hi" {
		t.Fatalf("read by path = %q", got)
	}
	if _, err := p.Stat(ctx, "/docs/\x00x"); !errors.Is(err, ErrBadPath) {
		t.Fatalf("NUL accepted: %v", err)
	}
	if _, err := p.Stat(ctx, "/nope"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	dirs, _, err := p.List(ctx, "/docs", "")
	if err != nil || len(dirs) != 1 || dirs[0].ID != e.ID {
		t.Fatalf("list by path = %v, %v", dirs, err)
	}
}

func TestCTokenIgnoresReplicaMTime(t *testing.T) {
	// With hashes: a second replica that appears later, with a newer mtime,
	// is the same content and must not change the version.
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/f.bin", []byte("payload"))
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()
	first, _, _ := p.List(ctx, rootID, "")
	f1, _ := find(first, "f.bin")
	b.Seed("/f.bin", []byte("payload"))
	b.SetMTime("/f.bin", time.Now().Add(time.Hour))
	second, _, _ := p.List(ctx, rootID, "")
	if len(second) != 1 {
		t.Fatalf("a replica surfaced as a separate entry: %v", names(second))
	}
	f2, _ := find(second, "f.bin")
	if f2.Version != f1.Version || f2.ID != f1.ID {
		t.Fatalf("version flipped when a replica appeared: %+v vs %+v", f1, f2)
	}
	if reps, _ := p.replicasOf(ctx, "/f.bin", f2.Version); len(reps) != 2 {
		t.Fatalf("replicas = %d", len(reps))
	}

	// Without hashes the token is a pure function of size and mtime, so two
	// indexes over the same members agree without talking to each other.
	c, d := fakeprovider.New("c"), fakeprovider.New("d")
	c.SetReportHashes(false)
	d.SetReportHashes(false)
	when := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	c.Seed("/g.bin", []byte("xyz"))
	d.Seed("/g.bin", []byte("xyz"))
	c.SetMTime("/g.bin", when)
	d.SetMTime("/g.bin", when.Add(time.Second)) // within tolerance: one content
	p1 := newTestPool(t, t.TempDir(), c, d)
	p2 := newTestPool(t, t.TempDir(), c, d)
	e1, _, _ := p1.List(ctx, rootID, "")
	e2, _, _ := p2.List(ctx, rootID, "")
	if len(e1) != 1 || len(e2) != 1 || e1[0].Version != e2[0].Version || e1[0].Version != timeToken(3, when.Add(time.Second)) {
		t.Fatalf("hashless tokens: %v / %v", e1, e2)
	}
}

func TestReadFailsOverToAnotherReplica(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/f.txt", []byte("both"))
	b.Seed("/f.txt", []byte("both"))
	a.Seed("/only-a.txt", []byte("a"))
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()
	entries, _, _ := p.List(ctx, rootID, "")
	f, _ := find(entries, "f.txt")
	onlyA, _ := find(entries, "only-a.txt")

	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	if got := readAll(t, p, f.ID); got != "both" {
		t.Fatalf("failover read = %q", got)
	}
	if b.Calls("ReadRange") != 1 {
		t.Fatalf("b served %d reads, want 1", b.Calls("ReadRange"))
	}
	_, err := p.ReadRange(ctx, onlyA.ID, "", 0, 10)
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("read with every replica down = %v, want ErrUnavailable", err)
	}
	if _, err := p.DownloadURL(ctx, f.ID); err != nil {
		t.Fatalf("download url failover: %v", err)
	}
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })
	if got := readAll(t, p, onlyA.ID); got != "a" {
		t.Fatalf("after recovery = %q", got)
	}
}

func TestListSurvivesOneMemberDown(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/on-a.txt", []byte("a"))
	a.Seed("/adir/x.txt", []byte("x"))
	b.Seed("/on-b.txt", []byte("b"))
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()
	warm, _, err := p.List(ctx, rootID, "")
	if err != nil {
		t.Fatal(err)
	}
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	cold, _, err := p.List(ctx, rootID, "")
	if err != nil {
		t.Fatalf("listing with a member down: %v", err)
	}
	if strings.Join(names(cold), ",") != strings.Join(names(warm), ",") {
		t.Fatalf("listing lost entries while a is down: %v vs %v", names(cold), names(warm))
	}
	onA, _ := find(cold, "on-a.txt")
	if _, err := p.ReadRange(ctx, onA.ID, "", 0, 1); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("read of a file only on the down member = %v", err)
	}
	onB, _ := find(cold, "on-b.txt")
	if got := readAll(t, p, onB.ID); got != "b" {
		t.Fatalf("other member's file = %q", got)
	}
	// Both down and the directory known: the snapshot still answers; an
	// unknown directory is unavailable rather than empty.
	b.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	snap, _, err := p.List(ctx, rootID, "")
	if err != nil || len(snap) != len(warm) {
		t.Fatalf("snapshot listing = %v, %v", names(snap), err)
	}
	// A directory the root enumeration did not show is known not to exist,
	// down or not; one that exists but was never enumerated cannot be
	// answered from the index and is unavailable, not empty.
	if _, _, err := p.List(ctx, "/never-listed", ""); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("unknown dir with everything down = %v, want ErrNotFound", err)
	}
	if _, _, err := p.List(ctx, "/adir", ""); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("never-enumerated dir with everything down = %v, want ErrUnavailable", err)
	}
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })
	if inner, _, err := p.List(ctx, "/adir", ""); err != nil || len(inner) != 1 {
		t.Fatalf("/adir after a returns = %v, %v", names(inner), err)
	}
}

func TestMemberRootIsRespected(t *testing.T) {
	a := fakeprovider.New("a")
	a.Seed("/pool/inside.txt", []byte("in"))
	a.Seed("/outside.txt", []byte("out"))
	p, err := New(Options{Name: "home", StateDir: filepath.Join(t.TempDir(), "s"), Members: []Member{{Name: "a", Provider: a, Root: "/pool"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	entries, _, err := p.List(context.Background(), rootID, "")
	if err != nil || strings.Join(names(entries), ",") != "inside.txt" {
		t.Fatalf("rooted listing = %v, %v", names(entries), err)
	}
}

func TestCapabilitiesAggregateMembers(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	p := newTestPool(t, t.TempDir(), a, b)
	c := p.Capabilities()
	if !c.StreamList || !c.ServerMove || !c.ServerRename || c.Delta {
		t.Fatalf("caps = %+v", c)
	}
	if c.QPS.Meta != 20 || c.QPS.Upload != 10 {
		t.Fatalf("qps should sum members: %+v", c.QPS)
	}
	if len(c.RapidUpload) == 0 {
		t.Fatal("rapid upload hashes should be requested so the primary member can dedupe")
	}
}
