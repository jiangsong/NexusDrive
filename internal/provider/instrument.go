package provider

import (
	"context"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Stats counts the calls a backend actually made.
//
// It exists because every performance claim in this system is about remote
// calls, not wall-clock time: a cached listing is meant to cost zero requests,
// and a 4 KiB read of a cached file is meant to cost zero. Without a counter
// those are assertions nobody can check, and a regression that doubles remote
// traffic looks the same as one that does not.
type Stats struct {
	mu  sync.Mutex
	ops map[string]int64
	// nanos is the time spent inside each operation. Call counts say how
	// much traffic a change causes; this says whether what is left is the
	// round trip or the code around it, which is the difference between
	// "the network is the floor" and "we have work to remove".
	nanos map[string]int64
	// ReadBytes counts bytes handed to callers by ReadRange, and WriteBytes
	// the bytes accepted by UploadPart.
	readBytes  atomic.Int64
	writeBytes atomic.Int64
}

// NewStats returns an empty counter.
func NewStats() *Stats { return &Stats{ops: map[string]int64{}} }

// timed returns the function that records one call to op: the count and the
// time it took land together, under one acquisition of the lock every backend
// call has to take anyway.
func (s *Stats) timed(op string) func() {
	if s == nil {
		return func() {}
	}
	start := time.Now()
	return func() {
		d := time.Since(start)
		s.mu.Lock()
		if s.ops == nil {
			s.ops = map[string]int64{}
		}
		if s.nanos == nil {
			s.nanos = map[string]int64{}
		}
		s.ops[op]++
		s.nanos[op] += int64(d)
		s.mu.Unlock()
	}
}

// Nanos returns the time spent per operation, in nanoseconds.
func (s *Stats) Nanos() map[string]int64 {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyCounts(s.nanos)
}

// copyCounts returns a copy of m; s.mu must be held.
func copyCounts(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Report returns the counts, the time spent, and the total, taken together
// under one acquisition of the lock every backend call contends for.
func (s *Stats) Report() (calls, nanos map[string]int64, total int64) {
	if s == nil {
		return nil, nil, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	calls, nanos = copyCounts(s.ops), copyCounts(s.nanos)
	for _, v := range calls {
		total += v
	}
	return calls, nanos, total
}

// Snapshot returns the current counts by operation name.
func (s *Stats) Snapshot() map[string]int64 {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyCounts(s.ops)
}

// Total returns the number of calls across all operations.
func (s *Stats) Total() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, v := range s.ops {
		n += v
	}
	return n
}

// Ops returns the operation names seen so far, sorted.
func (s *Stats) Ops() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.ops))
	for k := range s.ops {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ReadBytes and WriteBytes report the payload actually transferred.
func (s *Stats) ReadBytes() int64  { return s.readBytes.Load() }
func (s *Stats) WriteBytes() int64 { return s.writeBytes.Load() }

// Reset clears the counters, which a benchmark does between phases.
func (s *Stats) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ops = map[string]int64{}
	s.nanos = map[string]int64{}
	s.mu.Unlock()
	s.readBytes.Store(0)
	s.writeBytes.Store(0)
}

// counting wraps a Provider and records every call.
type counting struct {
	Provider
	s  *Stats
	ra RangeReaderAt // nil when the backend has no buffer-filling read
	sl StreamLister  // nil when the backend has no streaming directory path
}

func (c *counting) ListStream(ctx context.Context, dirID string, visit func(Entry) error) error {
	if c.sl == nil {
		return ErrUnsupported
	}
	defer c.s.timed("list")()
	return c.sl.ListStream(ctx, dirID, visit)
}

func (c *counting) List(ctx context.Context, dirID, cursor string) ([]Entry, string, error) {
	defer c.s.timed("list")()
	return c.Provider.List(ctx, dirID, cursor)
}

func (c *counting) Stat(ctx context.Context, id string) (Entry, error) {
	defer c.s.timed("stat")()
	return c.Provider.Stat(ctx, id)
}

func (c *counting) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	done := c.s.timed("read_range")
	rc, err := c.Provider.ReadRange(ctx, id, version, off, n)
	if err != nil || rc == nil {
		done()
		return rc, err
	}
	// The time is billed on Close, not here: this call only opens the
	// stream, and read_range must mean the same interval whichever of the
	// two read paths a backend implements.
	return &countingReader{ReadCloser: rc, s: c.s, done: done}, nil
}

// ReadRangeAt forwards the buffer-filling read when the backend has one. The
// backend is asked once, at construction: this runs on every block fetch.
func (c *counting) ReadRangeAt(ctx context.Context, id, version string, off int64, buf []byte) (int, error) {
	if c.ra == nil {
		return 0, ErrUnsupported
	}
	done := c.s.timed("read_range")
	n, err := c.ra.ReadRangeAt(ctx, id, version, off, buf)
	done()
	c.s.readBytes.Add(int64(n))
	return n, err
}

func (c *counting) DownloadURL(ctx context.Context, id string) (Link, error) {
	defer c.s.timed("download_url")()
	return c.Provider.DownloadURL(ctx, id)
}

func (c *counting) BeginUpload(ctx context.Context, parentID, name string, size int64, h Hashes) (UploadSession, error) {
	defer c.s.timed("begin_upload")()
	return c.Provider.BeginUpload(ctx, parentID, name, size, h)
}

func (c *counting) UploadPart(ctx context.Context, s UploadSession, idx int, r io.Reader, n int64) (PartToken, error) {
	defer c.s.timed("upload_part")()
	t, err := c.Provider.UploadPart(ctx, s, idx, r, n)
	if err == nil && n > 0 {
		c.s.writeBytes.Add(n)
	}
	return t, err
}

