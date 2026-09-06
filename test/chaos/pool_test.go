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
		AttrTTL: time.Second, DefaultDirTTL: time.Second, NegativeTTL: time.Second,
		Mounts: []vfs.Mount{{Prefix: "/", Remote: "home", RootID: p.RootID(), Provider: p, Mode: config.ModeWriteback, DirTTL: time.Second}},
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
