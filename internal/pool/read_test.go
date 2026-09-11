package pool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

// fanBlock is the VFS block size: readahead asks for one of these per
// request, so it is the unit fan-out spreads.
const fanBlock = 4 << 20

// seedBig puts the same file of the given number of blocks on every fake
// and returns its content. Each block has its own byte, so a block served
// from the wrong offset is caught.
func seedBig(pth string, blocks int, fakes ...*fakeprovider.Fake) []byte {
	data := make([]byte, 0, blocks*fanBlock)
	for i := 0; i < blocks; i++ {
		data = append(data, bytes.Repeat([]byte{byte('a' + i)}, fanBlock)...)
	}
	for _, f := range fakes {
		f.Seed(pth, data)
	}
	return data
}

func setLatency(d time.Duration, fakes ...*fakeprovider.Fake) {
	for _, f := range fakes {
		f.SetFaults(func(ft *fakeprovider.Faults) { ft.Latency = d })
	}
}

// fileEntry lists the pool root and returns the named file.
func fileEntry(t *testing.T, p *Pool, name string) provider.Entry {
	t.Helper()
	entries, _, err := p.List(context.Background(), rootID, "")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := find(entries, name)
	if !ok {
		t.Fatalf("%s not listed: %v", name, names(entries))
	}
	return e
}

// readBlocks reads every block of e through window concurrent ReadRange
// calls — one request per block, which is what VFS readahead issues — and
// checks every byte.
func readBlocks(t *testing.T, p *Pool, e provider.Entry, want []byte, window int) {
	t.Helper()
	blocks := len(want) / fanBlock
	next := make(chan int, blocks)
	for i := 0; i < blocks; i++ {
		next <- i
	}
	close(next)
	errs := make(chan error, blocks)
	var wg sync.WaitGroup
	for w := 0; w < window; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := make([]byte, fanBlock)
			for i := range next {
				off := int64(i) * fanBlock
				rc, err := p.ReadRange(context.Background(), e.ID, e.Version, off, fanBlock)
				if err != nil {
					errs <- fmt.Errorf("block %d: %w", i, err)
					continue
				}
				_, err = io.ReadFull(rc, got)
				rc.Close()
				if err != nil {
					errs <- fmt.Errorf("block %d body: %w", i, err)
					continue
				}
				if !bytes.Equal(got, want[off:off+fanBlock]) {
					errs <- fmt.Errorf("block %d: wrong bytes", i)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
}

func readRangeCalls(fakes ...*fakeprovider.Fake) []int {
	out := make([]int, len(fakes))
	for i, f := range fakes {
		out[i] = f.Calls("ReadRange")
	}
	return out
}

// newPoolOf builds a pool over arbitrary providers, for tests that wrap a
// fake to misbehave in one specific way.
func newPoolOf(t *testing.T, settings config.Pool, providers ...provider.Provider) *Pool {
	t.Helper()
	opt := Options{Name: "home", StateDir: t.TempDir(), Settings: settings}
	for _, pr := range providers {
		opt.Members = append(opt.Members, Member{Name: pr.Name(), Provider: pr, Adopt: true})
	}
	p, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// TestReadFanoutSpreadsBlocks: the parallel block requests of one reader
// land on every replica, not on the first one declared.
func TestReadFanoutSpreadsBlocks(t *testing.T) {
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	want := seedBig("/movie.mkv", 12, a, b, c)
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 3, MinReplicas: 1}, a, b, c)
	e := fileEntry(t, p, "movie.mkv")
	setLatency(20*time.Millisecond, a, b, c)

	readBlocks(t, p, e, want, 8)
	for i, n := range readRangeCalls(a, b, c) {
		if n < 3 || n > 5 {
			t.Errorf("member %d served %d of 12 blocks, want 4 ± 1 (all: %v)", i, n, readRangeCalls(a, b, c))
		}
	}
}

// TestReadFanoutSkipsDownMember: a member that went down stops receiving
// blocks; the others share them.
func TestReadFanoutSkipsDownMember(t *testing.T) {
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	want := seedBig("/movie.mkv", 12, a, b, c)
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 3, MinReplicas: 1, ProbeInterval: time.Hour, OutAfter: time.Hour}, a, b, c)
	ctx := context.Background()
	e := fileEntry(t, p, "movie.mkv")
	a.SetFaults(func(ft *fakeprovider.Faults) { ft.Down = true })
	// A read that lands on a fails over inside the same call.
	rc, err := p.ReadRange(ctx, e.ID, e.Version, 0, fanBlock)
	if err != nil {
		t.Fatalf("read with a failing: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, want[:fanBlock]) {
		t.Fatal("failover read: wrong bytes")
	}
	// Failed listings take a down, as any failing operation does.
	for i := 0; i < 3; i++ {
		if _, _, err := p.List(ctx, rootID, ""); err != nil {
			t.Fatal(err)
		}
	}
	if st := p.Status()[0].Health.State; st != provider.HealthDown {
		t.Fatalf("a = %s after failing, want down", st)
	}
	setLatency(20*time.Millisecond, b, c)
	refused := a.Calls("down")
	before := readRangeCalls(b, c)

	readBlocks(t, p, e, want, 8)
	if extra := a.Calls("down") - refused; extra != 0 {
		t.Fatalf("a down member was asked %d times", extra)
	}
	after := readRangeCalls(b, c)
	db, dc := after[0]-before[0], after[1]-before[1]
	if db+dc != 12 || db < 4 || dc < 4 {
		t.Fatalf("b served %d and c %d of 12 blocks; want both to share them", db, dc)
	}
}

