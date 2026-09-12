package chaos

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/pool"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

type poolRig struct {
	fs      *vfs.FS
	pool    *pool.Pool
	members map[string]*fakeprovider.Fake
	j       *journal.Journal
	up      *upload.Uploader
	clock   time.Time
}

func (r *poolRig) now() time.Time { return r.clock }

func newPoolRig(t *testing.T, memberNames ...string) *poolRig {
	return newPoolRigWith(t, config.Pool{Replicas: 2, MinReplicas: 1}, memberNames...)
}

func newPoolRigWith(t *testing.T, settings config.Pool, memberNames ...string) *poolRig {
	return newPoolRigTTL(t, settings, time.Second, memberNames...)
}

// newPoolRigSized is newPoolRigWith where every member has a capacity, so
// the pool can say how full each one is — what rebalance works from.
func newPoolRigSized(t *testing.T, settings config.Pool, capacity int64, memberNames ...string) *poolRig {
	t.Helper()
	r := newPoolRigTTL(t, settings, time.Second, memberNames...)
	r.pool.SetMemberCapacities(capacity)
	return r
}

func newPoolRigTTL(t *testing.T, settings config.Pool, ttl time.Duration, memberNames ...string) *poolRig {
	t.Helper()
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	r := &poolRig{members: map[string]*fakeprovider.Fake{}, clock: time.Now()}
	opt := pool.Options{Name: "home", StateDir: filepath.Join(dir, "pool"), Settings: settings, Now: r.now}
	for _, n := range memberNames {
		f := fakeprovider.New(n)
		r.members[n] = f
		opt.Members = append(opt.Members, pool.Member{Name: n, Provider: f, Adopt: true})
	}
	p, err := pool.New(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	r.pool = p
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca,
		AttrTTL: ttl, DefaultDirTTL: ttl, NegativeTTL: time.Second,
		Mounts: []vfs.Mount{{Prefix: "/", Remote: "home", RootID: p.RootID(), Provider: p, Mode: config.ModeWriteback, DirTTL: ttl}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	r.fs = fsys
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal"), Now: r.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	up, err := upload.New(upload.Options{
		Journal:     j,
		Providers:   func(remote string) (provider.Provider, bool) { return p, remote == "home" },
		Now:         r.now,
		MaxAttempts: 4,
		Policy:      retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond}, MaxAttempts: 2},
		Backoff:     retry.Backoff{Base: time.Millisecond, Max: 2 * time.Millisecond, Rand: func() float64 { return 1 }},
		Hooks:       fsys.UploadHooks(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fsys.SetWriteBackend(j, up)
	r.j, r.up = j, up
	return r
}

func listNames(t *testing.T, fsys *vfs.FS, p string) string {
	t.Helper()
	entries, err := fsys.ReadDirPath(context.Background(), p)
	if err != nil {
		t.Fatalf("readdir %s: %v", p, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return strings.Join(names, ",")
}

// TestPoolListSurvivesOneMemberDown: "成员失联" in the reliability matrix. A
// member that stops answering never empties a directory: what other members
// hold is served, what only it held is still listed (and reads of it fail
// with a distinct, retryable error rather than vanishing).
func TestPoolListSurvivesOneMemberDown(t *testing.T) {
	r := newPoolRig(t, "a", "b")
	a, b := r.members["a"], r.members["b"]
	a.Seed("/on-a.txt", []byte("a"))
	b.Seed("/on-b.txt", []byte("b"))
	a.Seed("/both.txt", []byte("both"))
	b.Seed("/both.txt", []byte("both"))
	ctx := context.Background()

	if got := listNames(t, r.fs, "/"); got != "both.txt,on-a.txt,on-b.txt" {
		t.Fatalf("warm listing = %s", got)
	}
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	if _, err := r.fs.DropCaches(ctx); err != nil {
		t.Fatal(err)
	}
	if got := listNames(t, r.fs, "/"); got != "both.txt,on-a.txt,on-b.txt" {
		t.Fatalf("listing with a down = %s", got)
	}
	if data, err := r.fs.ReadFileRange(ctx, "/both.txt", 0, 0); err != nil || string(data) != "both" {
		t.Fatalf("file replicated on b while a is down = %q, %v", data, err)
	}
	if data, err := r.fs.ReadFileRange(ctx, "/on-b.txt", 0, 0); err != nil || string(data) != "b" {
		t.Fatalf("file on b while a is down = %q, %v", data, err)
	}
	_, err := r.fs.ReadFileRange(ctx, "/on-a.txt", 0, 0)
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("file only on the down member = %v, want ErrUnavailable", err)
	}
	// The member comes back; nothing needed restarting.
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })
	if data, err := r.fs.ReadFileRange(ctx, "/on-a.txt", 0, 0); err != nil || string(data) != "a" {
		t.Fatalf("after recovery = %q, %v", data, err)
	}
}

