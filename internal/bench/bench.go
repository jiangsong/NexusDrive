// Package bench is the built-in IO benchmark behind `cloudfs bench`. It runs a
// fixed set of workloads against a directory and, when pointed at a daemon's
// control address, records how many backend requests each workload cost.
//
// The shape follows `juicefs bench`: the tool owns its dataset, runs the same
// work against any directory — a cloudfs mount, sshfs, a local disk — and
// prints one JSON object per workload so a script can compare without parsing
// prose. Remote call counts are the number that matters for a cache: a warm
// listing is meant to cost zero, and a result that only reports seconds
// cannot tell a cache hit from a fast network.
package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a run.
type Options struct {
	// Dir is the directory under test. The dataset lives directly below it.
	Dir string
	// Label tags every result, e.g. "cloudfs cold".
	Label string
	// Metrics is the daemon's control address (host:port). Empty disables
	// call counting.
	Metrics string
	// Cold, when set, is called before every repetition so the measurement
	// starts from empty caches.
	Cold func(ctx context.Context) error
	// Repeat runs each workload this many times and reports the median.
	Repeat int
	// Threads is the parallelism for parread and stress.
	Threads int
	// BigMiB sizes the large file; the second large file is a quarter of it.
	BigMiB int
	// SmallCount and SmallSize describe the small-file batch.
	SmallCount, SmallSize int
	// MetaFiles is the count for the create/stat/readdir/unlink workload.
	MetaFiles int
	// Out receives one JSON line per result. nil means os.Stdout.
	Out io.Writer
}