// TestReadFanoutPrefersFast: a slow replica gets its first requests while
// it is unmeasured, and none once the others are known to be faster.
func TestReadFanoutPrefersFast(t *testing.T) {
	a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
	want := seedBig("/movie.mkv", 12, a, b, c)
	p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 3, MinReplicas: 1}, a, b, c)
	e := fileEntry(t, p, "movie.mkv")
	setLatency(5*time.Millisecond, a, b)
	setLatency(40*time.Millisecond, c)

	readBlocks(t, p, e, want, 8)
	first := readRangeCalls(a, b, c)
	if first[2] == 0 {
		t.Fatalf("c was never tried while unmeasured: %v", first)
	}
	readBlocks(t, p, e, want, 8)
	all := readRangeCalls(a, b, c)
	if all[2] >= all[0] || all[2] >= all[1] {
		t.Fatalf("slow c served %d blocks, fast a %d and b %d", all[2], all[0], all[1])
	}
	if again := all[2] - first[2]; again != 0 {
		t.Fatalf("c, measured 8× slower, still served %d blocks of the second read", again)
	}
}

// TestUnofficialMemberGetsOneStreamPerFile: under read_fanout auto a drive
// on an unofficial API never serves two ranges of one file at once — the
// pattern that trips risk control — unless it is the only holder. `all`
// lifts the restriction.
func TestUnofficialMemberGetsOneStreamPerFile(t *testing.T) {
	for _, tc := range []struct {
		mode string
		ok   func(peak int) bool
		want string
	}{
		{mode: "", ok: func(peak int) bool { return peak == 1 }, want: "exactly 1"},
		{mode: config.ReadFanoutAuto, ok: func(peak int) bool { return peak == 1 }, want: "exactly 1"},
		{mode: config.ReadFanoutAll, ok: func(peak int) bool { return peak >= 2 }, want: "at least 2"},
	} {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			a, q := fakeprovider.New("a"), fakeprovider.New("q")
			caps := q.Capabilities()
			caps.Tier = provider.TierUnofficial
			q.SetCaps(caps)
			want := seedBig("/movie.mkv", 8, a, q)
			p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1, ReadFanout: tc.mode}, a, q)
			e := fileEntry(t, p, "movie.mkv")
			setLatency(30*time.Millisecond, a, q)

			readBlocks(t, p, e, want, 8)
			if q.Calls("ReadRange") == 0 {
				t.Fatal("the unofficial member served nothing; it should still carry one stream")
			}
			if peak := q.PeakConcurrent(); !tc.ok(peak) {
				t.Fatalf("unofficial member peaked at %d concurrent calls, want %s", peak, tc.want)
			}
		})
	}
	t.Run("only holder", func(t *testing.T) {
		a, q := fakeprovider.New("a"), fakeprovider.New("q")
		caps := q.Capabilities()
		caps.Tier = provider.TierUnofficial
		q.SetCaps(caps)
		want := seedBig("/solo.bin", 4, q)
		p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 2, MinReplicas: 1}, a, q)
		e := fileEntry(t, p, "solo.bin")
		setLatency(30*time.Millisecond, q)
		readBlocks(t, p, e, want, 4)
		if n := q.Calls("ReadRange"); n != 4 {
			t.Fatalf("the only holder served %d of 4 blocks", n)
		}
	})
}