func (c *counting) CompleteUpload(ctx context.Context, s UploadSession, parts []PartToken) (Entry, error) {
	defer c.s.timed("complete_upload")()
	return c.Provider.CompleteUpload(ctx, s, parts)
}

func (c *counting) Mkdir(ctx context.Context, parentID, name string) (Entry, error) {
	defer c.s.timed("mkdir")()
	return c.Provider.Mkdir(ctx, parentID, name)
}

func (c *counting) Rename(ctx context.Context, id, newName string) (Entry, error) {
	defer c.s.timed("rename")()
	return c.Provider.Rename(ctx, id, newName)
}

func (c *counting) Move(ctx context.Context, id, newParentID string) (Entry, error) {
	defer c.s.timed("move")()
	return c.Provider.Move(ctx, id, newParentID)
}

func (c *counting) Delete(ctx context.Context, id string) error {
	defer c.s.timed("delete")()
	return c.Provider.Delete(ctx, id)
}

// countingReader adds the bytes a caller actually consumed, and closes out
// the timing of the read that produced it.
type countingReader struct {
	io.ReadCloser
	s    *Stats
	done func()
}

func (r *countingReader) Close() error {
	if r.done != nil {
		r.done()
		r.done = nil
	}
	return r.ReadCloser.Close()
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.s.readBytes.Add(int64(n))
	}
	return n, err
}

type countingChanges struct {
	*counting
	cl ChangeLister
}

func (c *countingChanges) Changes(ctx context.Context, cursor string) ([]Change, string, error) {
	defer c.s.timed("changes")()
	return c.cl.Changes(ctx, cursor)
}

type countingCopy struct {
	*counting
	sc ServerCopier
}

func (c *countingCopy) Copy(ctx context.Context, id, newParentID, newName string) (Entry, error) {
	defer c.s.timed("copy")()
	return c.sc.Copy(ctx, id, newParentID, newName)
}

type countingBoth struct {
	*counting
	cl ChangeLister
	sc ServerCopier
}

func (c *countingBoth) Changes(ctx context.Context, cursor string) ([]Change, string, error) {
	defer c.s.timed("changes")()
	return c.cl.Changes(ctx, cursor)
}

func (c *countingBoth) Copy(ctx context.Context, id, newParentID, newName string) (Entry, error) {
	defer c.s.timed("copy")()
	return c.sc.Copy(ctx, id, newParentID, newName)
}

// countingPut forwards single-request uploads. It is stacked on top of the
// other variants by Instrument when the backend implements SinglePutter.
// countingPut also forwards the buffer-filling read; embedding the Provider
// interface would otherwise hide it.
func (c *countingPut) ReadRangeAt(ctx context.Context, id, version string, off int64, buf []byte) (int, error) {
	return c.base.ReadRangeAt(ctx, id, version, off, buf)
}

func (c *countingPut) ListStream(ctx context.Context, dirID string, visit func(Entry) error) error {
	return c.base.ListStream(ctx, dirID, visit)
}

type countingPut struct {
	base *counting
	Provider
	s  *Stats
	sp SinglePutter
}

func (c *countingPut) PutFile(ctx context.Context, parentID, name string, r io.Reader, size int64, h Hashes) (Entry, error) {
	defer c.s.timed("put_file")()
	e, err := c.sp.PutFile(ctx, parentID, name, r, size, h)
	if err == nil && size > 0 {
		c.s.writeBytes.Add(size)
	}
	return e, err
}

func (c *countingPut) unwrap() Provider { return c.Provider }

// Unwrap returns the backend inside an instrumented provider, or p itself.
// Callers that need the concrete driver — tests, and code that reaches for a
// driver-specific method — go through this rather than type-asserting the
// value the daemon holds.
func Unwrap(p Provider) Provider {
	for {
		u, ok := p.(interface{ unwrap() Provider })
		if !ok {
			return p
		}
		p = u.unwrap()
	}
}

func (c *counting) unwrap() Provider        { return c.Provider }
func (c *countingChanges) unwrap() Provider { return c.Provider }
func (c *countingCopy) unwrap() Provider    { return c.Provider }
func (c *countingBoth) unwrap() Provider    { return c.Provider }

// Instrument returns p wrapped so every call is counted into s.
//
// The optional interfaces decide behaviour elsewhere — the refresher polls a
// remote only when it satisfies ChangeLister — so the wrapper must implement
// the same change/copy variants. ReadRangeAt and ListStream are capability-
// gated probes that return ErrUnsupported when the underlying path is absent.
//
// Interfaces outside this package's own set, such as Transporter, are not
// forwarded: reach the driver through Unwrap instead of asserting on the value
// the daemon holds.
func Instrument(p Provider, s *Stats) Provider {
	if p == nil || s == nil {
		return p
	}
	base := &counting{Provider: p, s: s}
	if ra, ok := p.(RangeReaderAt); ok {
		base.ra = ra
	}
	base.sl, _ = p.(StreamLister)
	cl, hasChanges := p.(ChangeLister)
	sc, hasCopy := p.(ServerCopier)
	var wrapped Provider = base
	switch {
	case hasChanges && hasCopy:
		wrapped = &countingBoth{counting: base, cl: cl, sc: sc}
	case hasChanges:
		wrapped = &countingChanges{counting: base, cl: cl}
	case hasCopy:
		wrapped = &countingCopy{counting: base, sc: sc}
	}
	if sp, ok := p.(SinglePutter); ok {
		// Wrapping the already-wrapped provider keeps every other optional
		// interface reachable through embedding? It does not — embedding an
		// interface value only forwards Provider's methods — so the put
		// variant is only used when there is nothing else to preserve.
		if !hasChanges && !hasCopy {
			wrapped = &countingPut{base: base, Provider: base, s: s, sp: sp}
		}
	}
	return wrapped
}