func (o *Options) withDefaults() {
	if o.Repeat <= 0 {
		o.Repeat = 1
	}
	if o.Threads <= 0 {
		o.Threads = 4
	}
	if o.BigMiB <= 0 {
		o.BigMiB = 256
	}
	if o.SmallCount <= 0 {
		o.SmallCount = 500
	}
	if o.SmallSize <= 0 {
		o.SmallSize = 4 << 10
	}
	if o.MetaFiles <= 0 {
		o.MetaFiles = 10000
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
}

// Result is one workload's outcome.
type Result struct {
	Test   string  `json:"test"`
	Label  string  `json:"label"`
	Ops    int64   `json:"ops"`
	Bytes  int64   `json:"bytes"`
	WallMS float64 `json:"wall_ms"`
	// MBs and OpsS are derived from the median run.
	MBs     float64 `json:"mb_s,omitempty"`
	OpsS    float64 `json:"ops_s,omitempty"`
	CloseMS float64 `json:"close_ms,omitempty"`
	Runs    int     `json:"runs"`
	MinMS   float64 `json:"min_ms,omitempty"`
	MaxMS   float64 `json:"max_ms,omitempty"`
	Note    string  `json:"note,omitempty"`
	Failed  bool    `json:"failed,omitempty"`

	// Backend cost of the median run, when Metrics is set.
	RemoteCalls int64            `json:"remote_calls"`
	RemoteByOp  map[string]int64 `json:"remote_by_op,omitempty"`
	// RemoteMsByOp is how long those calls took inside the daemon, which is
	// what separates a slow link from slow code above it.
	RemoteMsByOp     map[string]float64 `json:"remote_ms_by_op,omitempty"`
	CacheHits        int64              `json:"cache_hits"`
	CacheMisses      int64              `json:"cache_misses"`
	RemoteReadBytes  int64              `json:"remote_read_bytes"`
	RemoteWriteBytes int64              `json:"remote_write_bytes"`

	// Kernel requests the median run sent this process: zero on a warm
	// tree means the kernel answered by itself. FuseReadSizes is the
	// cumulative READ size histogram, which decides what a random miss
	// costs at the backend.
	FuseOps       map[string]int64 `json:"fuse_ops,omitempty"`
	FuseReadBytes int64            `json:"fuse_read_bytes,omitempty"`
	FuseReadSizes map[string]int64 `json:"fuse_read_sizes,omitempty"`
}

func (r *Result) derive() {
	if r.WallMS <= 0 {
		return
	}
	if r.Bytes > 0 {
		r.MBs = float64(r.Bytes) / (1 << 20) / (r.WallMS / 1000)
	}
	if r.Ops > 0 {
		r.OpsS = float64(r.Ops) / (r.WallMS / 1000)
	}
}

// workload runs one test and returns its raw result.
type workload func(o *Options) Result

// All lists the workloads in the order they run by default.
var All = []string{
	"walk", "statstorm", "seqread1m", "seqread4k", "randread", "parread",
	"seqwrite", "randwrite", "smallfiles", "metadata", "stress",
}

var workloads = map[string]workload{
	"walk":       func(o *Options) Result { return walk(o) },
	"statstorm":  func(o *Options) Result { return statstorm(o) },
	"seqread1m":  func(o *Options) Result { return seqread(o, bigName, 1<<20) },
	"seqread4k":  func(o *Options) Result { return seqread(o, big4Name, 4<<10) },
	"randread":   func(o *Options) Result { return randread(o, 500, 4<<10) },
	"parread":    func(o *Options) Result { return parread(o, 50, 64<<10) },
	"seqwrite":   func(o *Options) Result { return seqwrite(o, o.BigMiB/4) },
	"randwrite":  func(o *Options) Result { return randwrite(o, 64, 500, 4<<10) },
	"smallfiles": func(o *Options) Result { return smallfiles(o) },
	"metadata":   func(o *Options) Result { return metadata(o) },
	"stress":     func(o *Options) Result { return stress(o, 25, 1024) },
}

// Run executes the named workloads, each Repeat times, and writes one result
// line per workload. The metadata workload writes four lines, one per phase.
func Run(ctx context.Context, o Options, names []string) ([]Result, error) {
	o.withDefaults()
	if len(names) == 0 {
		names = All
	}
	var out []Result
	for _, name := range names {
		w, ok := workloads[strings.TrimSpace(name)]
		if !ok {
			return out, fmt.Errorf("bench: unknown workload %q (known: %s)", name, strings.Join(All, ", "))
		}
		res, err := repeat(ctx, &o, w)
		if err != nil {
			return out, err
		}
		for _, r := range res {
			out = append(out, r)
			line, _ := json.Marshal(r)
			fmt.Fprintln(o.Out, string(line))
		}
	}
	return out, nil
}

// repeat runs w Repeat times and folds the runs into one result per test
// name: the median run's figures, with min and max of the wall time.
func repeat(ctx context.Context, o *Options, w workload) ([]Result, error) {
	type run struct {
		r     Result
		delta metricsDelta
	}
	byTest := map[string][]run{}
	var order []string
	for i := 0; i < o.Repeat; i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if o.Cold != nil {
			if err := o.Cold(ctx); err != nil {
				return nil, fmt.Errorf("bench: cold reset: %w", err)
			}
		}
		before := scrape(o.Metrics)
		r := w(o)
		after := scrape(o.Metrics)
		// The metadata workload reports phases through a side channel.
		results := []Result{r}
		if extra, ok := phaseResults(r); ok {
			results = extra
		}
		for _, rr := range results {
			rr.Label = o.Label
			if _, seen := byTest[rr.Test]; !seen {
				order = append(order, rr.Test)
			}
			byTest[rr.Test] = append(byTest[rr.Test], run{r: rr, delta: after.sub(before)})
		}
	}
	var out []Result
	for _, test := range order {
		runs := byTest[test]
		sort.Slice(runs, func(i, j int) bool { return runs[i].r.WallMS < runs[j].r.WallMS })
		med := runs[len(runs)/2]
		r := med.r
		r.Runs = len(runs)
		r.MinMS = runs[0].r.WallMS
		r.MaxMS = runs[len(runs)-1].r.WallMS
		r.RemoteCalls = med.delta.calls
		r.RemoteByOp = med.delta.byOp
		if len(med.delta.secondsByOp) > 0 {
			r.RemoteMsByOp = make(map[string]float64, len(med.delta.secondsByOp))
			for op, sec := range med.delta.secondsByOp {
				r.RemoteMsByOp[op] = sec * 1000
			}
		}
		r.CacheHits, r.CacheMisses = med.delta.hits, med.delta.misses
		r.RemoteReadBytes, r.RemoteWriteBytes = med.delta.readBytes, med.delta.writeBytes
		r.FuseOps, r.FuseReadBytes, r.FuseReadSizes = med.delta.fuseOps, med.delta.fuseReadBytes, med.delta.fuseReadSizes
		r.derive()
		out = append(out, r)
	}
	return out, nil
}

// Dataset names.
const (
	treeDir   = "tree"
	bigName   = "big.bin"
	big4Name  = "big-quarter.bin"
	scratch   = "scratch"
	treeDirs  = 40
	treeFiles = 50
)