// flaky404 answers ErrNotFound to the first few range reads while the file
// is still there, the way a drive does while a direct link is refreshed.
type flaky404 struct {
	*fakeprovider.Fake
	left atomic.Int32
}

func (f *flaky404) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	if f.left.Add(-1) >= 0 {
		return nil, provider.ErrNotFound
	}
	return f.Fake.ReadRange(ctx, id, version, off, n)
}

// TestTransient404DoesNotDropReplica: one 404 from a member that still has
// the file (its Stat says so) leaves the replica live and readable.
func TestTransient404DoesNotDropReplica(t *testing.T) {
	a, bf := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/f.txt", []byte("F"))
	bf.Seed("/f.txt", []byte("F"))
	b := &flaky404{Fake: bf}
	b.left.Store(1)
	p := newPoolOf(t, config.Pool{Replicas: 2, MinReplicas: 1}, a, b)
	ctx := context.Background()
	e := fileEntry(t, p, "f.txt")
	for i := 0; i < 4; i++ {
		if got := readAll(t, p, e.ID); got != "F" {
			t.Fatalf("read %d = %q", i, got)
		}
	}
	if b.left.Load() >= 0 {
		t.Fatal("b never answered 404; the test did not exercise the path")
	}
	if n := bf.Calls("Stat"); n != 1 {
		t.Fatalf("b was asked to confirm the 404 %d times, want 1", n)
	}
	reps, err := p.replicasOf(ctx, "/f.txt", e.Version)
	if err != nil || len(reps) != 2 {
		t.Fatalf("replicas after a transient 404 = %d, %v; want both live", len(reps), err)
	}
	if bf.Calls("ReadRange") == 0 {
		t.Fatal("b is no longer read after a transient 404")
	}
}

// TestConfirmed404MarksReplicaMissing: a member whose Stat agrees the file
// is gone loses the replica, and the next reads do not ask it again.
func TestConfirmed404MarksReplicaMissing(t *testing.T) {
	a, b := fakeprovider.New("a"), fakeprovider.New("b")
	a.Seed("/f.txt", []byte("F"))
	b.Seed("/f.txt", []byte("F"))
	p := newTestPool(t, t.TempDir(), a, b)
	ctx := context.Background()
	e := fileEntry(t, p, "f.txt")
	if got := readAll(t, p, e.ID); got != "F" {
		t.Fatalf("read = %q", got)
	}
	id, _ := b.IDOf("/f.txt")
	if err := b.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if got := readAll(t, p, e.ID); got != "F" {
			t.Fatalf("read %d after b lost the file = %q", i, got)
		}
	}
	reps, err := p.replicasOf(ctx, "/f.txt", e.Version)
	if err != nil || len(reps) != 1 || reps[0].member != "a" {
		t.Fatalf("replicas = %+v, %v; want only a", reps, err)
	}
	before := b.Calls("ReadRange")
	for i := 0; i < 4; i++ {
		readAll(t, p, e.ID)
	}
	if extra := b.Calls("ReadRange") - before; extra != 0 {
		t.Fatalf("a replica confirmed missing was read %d more times", extra)
	}
}