// TestPoolWriteThroughTheVFSLandsOnAMember: the whole write path — staging,
// journal commit, upload — works against a pool exactly as against one
// drive, and the file ends up at its real path on a member.
func TestPoolWriteThroughTheVFSLandsOnAMember(t *testing.T) {
	r := newPoolRig(t, "a", "b")
	ctx := context.Background()
	if _, err := r.fs.Mkdir(ctx, meta.RootIno, "notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fs.WriteFile(ctx, "/notes/today.md", []byte("# today"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.up.DrainOnce(ctx, "home"); err != nil {
		t.Fatal(err)
	}
	st, _ := r.j.Stats(ctx)
	if st.Pending+st.Uploading+st.Dead != 0 {
		t.Fatalf("queue after drain: %+v", st)
	}
	attr, err := r.fs.StatPath(ctx, "/notes/today.md")
	if err != nil || attr.LocalOnly {
		t.Fatalf("node not adopted after upload: %+v, %v", attr, err)
	}
	landed := 0
	for name, f := range r.members {
		if got, ok := f.Content("/notes/today.md"); ok {
			landed++
			if string(got) != "# today" {
				t.Fatalf("%s holds %q", name, got)
			}
		}
	}
	if landed != 1 {
		t.Fatalf("the upload landed on %d members, want exactly the primary", landed)
	}
	data, err := r.fs.ReadFileRange(ctx, "/notes/today.md", 0, 0)
	if err != nil || string(data) != "# today" {
		t.Fatalf("read back = %q, %v", data, err)
	}
}

// TestPoolWriteWhenAllMembersDownDefersInsteadOfDeadLettering: "成员全部失联"
// is not a failed upload. The write stays committed in the journal, spends
// no attempts, keeps its bytes, and lands when a member returns.
func TestPoolWriteWhenAllMembersDownDefersInsteadOfDeadLettering(t *testing.T) {
	r := newPoolRig(t, "a", "b")
	ctx := context.Background()
	if _, err := r.fs.WriteFile(ctx, "/offline.txt", []byte("written while every drive is down"), false); err != nil {
		t.Fatal(err)
	}
	for _, f := range r.members {
		f.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	}
	for i := 0; i < 6; i++ {
		r.clock = r.clock.Add(time.Minute)
		if _, err := r.up.DrainOnce(ctx, "home"); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := r.j.Stats(ctx)
	if st.Dead != 0 || st.Pending+st.Uploading != 1 {
		t.Fatalf("queue with every member down: %+v", st)
	}
	ups, _ := r.j.All(ctx)
	if len(ups) != 1 || ups[0].Attempt != 0 {
		t.Fatalf("attempts were spent on an outage: %+v", ups)
	}
	if data, err := r.fs.ReadFileRange(ctx, "/offline.txt", 0, 0); err != nil || string(data) != "written while every drive is down" {
		t.Fatalf("local read during the outage = %q, %v", data, err)
	}
	r.members["b"].SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })
	r.clock = r.clock.Add(time.Minute) // past the deferral
	if _, err := r.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, ok := r.members["b"].Content("/offline.txt"); !ok || string(got) != "written while every drive is down" {
		t.Fatalf("after b returned: %q, %v", got, ok)
	}
}