// Prepare creates the dataset under dir if it is not already there: a tree of
// small files for metadata work, two large files for throughput, and a
// scratch directory for writes. It is idempotent so a dataset written through
// one mount can be reused by the next run.
func Prepare(dir string, o Options) error {
	o.withDefaults()
	if err := os.MkdirAll(filepath.Join(dir, scratch), 0o755); err != nil {
		return err
	}
	root := filepath.Join(dir, treeDir)
	if _, err := os.Stat(root); err != nil {
		blob := make([]byte, o.SmallSize)
		rand.New(rand.NewSource(7)).Read(blob)
		for d := 0; d < treeDirs; d++ {
			p := filepath.Join(root, fmt.Sprintf("d%02d", d))
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			for f := 0; f < treeFiles; f++ {
				if err := os.WriteFile(filepath.Join(p, fmt.Sprintf("f%03d.dat", f)), blob, 0o644); err != nil {
					return err
				}
			}
		}
	}
	for _, f := range []struct {
		name string
		mib  int
	}{{bigName, o.BigMiB}, {big4Name, o.BigMiB / 4}} {
		p := filepath.Join(dir, f.name)
		if st, err := os.Stat(p); err == nil && st.Size() == int64(f.mib)<<20 {
			continue
		}
		if err := writeRandom(p, int64(f.mib)<<20); err != nil {
			return err
		}
	}
	return nil
}

func writeRandom(p string, size int64) error {
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	rng := rand.New(rand.NewSource(size))
	buf := make([]byte, 1<<20)
	for written := int64(0); written < size; {
		rng.Read(buf)
		n := int64(len(buf))
		if size-written < n {
			n = size - written
		}
		if _, err := f.Write(buf[:n]); err != nil {
			f.Close()
			return err
		}
		written += n
	}
	return f.Close()
}

func ms(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }

func fail(test string, err error) Result {
	return Result{Test: test, Failed: true, Note: "FAILED: " + err.Error()}
}

// walk stats every entry under tree/, which is what find, ls -R and a
// source-tree grep all reduce to.
func walk(o *Options) Result {
	root := filepath.Join(o.Dir, treeDir)
	var files, dirs int64
	start := time.Now()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			dirs++
		} else {
			files++
		}
		return nil
	})
	el := time.Since(start)
	if err != nil {
		return fail("walk", err)
	}
	return Result{Test: "walk", Ops: files + dirs, WallMS: ms(el),
		Note: fmt.Sprintf("%d files, %d dirs", files, dirs)}
}

// statstorm hits every file by path, the pattern a build tool or git produces.
func statstorm(o *Options) Result {
	var paths []string
	filepath.WalkDir(filepath.Join(o.Dir, treeDir), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			paths = append(paths, p)
		}
		return nil
	})
	start := time.Now()
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return fail("statstorm", err)
		}
	}
	return Result{Test: "statstorm", Ops: int64(len(paths)), WallMS: ms(time.Since(start))}
}

func seqread(o *Options, name string, bufSize int) Result {
	test := fmt.Sprintf("seqread-%dk", bufSize/1024)
	f, err := os.Open(filepath.Join(o.Dir, name))
	if err != nil {
		return fail(test, err)
	}
	defer f.Close()
	buf := make([]byte, bufSize)
	var total, ops int64
	start := time.Now()
	for {
		n, err := f.Read(buf)
		total += int64(n)
		ops++
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(test, err)
		}
	}
	return Result{Test: test, Ops: ops, Bytes: total, WallMS: ms(time.Since(start))}
}

func randread(o *Options, n, size int) Result {
	test := fmt.Sprintf("randread-%dk", size/1024)
	f, err := os.Open(filepath.Join(o.Dir, bigName))
	if err != nil {
		return fail(test, err)
	}
	defer f.Close()
	st, _ := f.Stat()
	max := st.Size() - int64(size)
	rng := rand.New(rand.NewSource(42))
	buf := make([]byte, size)
	start := time.Now()
	for i := 0; i < n; i++ {
		if _, err := f.ReadAt(buf, rng.Int63n(max)); err != nil {
			return fail(test, err)
		}
	}
	return Result{Test: test, Ops: int64(n), Bytes: int64(n * size), WallMS: ms(time.Since(start))}
}