// TestReadFanoutOffKeepsOneOrderedStream: `off` is the v1 behaviour — every
// concurrent read goes to the replica that ranks first, past its connection
// budget. Under auto the overflow goes to the other holders.
func TestReadFanoutOffKeepsOneOrderedStream(t *testing.T) {
	for _, tc := range []struct {
		mode string
		ok   func(onFastest int) bool
		want string
	}{
		{mode: config.ReadFanoutOff, ok: func(n int) bool { return n == 8 }, want: "all 8"},
		{mode: config.ReadFanoutAuto, ok: func(n int) bool { return n < 8 }, want: "fewer than 8"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
			want := seedBig("/movie.mkv", 8, a, b, c)
			for _, f := range []*fakeprovider.Fake{a, b, c} {
				caps := f.Capabilities()
				caps.MaxConnsPerHost = 2
				f.SetCaps(caps)
			}
			p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 3, MinReplicas: 1, ReadFanout: tc.mode}, a, b, c)
			ctx := context.Background()
			e := fileEntry(t, p, "movie.mkv")
			setLatency(10*time.Millisecond, a)
			setLatency(20*time.Millisecond, b, c)
			// Measure every member once, so a ranks first by a margin no
			// scheduling jitter closes.
			for i := 0; i < 3; i++ {
				rc, err := p.ReadRange(ctx, e.ID, e.Version, 0, fanBlock)
				if err != nil {
					t.Fatal(err)
				}
				rc.Close()
			}
			before := readRangeCalls(a, b, c)
			readBlocks(t, p, e, want, 8)
			after := readRangeCalls(a, b, c)
			if onA := after[0] - before[0]; !tc.ok(onA) {
				t.Fatalf("read_fanout %s sent %d of 8 concurrent blocks to the fastest member, want %s (before %v after %v)", tc.mode, onA, tc.want, before, after)
			}
		})
	}
}