// TestPoolRemoteChangedUnderUsProducesConflictCopy: the existing conflict
// rule holds through a pool. A member's copy edited after our writer opened
// the file is not overwritten; our data lands beside it.
func TestPoolRemoteChangedUnderUsProducesConflictCopy(t *testing.T) {
	r := newPoolRig(t, "a", "b")
	ctx := context.Background()
	r.members["a"].Seed("/shared.md", []byte("original"))
	if data, err := r.fs.ReadFileRange(ctx, "/shared.md", 0, 0); err != nil || string(data) != "original" {
		t.Fatalf("seeded read = %q, %v", data, err)
	}
	if _, err := r.fs.WriteFile(ctx, "/shared.md", []byte("my edit"), false); err != nil {
		t.Fatal(err)
	}
	// Someone else edits the member's copy before ours goes out.
	r.members["a"].Seed("/shared.md", []byte("their edit"))
	r.members["a"].SetMTime("/shared.md", time.Now().Add(time.Minute))
	if _, err := r.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fs.DropCaches(ctx); err != nil {
		t.Fatal(err)
	}
	got := listNames(t, r.fs, "/")
	if !strings.Contains(got, "shared (conflict ") || !strings.Contains(got, "shared.md") {
		t.Fatalf("listing after the race = %s", got)
	}
	if data, _ := r.members["a"].Content("/shared.md"); string(data) != "their edit" {
		t.Fatalf("their edit was overwritten: %q", data)
	}
}

// TestPoolMemberDownDoesNotAffectFilesOnOtherMembers: requirement 2 of the
// pool. A member that stops answering costs the files on other members
// nothing — not an error, not a wait on the dead member — once the pool has
// noticed, and the pool notices from the failures themselves.
func TestPoolMemberDownDoesNotAffectFilesOnOtherMembers(t *testing.T) {
	r := newPoolRigWith(t, config.Pool{Replicas: 2, MinReplicas: 1, ProbeInterval: time.Hour, OutAfter: time.Hour}, "a", "b")
	a, b := r.members["a"], r.members["b"]
	for i := 0; i < 4; i++ {
		b.Seed("/on-b/"+string(rune('0'+i))+".txt", []byte("b"))
		a.Seed("/on-a/"+string(rune('0'+i))+".txt", []byte("a"))
	}
	ctx := context.Background()
	for _, dir := range []string{"/", "/on-a", "/on-b"} {
		if _, err := r.fs.ReadDirPath(ctx, dir); err != nil {
			t.Fatal(err)
		}
	}
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true; ft.Latency = 20 * time.Millisecond })
	// Three failed operations mark a down. Each cold listing of /on-a
	// asks a once.
	for i := 0; i < 3; i++ {
		if _, err := r.fs.DropCaches(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := r.fs.ReadDirPath(ctx, "/on-a"); err != nil {
			t.Fatal(err)
		}
	}
	if st := r.pool.Status()[0]; st.Health.State != provider.HealthDown {
		t.Fatalf("a = %s after repeated failures", st.Health.State)
	}
	refused := a.Calls("down")
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := r.fs.DropCaches(ctx); err != nil {
			t.Fatal(err)
		}
		if got := listNames(t, r.fs, "/on-b"); got != "0.txt,1.txt,2.txt,3.txt" {
			t.Fatalf("/on-b with a down = %s", got)
		}
		if data, err := r.fs.ReadFileRange(ctx, "/on-b/2.txt", 0, 0); err != nil || string(data) != "b" {
			t.Fatalf("read on b = %q, %v", data, err)
		}
	}
	if a.Calls("down") != refused {
		t.Fatalf("operations on b's files touched the down member %d times", a.Calls("down")-refused)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("operations on b's files waited on the down member: %v", took)
	}
	// a's own files are still listed, and say why they cannot be read.
	if got := listNames(t, r.fs, "/on-a"); got != "0.txt,1.txt,2.txt,3.txt" {
		t.Fatalf("/on-a with a down = %s", got)
	}
	if _, err := r.fs.ReadFileRange(ctx, "/on-a/1.txt", 0, 0); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("read of a's file = %v", err)
	}
}