// parread runs several readers at once, the shape of a build or a media scan.
func parread(o *Options, opsPerWorker, size int) Result {
	test := fmt.Sprintf("parread-%dx%dk", o.Threads*2, size/1024)
	workers := o.Threads * 2
	p := filepath.Join(o.Dir, bigName)
	st, err := os.Stat(p)
	if err != nil {
		return fail(test, err)
	}
	max := st.Size() - int64(size)
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			f, err := os.Open(p)
			if err != nil {
				errs <- err
				return
			}
			defer f.Close()
			rng := rand.New(rand.NewSource(seed))
			buf := make([]byte, size)
			for i := 0; i < opsPerWorker; i++ {
				if _, err := f.ReadAt(buf, rng.Int63n(max)); err != nil {
					errs <- err
					return
				}
			}
		}(int64(w) + 1)
	}
	wg.Wait()
	close(errs)
	el := time.Since(start)
	if err := <-errs; err != nil {
		return fail(test, err)
	}
	ops := int64(workers * opsPerWorker)
	return Result{Test: test, Ops: ops, Bytes: ops * int64(size), WallMS: ms(el)}
}

// seqwrite reports the write phase and the close phase separately: for a
// write-back filesystem close() is where the durability decision is made.
func seqwrite(o *Options, mib int) Result {
	test := fmt.Sprintf("seqwrite-%dm", mib)
	p := filepath.Join(o.Dir, scratch, fmt.Sprintf("w-%s-%d.bin", slug(o.Label), mib))
	os.Remove(p)
	f, err := os.Create(p)
	if err != nil {
		return fail(test, err)
	}
	buf := make([]byte, 1<<20)
	rand.New(rand.NewSource(1)).Read(buf)
	start := time.Now()
	var total int64
	for i := 0; i < mib; i++ {
		n, err := f.Write(buf)
		total += int64(n)
		if err != nil {
			f.Close()
			return fail(test, err)
		}
	}
	writeEl := time.Since(start)
	cstart := time.Now()
	if err := f.Close(); err != nil {
		return fail(test, fmt.Errorf("close: %w", err))
	}
	closeEl := time.Since(cstart)
	return Result{Test: test, Ops: int64(mib), Bytes: total, WallMS: ms(writeEl + closeEl), CloseMS: ms(closeEl)}
}

// randwrite overwrites random 4 KiB spots in an existing file, the pattern a
// database or a VM image produces. The file is written fresh first so the
// result does not depend on what an earlier run left behind.
func randwrite(o *Options, mib, n, size int) Result {
	test := fmt.Sprintf("randwrite-%dk", size/1024)
	p := filepath.Join(o.Dir, scratch, fmt.Sprintf("rw-%s.bin", slug(o.Label)))
	if err := writeRandom(p, int64(mib)<<20); err != nil {
		return fail(test, err)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return fail(test, err)
	}
	rng := rand.New(rand.NewSource(3))
	buf := make([]byte, size)
	rng.Read(buf)
	max := int64(mib)<<20 - int64(size)
	start := time.Now()
	for i := 0; i < n; i++ {
		if _, err := f.WriteAt(buf, rng.Int63n(max)); err != nil {
			f.Close()
			return fail(test, err)
		}
	}
	writeEl := time.Since(start)
	cstart := time.Now()
	if err := f.Close(); err != nil {
		return fail(test, fmt.Errorf("close: %w", err))
	}
	closeEl := time.Since(cstart)
	return Result{Test: test, Ops: int64(n), Bytes: int64(n * size), WallMS: ms(writeEl + closeEl), CloseMS: ms(closeEl)}
}

// smallfiles is the create-heavy pattern: unpacking an archive, saving from
// an editor, an agent writing a batch of files.
func smallfiles(o *Options) Result {
	test := fmt.Sprintf("smallfiles-%d", o.SmallCount)
	base := filepath.Join(o.Dir, scratch, "many-"+slug(o.Label))
	os.RemoveAll(base)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return fail(test, err)
	}
	buf := make([]byte, o.SmallSize)
	rand.New(rand.NewSource(2)).Read(buf)
	start := time.Now()
	for i := 0; i < o.SmallCount; i++ {
		p := filepath.Join(base, fmt.Sprintf("f%04d.dat", i))
		f, err := os.Create(p)
		if err != nil {
			return fail(test, err)
		}
		if _, err := f.Write(buf); err != nil {
			f.Close()
			return fail(test, err)
		}
		if err := f.Close(); err != nil {
			return fail(test, fmt.Errorf("close: %w", err))
		}
	}
	return Result{Test: test, Ops: int64(o.SmallCount), Bytes: int64(o.SmallCount * o.SmallSize), WallMS: ms(time.Since(start))}
}