// TestReplicaCacheFollowsIndexWrites: the resolved-replica cache never
// serves a read from what the index said before a write, rename, delete or
// repair.
func TestReplicaCacheFollowsIndexWrites(t *testing.T) {
	ctx := context.Background()
	t.Run("overwrite", func(t *testing.T) {
		a, b := fakeprovider.New("a"), fakeprovider.New("b")
		p := newTestPool(t, t.TempDir(), a, b)
		e := upload(t, ctx, p, rootID, "o.txt", []byte("first"))
		if got := readAll(t, p, e.ID); got != "first" {
			t.Fatalf("read = %q", got)
		}
		e2 := upload(t, ctx, p, rootID, "o.txt", []byte("second version"))
		if got := readAll(t, p, e2.ID); got != "second version" {
			t.Fatalf("read after overwrite = %q", got)
		}
	})
	t.Run("rename on path-id members", func(t *testing.T) {
		a, b := fakeprovider.NewPathIDs("a"), fakeprovider.NewPathIDs("b")
		a.Seed("/d/f.txt", []byte("moved"))
		b.Seed("/d/f.txt", []byte("moved"))
		p := newTestPool(t, t.TempDir(), a, b)
		d := fileEntry(t, p, "d")
		inner, _, err := p.List(ctx, d.ID, "")
		if err != nil || len(inner) != 1 {
			t.Fatalf("/d = %v, %v", names(inner), err)
		}
		f := inner[0]
		for i := 0; i < 2; i++ {
			readAll(t, p, f.ID)
		}
		if _, err := p.Rename(ctx, f.ID, "g.txt"); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if got := readAll(t, p, f.ID); got != "moved" {
				t.Fatalf("read after rename = %q", got)
			}
		}
		row, _, _ := p.entryAt(ctx, "/d/g.txt")
		if reps, err := p.replicasOf(ctx, "/d/g.txt", row.ctoken); err != nil || len(reps) != 2 {
			t.Fatalf("replicas after rename = %d, %v", len(reps), err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		a, b := fakeprovider.New("a"), fakeprovider.New("b")
		a.Seed("/f.txt", []byte("F"))
		b.Seed("/f.txt", []byte("F"))
		p := newTestPool(t, t.TempDir(), a, b)
		e := fileEntry(t, p, "f.txt")
		readAll(t, p, e.ID)
		if err := p.Delete(ctx, e.ID); err != nil {
			t.Fatal(err)
		}
		before := a.Calls("ReadRange") + b.Calls("ReadRange")
		if _, err := p.ReadRange(ctx, e.ID, "", 0, 1); !errors.Is(err, provider.ErrNotFound) {
			t.Fatalf("read after delete = %v, want ErrNotFound", err)
		}
		if extra := a.Calls("ReadRange") + b.Calls("ReadRange") - before; extra != 0 {
			t.Fatalf("a read of a deleted file asked members %d times", extra)
		}
	})
	t.Run("repair adds a replica", func(t *testing.T) {
		a, b, c := fakeprovider.New("a"), fakeprovider.New("b"), fakeprovider.New("c")
		p := newTestPoolWith(t, t.TempDir(), config.Pool{Replicas: 3, MinReplicas: 1}, a, b, c)
		e := upload(t, ctx, p, rootID, "r.txt", []byte("replicate me"))
		readAll(t, p, e.ID)
		if _, _, reps, err := p.resolveFile(ctx, e.ID, ""); err != nil || len(reps) != 1 {
			t.Fatalf("before repair the read path sees %d replicas, %v; want 1", len(reps), err)
		}
		if made, err := p.RepairOnce(ctx); err != nil || made != 2 {
			t.Fatalf("repair made %d, %v", made, err)
		}
		if _, _, reps, err := p.resolveFile(ctx, e.ID, ""); err != nil || len(reps) != 3 {
			t.Fatalf("after repair the read path sees %d replicas, %v; want 3", len(reps), err)
		}
		if got := readAll(t, p, e.ID); got != "replicate me" {
			t.Fatalf("read after repair = %q", got)
		}
	})
}

// TestLatencyIsComparableAcrossRequestSizes: the EWMA is per MiB, so a
// member answering big requests is not mistaken for a slow one; a request
// under a MiB counts as one MiB.
func TestLatencyIsComparableAcrossRequestSizes(t *testing.T) {
	small, big, tiny := &member{}, &member{}, &member{}
	small.noteLatency(40*time.Millisecond, 4<<20)
	big.noteLatency(320*time.Millisecond, 32<<20)
	tiny.noteLatency(10*time.Millisecond, 0)
	if small.latency != big.latency || small.latency != float64(10*time.Millisecond) {
		t.Fatalf("per-MiB latency: 4 MiB → %v, 32 MiB → %v; want 10ms both", time.Duration(small.latency), time.Duration(big.latency))
	}
	if tiny.latency != float64(10*time.Millisecond) || !tiny.measured {
		t.Fatalf("a sub-MiB request = %v (measured %v), want 10ms", time.Duration(tiny.latency), tiny.measured)
	}
}

func TestReplicaCacheExpiresAndIsBounded(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newReplicaCache(5*time.Second, 4, func() time.Time { return now })
	v := resolved{pth: "/f", reps: []replicaRow{{member: "a"}}}
	gen := c.generation()
	c.put("p-1", "", gen, v)
	if got, ok := c.get("p-1", ""); !ok || got.pth != "/f" {
		t.Fatalf("fresh entry = %+v, %v", got, ok)
	}
	if _, ok := c.get("p-1", "h1:x"); ok {
		t.Fatal("a different version is a different key")
	}
	now = now.Add(6 * time.Second)
	if _, ok := c.get("p-1", ""); ok {
		t.Fatal("an entry older than the TTL was served")
	}
	c.put("p-1", "", c.generation(), v)
	c.invalidate()
	if _, ok := c.get("p-1", ""); ok {
		t.Fatal("an entry survived invalidation")
	}
	// A lookup that started before an index write must not store what it
	// read: the write already happened.
	stale := c.generation()
	c.invalidate()
	c.put("p-2", "", stale, v)
	if _, ok := c.get("p-2", ""); ok {
		t.Fatal("a result read before an invalidation was stored")
	}
	for i := 0; i < 20; i++ {
		c.put(fmt.Sprintf("p-%d", i), "", c.generation(), v)
	}
	if n := c.size(); n > 4 {
		t.Fatalf("cache holds %d entries, bound is 4", n)
	}
}

// TestIndexWritesOutsideTxInvalidateReplicaCache: every transaction
// invalidates the replica cache; a statement that changes replicas, entries
// or ids outside one must go through execIndex, which does too.
func TestIndexWritesOutsideTxInvalidateReplicaCache(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	direct := regexp.MustCompile("p\\.db\\.ExecContext\\(ctx,\\s*`\\s*(?:INSERT(?: OR \\w+)? INTO|UPDATE|DELETE FROM)\\s+(?:replicas|entries|ids)\\b")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := direct.FindIndex(b); loc != nil {
			line := bytes.Count(b[:loc[0]], []byte("\n")) + 1
			t.Errorf("%s:%d changes the index outside a transaction without execIndex", f, line)
		}
	}
}