// TestPoolMemberRecoversWithoutRestart: the probe notices a member that
// answers again, and the next operation uses it.
func TestPoolMemberRecoversWithoutRestart(t *testing.T) {
	r := newPoolRigWith(t, config.Pool{Replicas: 2, MinReplicas: 1, ProbeInterval: time.Hour, OutAfter: time.Hour}, "a", "b")
	a := r.members["a"]
	a.Seed("/only-a.txt", []byte("a"))
	ctx := context.Background()
	if _, err := r.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	for i := 0; i < 3; i++ {
		_, _ = r.fs.ReadFileRange(ctx, "/only-a.txt", 0, 0)
	}
	if st := r.pool.Status()[0]; st.Health.State != provider.HealthDown {
		t.Fatalf("a = %s", st.Health.State)
	}
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = false })
	if _, err := r.fs.ReadFileRange(ctx, "/only-a.txt", 0, 0); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("before the probe the member is still down: %v", err)
	}
	r.pool.ProbeOnce(ctx)
	if data, err := r.fs.ReadFileRange(ctx, "/only-a.txt", 0, 0); err != nil || string(data) != "a" {
		t.Fatalf("after the probe = %q, %v", data, err)
	}
}

// TestPoolDeadMemberTriggersReReplication: requirement 4 of the pool. A
// member that stays down past out_after is treated as lost; its files are
// rebuilt on the remaining members from the surviving replicas, and they
// stay readable throughout.
func TestPoolDeadMemberTriggersReReplication(t *testing.T) {
	r := newPoolRigWith(t, config.Pool{Replicas: 2, MinReplicas: 1, OutAfter: 10 * time.Minute, ProbeInterval: time.Hour}, "a", "b", "c")
	a, b, c := r.members["a"], r.members["b"], r.members["c"]
	ctx := context.Background()
	if _, err := r.fs.WriteFile(ctx, "/precious.txt", []byte("two copies"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	holders := 0
	for _, f := range []*fakeprovider.Fake{a, b, c} {
		if _, ok := f.Content("/precious.txt"); ok {
			holders++
		}
	}
	if holders != 2 {
		t.Fatalf("after repair the file is on %d members, want 2", holders)
	}
	// The primary dies for good. Listings notice (the block cache still
	// serves the file's own bytes locally, as it should).
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	for i := 0; i < 3; i++ {
		if _, err := r.fs.DropCaches(ctx); err != nil {
			t.Fatal(err)
		}
		if got := listNames(t, r.fs, "/"); got != "precious.txt" {
			t.Fatalf("listing while a is down = %s", got)
		}
		if data, err := r.fs.ReadFileRange(ctx, "/precious.txt", 0, 0); err != nil || string(data) != "two copies" {
			t.Fatalf("read while a is down = %q, %v", data, err)
		}
	}
	if st := r.pool.Status()[0].Health.State; st != provider.HealthDown {
		t.Fatalf("a = %s", st)
	}
	r.clock = r.clock.Add(11 * time.Minute)
	if n, err := r.pool.ScanOnce(ctx); err != nil || n != 1 {
		t.Fatalf("scan after a went out: %d, %v", n, err)
	}
	if _, err := r.pool.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	holders = 0
	for _, f := range []*fakeprovider.Fake{b, c} {
		if got, ok := f.Content("/precious.txt"); ok {
			holders++
			if string(got) != "two copies" {
				t.Fatalf("%s holds %q", f.Name(), got)
			}
		}
	}
	if holders != 2 {
		t.Fatalf("after re-replication the file is on %d live members, want 2", holders)
	}
	if data, err := r.fs.ReadFileRange(ctx, "/precious.txt", 0, 0); err != nil || string(data) != "two copies" {
		t.Fatalf("read after re-replication = %q, %v", data, err)
	}
}

// TestPoolDrainMovesEverythingOffThenRemoves: taking a member out on
// purpose. Its files move to the others while every one of them stays
// readable, and when it is empty nothing of the pool is left on it.
func TestPoolDrainMovesEverythingOffThenRemoves(t *testing.T) {
	r := newPoolRigWith(t, config.Pool{Replicas: 2, MinReplicas: 1}, "a", "b", "c")
	a := r.members["a"]
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := r.fs.WriteFile(ctx, "/f"+string(rune('0'+i))+".txt", []byte("keep me"), false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(a.Tree()) != 5 {
		t.Fatalf("a as primary should hold everything: %v", a.Tree())
	}
	if err := r.pool.SetMemberState("a", "draining"); err != nil {
		t.Fatal(err)
	}
	empty := false
	for i := 0; i < 6 && !empty; i++ {
		var err error
		_, empty, err = r.pool.DrainOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.fs.DropCaches(ctx); err != nil {
			t.Fatal(err)
		}
		if data, err := r.fs.ReadFileRange(ctx, "/f2.txt", 0, 0); err != nil || string(data) != "keep me" {
			t.Fatalf("read during drain = %q, %v", data, err)
		}
	}
	if !empty || len(a.Tree()) != 0 {
		t.Fatalf("a after drain: empty=%v tree=%v", empty, a.Tree())
	}
	if got := listNames(t, r.fs, "/"); got != "f0.txt,f1.txt,f2.txt,f3.txt,f4.txt" {
		t.Fatalf("listing after drain = %s", got)
	}
	for _, f := range []*fakeprovider.Fake{r.members["b"], r.members["c"]} {
		if len(f.Tree()) != 5 {
			t.Fatalf("%s after drain: %v", f.Name(), f.Tree())
		}
	}
}

// TestScrubDetectsOutOfBandDeletion: a copy deleted in the vendor's app is
// a lost replica, not a deletion — deleting is done through the pool. The
// scrub notices and repair puts the copy back.
func TestScrubDetectsOutOfBandDeletion(t *testing.T) {
	r := newPoolRigWith(t, config.Pool{Replicas: 2, MinReplicas: 1, ScrubSample: 1}, "a", "b")
	a, b := r.members["a"], r.members["b"]
	ctx := context.Background()
	if _, err := r.fs.WriteFile(ctx, "/docs/keep.txt", nil, false); err == nil {
		t.Fatal("expected no /docs yet")
	}
	if _, err := r.fs.Mkdir(ctx, meta.RootIno, "docs"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fs.WriteFile(ctx, "/docs/keep.txt", []byte("two copies please"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Content("/docs/keep.txt"); !ok {
		t.Fatalf("b lacks the replica: %v", b.Tree())
	}
	// Deleted in b's app.
	id, _ := b.IDOf("/docs/keep.txt")
	if err := b.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.ScrubOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got, ok := b.Content("/docs/keep.txt"); !ok || string(got) != "two copies please" {
		t.Fatalf("b after scrub+repair = %q %v", got, ok)
	}
	if got, _ := a.Content("/docs/keep.txt"); string(got) != "two copies please" {
		t.Fatalf("a = %q", got)
	}
	if got := listNames(t, r.fs, "/docs"); got != "keep.txt" {
		t.Fatalf("listing = %s", got)
	}
}

// TestDeltaEchoOfOurOwnWriteIsANoOp: the pool has a change feed, and the
// refresher polls it. The member reporting the upload this machine just
// made must come back as the id and version the VFS already holds — zero
// applied changes — or the refresher and the pool would feed each other.
func TestDeltaEchoOfOurOwnWriteIsANoOp(t *testing.T) {
	r := newPoolRigTTL(t, config.Pool{Replicas: 2, MinReplicas: 1}, time.Hour, "a", "b")
	ctx := context.Background()
	ref := vfs.NewRefresher(r.fs, time.Minute)
	mount := r.fs.Mounts()[0]
	if _, err := r.fs.ReadDirPath(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	if _, err := ref.PollOnce(ctx, mount); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fs.WriteFile(ctx, "/mine.txt", []byte("written here"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.RepairOnce(ctx); err != nil {
		t.Fatal(err)
	}
	applied, err := ref.PollOnce(ctx, mount)
	if err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatalf("the echo of our own write (and its repair copy) applied %d changes", applied)
	}
	if data, err := r.fs.ReadFileRange(ctx, "/mine.txt", 0, 0); err != nil || string(data) != "written here" {
		t.Fatalf("read = %q, %v", data, err)
	}
}

// TestDeltaSurfacesVendorAppEdit: an edit made on a member in its own app
// reaches the mount through the feed, before any TTL would.
func TestDeltaSurfacesVendorAppEdit(t *testing.T) {
	r := newPoolRigTTL(t, config.Pool{Replicas: 2, MinReplicas: 1}, time.Hour, "a", "b")
	a := r.members["a"]
	a.Seed("/shared.txt", []byte("v1"))
	ctx := context.Background()
	ref := vfs.NewRefresher(r.fs, time.Minute)
	mount := r.fs.Mounts()[0]
	if data, err := r.fs.ReadFileRange(ctx, "/shared.txt", 0, 0); err != nil || string(data) != "v1" {
		t.Fatalf("read v1 = %q, %v", data, err)
	}
	if _, err := ref.PollOnce(ctx, mount); err != nil {
		t.Fatal(err)
	}
	a.Seed("/shared.txt", []byte("v2 from the app"))
	a.Seed("/appended.txt", []byte("new in the app"))
	applied, err := ref.PollOnce(ctx, mount)
	if err != nil || applied == 0 {
		t.Fatalf("applied = %d, %v", applied, err)
	}
	if data, err := r.fs.ReadFileRange(ctx, "/shared.txt", 0, 0); err != nil || string(data) != "v2 from the app" {
		t.Fatalf("read after the feed = %q, %v", data, err)
	}
	if got := listNames(t, r.fs, "/"); got != "appended.txt,shared.txt" {
		t.Fatalf("listing after the feed = %s", got)
	}
}

// TestRebalanceLosesNothingWhenAMemberGoesAwayMidPlan: the reliability
// matrix for a rebalance is simple — moves may fail, files may not
// disappear. Every move is copy, verify, drop; a member that stops
// answering in the middle leaves the file exactly where it was, and the
// queue remembers to try again.
func TestRebalanceLosesNothingWhenAMemberGoesAwayMidPlan(t *testing.T) {
	ctx := context.Background()
	r := newPoolRigSized(t, config.Pool{Replicas: 1, MinReplicas: 1, ProbeInterval: time.Hour}, 256, "a", "b")
	a, b := r.members["a"], r.members["b"]

	// Everything starts on a: b is the drive that was just added.
	if err := r.pool.SetMemberState("b", "disabled"); err != nil {
		t.Fatal(err)
	}
	const files = 8
	for i := 0; i < files; i++ {
		if _, err := r.fs.WriteFile(ctx, "/f"+string(rune('a'+i))+".txt", []byte{byte('a' + i), 'x', 'y'}, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.pool.SetMemberState("b", "enabled"); err != nil {
		t.Fatal(err)
	}
	plan, err := r.pool.PlanRebalance(ctx, 0.01, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Moves) == 0 {
		t.Fatalf("nothing planned: %+v", plan)
	}

	// The destination disappears halfway through the plan.
	moved := 0
	for i := 0; i < len(plan.Moves)*3; i++ {
		if moved == 1 {
			b.SetFaults(func(f *fakeprovider.Faults) { f.Down = true })
		}
		n, err := r.pool.RebalanceOnce(ctx)
		if err != nil {
			break
		}
		if n == 0 {
			break
		}
		moved += n
	}
	b.SetFaults(func(f *fakeprovider.Faults) { f.Down = false })

	// Every file is still readable through the mount, wherever it lives.
	for i := 0; i < files; i++ {
		path := "/f" + string(rune('a'+i)) + ".txt"
		got, err := r.fs.ReadFileRange(ctx, path, 0, 3)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if want := []byte{byte('a' + i), 'x', 'y'}; string(got) != string(want) {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
	// And no file lost its only copy: what the pool thinks is live is
	// really there on the member it names.
	for i := 0; i < files; i++ {
		path := "/f" + string(rune('a'+i)) + ".txt"
		_, onA := a.Content(path)
		_, onB := b.Content(path)
		if !onA && !onB {
			t.Fatalf("%s is on neither member", path)
		}
	}
	st, err := r.pool.RebalanceStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Queued+st.Done+st.Failed != len(plan.Moves) {
		t.Fatalf("queue accounts for %+v of %d planned moves", st, len(plan.Moves))
	}
}