// phaseKey marks a result that carries several phase results in its Note.
const phaseKey = "\x00phases:"

// metadata is the mdtest shape: create N empty files, stat them, list the
// directory, unlink them. Each phase is its own result line.
func metadata(o *Options) Result {
	base := filepath.Join(o.Dir, scratch, "meta-"+slug(o.Label))
	os.RemoveAll(base)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return fail("meta-create", err)
	}
	n := o.MetaFiles
	names := make([]string, n)
	for i := range names {
		names[i] = filepath.Join(base, fmt.Sprintf("m%06d", i))
	}
	var phases []Result

	start := time.Now()
	for _, p := range names {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if err != nil {
			return fail("meta-create", err)
		}
		if err := f.Close(); err != nil {
			return fail("meta-create", fmt.Errorf("close: %w", err))
		}
	}
	phases = append(phases, Result{Test: "meta-create", Ops: int64(n), WallMS: ms(time.Since(start))})

	start = time.Now()
	for _, p := range names {
		if _, err := os.Stat(p); err != nil {
			return fail("meta-stat", err)
		}
	}
	phases = append(phases, Result{Test: "meta-stat", Ops: int64(n), WallMS: ms(time.Since(start))})

	start = time.Now()
	entries, err := os.ReadDir(base)
	if err != nil {
		return fail("meta-readdir", err)
	}
	if len(entries) != n {
		return fail("meta-readdir", fmt.Errorf("listed %d entries, want %d", len(entries), n))
	}
	phases = append(phases, Result{Test: "meta-readdir", Ops: int64(n), WallMS: ms(time.Since(start))})

	start = time.Now()
	for _, p := range names {
		if err := os.Remove(p); err != nil {
			return fail("meta-unlink", err)
		}
	}
	phases = append(phases, Result{Test: "meta-unlink", Ops: int64(n), WallMS: ms(time.Since(start))})
	os.Remove(base)

	b, _ := json.Marshal(phases)
	return Result{Test: "metadata", Note: phaseKey + string(b)}
}

func phaseResults(r Result) ([]Result, bool) {
	if !strings.HasPrefix(r.Note, phaseKey) {
		return nil, false
	}
	var out []Result
	if err := json.Unmarshal([]byte(strings.TrimPrefix(r.Note, phaseKey)), &out); err != nil {
		return nil, false
	}
	return out, true
}

// stress is the sustained mixed workload the design is meant to survive:
// concurrent writers and readers on one tree, each verifying what it wrote.
// A failure here is a correctness bug, not a slow result.
func stress(o *Options, iters, sizeKB int) Result {
	test := fmt.Sprintf("stress-%dw-%dkb", o.Threads, sizeKB)
	base := filepath.Join(o.Dir, scratch, "stress-"+slug(o.Label))
	os.RemoveAll(base)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return fail(test, err)
	}
	payload := make([]byte, sizeKB*1024)
	rand.New(rand.NewSource(9)).Read(payload)
	var wg sync.WaitGroup
	var failures atomic.Int64
	var firstErr atomic.Value
	start := time.Now()
	for w := 0; w < o.Threads; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				p := filepath.Join(base, fmt.Sprintf("w%d-%04d.bin", id, i))
				if err := os.WriteFile(p, payload, 0o644); err != nil {
					failures.Add(1)
					firstErr.CompareAndSwap(nil, "write: "+err.Error())
					continue
				}
				got, err := os.ReadFile(p)
				if err != nil {
					failures.Add(1)
					firstErr.CompareAndSwap(nil, "read: "+err.Error())
					continue
				}
				if !bytes.Equal(got, payload) {
					failures.Add(1)
					firstErr.CompareAndSwap(nil, fmt.Sprintf("content mismatch: got %d bytes", len(got)))
				}
			}
		}(w)
	}
	wg.Wait()
	el := time.Since(start)
	ops := int64(o.Threads * iters)
	r := Result{Test: test, Ops: ops, Bytes: ops * int64(sizeKB) * 1024, WallMS: ms(el)}
	if n := failures.Load(); n > 0 {
		r.Failed = true
		r.Note = fmt.Sprintf("%d/%d iterations FAILED; first: %v", n, ops, firstErr.Load())
	}
	return r
}

func slug(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '/' || r == '=' {
			return '_'
		}
		return r
	}, s)
}

// ErrNoMetrics is returned by Scrape when the address is empty.
var ErrNoMetrics = errors.New("bench: no metrics address")
