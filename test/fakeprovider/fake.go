// Package fakeprovider is an in-memory Provider with fault injection. It is the
// backend for unit, conformance and chaos tests: every call is counted so a
// test can assert "second access made zero remote calls", and Faults lets a
// test inject latency, 429s, risk-control bans, disconnects and link expiry.
package fakeprovider

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/provider"
)

// Faults configures failure injection. Zero value injects nothing.
type Faults struct {
	Latency time.Duration
	// RateLimitEvery makes every Nth call fail with ErrRateLimited (0 = off).
	RateLimitEvery int
	// RiskControlAfter makes calls fail with ErrRiskControl once the total
	// call count exceeds it (0 = off).
	RiskControlAfter int
	// FailNext makes the next N calls fail with ErrTransient.
	FailNext int
	// ShortRead caps every ReadRange answer at this many bytes (0 = off),
	// the way a server that limits request size does.
	ShortRead int64
	// LinkTTL is how long DownloadURL links stay valid.
	LinkTTL time.Duration
	// CursorReset makes the next Changes call report that the cursor is no
	// longer valid, with the current end of the log as the replacement.
	CursorReset bool
	// Down makes every call fail with ErrTransient until cleared, the way a
	// backend that is simply unreachable behaves. FailNext is a count; Down is
	// a state, which is what "this drive is gone" needs. Calls refused this
	// way are counted under "down", not under their own op.
	Down bool
	// MaxConcurrent, when > 0, serves at most this many calls at once; the
	// rest wait their turn, the way a drive's connection budget caps its
	// throughput. Set it before calls start.
	MaxConcurrent int
	// FailOp makes the next N calls of one operation ("Copy",
	// "CompleteUpload") fail with ErrTransient, the way a call that timed
	// out looks: the caller never learns whether it took effect.
	FailOp map[string]int
	// QuotaExceeded makes every write fail with ErrQuotaExceeded, the way
	// a drive whose account is already full answers.
	QuotaExceeded bool
	// QuotaAfterBytes, when > 0, accepts writes until the backend holds
	// that many bytes and fails with ErrQuotaExceeded after that — a drive
	// that fills up mid-transfer, which is where an upload has to be
	// restarted somewhere else rather than retried here.
	QuotaAfterBytes int64
}

type node struct {
	entry    provider.Entry
	data     []byte
	children map[string]string // name -> id
}

type upload struct {
	parentID string
	name     string
	size     int64
	parts    map[int][]byte
}

// Fake is the in-memory provider.
type Fake struct {
	name string
	caps provider.Caps

	mu      sync.Mutex
	nodes   map[string]*node
	blobs   map[string][]byte // sha1 -> data, for rapid upload
	uploads map[string]*upload
	changes []provider.Change
	links   map[string]time.Time // token -> expiry
	nextID  int
	calls   map[string]int
	// readBytes sums the lengths of every ReadRange, so a test can tell a
	// sub-block fetch from a whole-block one.
	readBytes int64
	// maxReadLen is the high-water mark of the requested length (n) across
	// every ReadRange call since creation or the last ResetReadRangeStats,
	// so a test can assert "nothing bigger than one block was ever asked
	// for" without inspecting individual calls.
	maxReadLen int64
	total      int
	// sem, active and peak belong to admit (concurrency.go).
	sem    chan struct{}
	active int
	peak   int
	Faults Faults
	// noHashes hides content hashes from every Entry, the way a backend
	// without a hash API (sftp, webdav) reports files.
	noHashes bool
	// quota is what Quota reports; zero Total means the backend cannot say.
	quota provider.Quota
	// hideNaming keeps the naming rules out of Capabilities while still
	// enforcing them, the way a drive that documents nothing behaves.
	hideNaming bool
	// rootID is the id of the root directory. Opaque-id backends use the
	// RootID constant; a path-id backend uses "/".
	rootID string
	// pathIDs makes an entry's id its slash path, the way sftp, webdav, s3
	// and smb address things. Renaming or moving a directory then changes
	// the id of everything beneath it.
	pathIDs bool
}

// RootID is the id of the root directory.
const RootID = "root"

// New returns an empty Fake with one root directory.
func New(name string) *Fake {
	f := &Fake{
		name:    name,
		nodes:   map[string]*node{},
		blobs:   map[string][]byte{},
		uploads: map[string]*upload{},
		links:   map[string]time.Time{},
		calls:   map[string]int{},
		caps: provider.Caps{
			HashTypes:       []provider.HashType{provider.HashSHA1},
			RapidUpload:     []provider.HashType{provider.HashSHA1},
			RangeRead:       true,
			PartSize:        4 << 20,
			MaxParts:        10000,
			UploadParallel:  4,
			ServerMove:      true,
			ServerRename:    true,
			ServerCopy:      false,
			Delta:           true,
			LinkTTL:         15 * time.Minute,
			LinkShareable:   true,
			QPS:             provider.QPS{Meta: 10, Download: 10, Upload: 5},
			MaxConnsPerHost: 8,
			Tier:            provider.TierOfficial,
		},
	}
	f.rootID = RootID
	f.nodes[f.rootID] = &node{
		entry:    provider.Entry{ID: f.rootID, Name: "", Kind: provider.KindDir, ModTime: time.Now()},
		children: map[string]string{},
	}
	return f
}

// NewPathIDs returns an empty Fake that addresses entries by path instead of
// by an opaque id, the way sftp, webdav, s3 and smb do. Renaming or moving a
// directory rewrites the id of every entry beneath it, which is the behaviour
// the tree has to survive.
func NewPathIDs(name string) *Fake {
	f := New(name)
	delete(f.nodes, f.rootID)
	f.rootID = "/"
	f.pathIDs = true
	f.caps.PathIDs = true
	f.nodes[f.rootID] = &node{
		entry:    provider.Entry{ID: f.rootID, Name: "", Kind: provider.KindDir, ModTime: time.Now()},
		children: map[string]string{},
	}
	return f
}

// childID is the id a child of parent takes. With opaque ids it is a fresh
// counter; with path ids it is the parent's path joined with the name.
func (f *Fake) childID(parent, name string) string {
	if !f.pathIDs {
		f.nextID++
		return "n" + strconv.Itoa(f.nextID)
	}
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

// rekeyLocked rewrites the id of n and of everything beneath it after its
// path changed. Only a path-id backend needs it; an opaque id survives a
// rename untouched.
func (f *Fake) rekeyLocked(oldID, newID string) {
	n, ok := f.nodes[oldID]
	if !ok {
		return
	}
	delete(f.nodes, oldID)
	n.entry.ID = newID
	f.nodes[newID] = n
	for name, cid := range n.children {
		childNew := newID + "/" + name
		if newID == "/" {
			childNew = "/" + name
		}
		f.nodes[cid].entry.ParentID = newID
		f.rekeyLocked(cid, childNew)
		n.children[name] = childNew
	}
}

// Calls returns how many times op was invoked ("List", "ReadRange", …).
func (f *Fake) Calls(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

// TotalCalls returns the total number of provider calls.
// ReadBytes is the total length requested through ReadRange.
func (f *Fake) ReadBytes() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readBytes
}

// MaxReadRangeLen returns the largest length any single ReadRange call has
// requested since creation or the last ResetReadRangeStats. A test asserting
// "the caller never asked for more than one block" reads this instead of
// inspecting every call.
func (f *Fake) MaxReadRangeLen() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxReadLen
}

// ResetReadRangeStats zeroes the MaxReadRangeLen high-water mark, so a test
// can check "nothing this large happened after this point" without a second
// Fake.
func (f *Fake) ResetReadRangeStats() {
	f.mu.Lock()
	f.maxReadLen = 0
	f.mu.Unlock()
}

func (f *Fake) TotalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.total
}

// Seed creates a file directly, bypassing the upload path. Path segments are
// created as directories as needed. Returns the file entry.
func (f *Fake) Seed(path string, data []byte) provider.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	segs := splitPath(path)
	parent := f.rootID
	for _, s := range segs[:len(segs)-1] {
		if id, ok := f.nodes[parent].children[s]; ok {
			parent = id
			continue
		}
		e := f.newNode(parent, s, provider.KindDir, nil)
		parent = e.ID
	}
	// Overwriting keeps the same id and mints a new version, the way a real
	// drive behaves. Handing out a fresh id would make every rewrite look like
	// a delete plus a create to anything watching the change feed.
	e, _ := f.putFile(parent, segs[len(segs)-1], data)
	return e
}

func (f *Fake) newNode(parent, name string, kind provider.Kind, data []byte) provider.Entry {
	id := f.childID(parent, name)
	e := provider.Entry{ID: id, ParentID: parent, Name: name, Kind: kind, ModTime: time.Now()}
	if kind == provider.KindFile {
		e.Size = int64(len(data))
		sum := sha1sum(data)
		e.Hashes = provider.Hashes{provider.HashSHA1: sum}
		e.Version = sum
		f.blobs[sum] = data
	}
	n := &node{entry: e, data: data}
	if kind == provider.KindDir {
		n.children = map[string]string{}
	}
	f.nodes[id] = n
	f.nodes[parent].children[name] = id
	f.record(provider.Change{Op: provider.ChangeUpsert, ID: id, ParentID: parent, Entry: &e})
	return e
}

func (f *Fake) record(c provider.Change) { f.changes = append(f.changes, c) }

// view is what a caller sees of an entry: with hashes hidden, the version
// is what a hashless backend can still distinguish (size and mtime).
func (f *Fake) view(e provider.Entry) provider.Entry {
	if !f.noHashes || e.Kind != provider.KindFile {
		return e
	}
	e.Hashes = nil
	e.Version = ""
	provider.EnsureVersion(&e)
	return e
}

// SetQuota sets what the backend reports as its space.
func (f *Fake) SetQuota(total, used int64) {
	f.mu.Lock()
	f.quota = provider.Quota{Total: total, Used: used}
	f.mu.Unlock()
}

// Quota implements provider.Quotaer.
func (f *Fake) Quota(ctx context.Context) (provider.Quota, error) {
	if err := f.enter(ctx, "Quota"); err != nil {
		return provider.Quota{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.quota.Total == 0 {
		return provider.Quota{}, provider.ErrUnsupported
	}
	return f.quota, nil
}

// Copy implements provider.ServerCopier: the backend duplicates a file
// without the bytes leaving it. Enable it with SetCaps(ServerCopy: true);
// it is off by default because most drives cannot do it.
func (f *Fake) Copy(ctx context.Context, id, newParentID, newName string) (provider.Entry, error) {
	if err := f.enter(ctx, "Copy"); err != nil {
		return provider.Entry{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.caps.ServerCopy {
		return provider.Entry{}, provider.ErrUnsupported
	}
	src, ok := f.nodes[id]
	if !ok || src.entry.Kind != provider.KindFile {
		return provider.Entry{}, provider.ErrNotFound
	}
	if _, ok := f.nodes[newParentID]; !ok {
		return provider.Entry{}, provider.ErrNotFound
	}
	if err := f.checkName(newParentID, newName); err != nil {
		return provider.Entry{}, err
	}
	if f.noRoom(int64(len(src.data))) {
		return provider.Entry{}, f.errFull()
	}
	data := append([]byte(nil), src.data...)
	e, ok := f.putFile(newParentID, newName, data)
	if !ok {
		return provider.Entry{}, fmt.Errorf("%w: parent directory of %s is gone", provider.ErrNotFound, newName)
	}
	return f.view(e), nil
}

// SetNaming makes the backend refuse names by these rules, the way a real
// drive does, so a pool's placement can be tested against a refusal.
func (f *Fake) SetNaming(n provider.Naming) {
	f.mu.Lock()
	f.caps.Naming = n
	f.mu.Unlock()
}

// checkName applies the naming rules to a name about to be created.
// Caller holds the lock.
func (f *Fake) checkName(parentID, name string) error {
	if err := provider.CheckName(f.caps.Naming, name); err != nil {
		return err
	}
	if f.caps.Naming.CaseInsensitive {
		if p, ok := f.nodes[parentID]; ok {
			for existing := range p.children {
				if existing != name && strings.EqualFold(existing, name) {
					return fmt.Errorf("%w: %q differs from %q only by case", provider.ErrExists, name, existing)
				}
			}
		}
	}
	return nil
}

// SetReportHashes hides (false) or shows (true, the default) content hashes.
func (f *Fake) SetReportHashes(on bool) {
	f.mu.Lock()
	f.noHashes = !on
	f.mu.Unlock()
}

// SetMTime rewrites the modification time of the file at path, the way an
// out-of-band edit or a backend that stamps its own time would. It mints no
// new content version.
func (f *Fake) SetMTime(path string, t time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.lookupLocked(path)
	if !ok {
		return false
	}
	f.nodes[id].entry.ModTime = t
	return true
}

// Tree lists every path the backend holds, sorted, directories with a
// trailing slash. Tests use it to assert that a mirrored replica landed at
// the real path a user would see in the vendor's own app.
func (f *Fake) Tree() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	var walk func(id, prefix string)
	walk = func(id, prefix string) {
		n := f.nodes[id]
		names := make([]string, 0, len(n.children))
		for name := range n.children {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			cid := n.children[name]
			c := f.nodes[cid]
			if c.entry.Kind == provider.KindDir {
				out = append(out, prefix+name+"/")
				walk(cid, prefix+name+"/")
			} else {
				out = append(out, prefix+name)
			}
		}
	}
	walk(f.rootID, "/")
	return out
}

// lookupLocked resolves a slash path to an id. Caller holds the lock.
func (f *Fake) lookupLocked(path string) (string, bool) {
	id := f.rootID
	for _, s := range splitPath(path) {
		n, ok := f.nodes[id]
		if !ok {
			return "", false
		}
		if id, ok = n.children[s]; !ok {
			return "", false
		}
	}
	_, ok := f.nodes[id]
	return id, ok
}

// enter applies fault injection and counts the call. Caller must hold no lock.
func (f *Fake) enter(ctx context.Context, op string) error {
	f.mu.Lock()
	if f.Faults.Down {
		f.calls["down"]++
		f.mu.Unlock()
		return provider.ErrTransient
	}
	f.calls[op]++
	f.total++
	total := f.total
	faults := f.Faults
	if faults.FailNext > 0 {
		f.Faults.FailNext--
	}
	failOp := false
	if n := f.Faults.FailOp[op]; n > 0 {
		f.Faults.FailOp[op] = n - 1
		failOp = true
	}
	f.mu.Unlock()

	done, err := f.admit(ctx, faults.MaxConcurrent)
	if err != nil {
		return err
	}
	defer done()
	if faults.Latency > 0 {
		select {
		case <-time.After(faults.Latency):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if faults.FailNext > 0 || failOp {
		return provider.ErrTransient
	}
	if faults.RiskControlAfter > 0 && total > faults.RiskControlAfter {
		return provider.ErrRiskControl
	}
	if faults.RateLimitEvery > 0 && total%faults.RateLimitEvery == 0 {
		return &provider.RetryAfterError{Err: provider.ErrRateLimited, RetryAfter: 50 * time.Millisecond}
	}
	return nil
}

func (f *Fake) Name() string { return f.name }

// RootID reports the id the daemon should list a mount's root from. In-process
// tests build the mount with RootID directly, but the daemon discovers it
// through this interface (daemon/account.go); without it a fake remote mounted
// via a config would list from "/" and 404. Exposing it makes `fake` a usable
// empty drive for a running daemon, which is what the dev/demo setup needs.
func (f *Fake) RootID() string { return f.rootID }

// Capabilities returns the capability matrix. It takes the lock because
// tests change the matrix while the daemon is running.
func (f *Fake) Capabilities() provider.Caps {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.caps
	if f.hideNaming {
		c.Naming = provider.Naming{}
	}
	return c
}

// SetNamingAdvertised hides (false) the naming rules from Capabilities
// while the backend keeps refusing by them.
func (f *Fake) SetNamingAdvertised(on bool) {
	f.mu.Lock()
	f.hideNaming = !on
	f.mu.Unlock()
}

// SetCaps overrides the capability matrix (tests use it to simulate backends
// without hashes, without Range, etc.).
func (f *Fake) SetCaps(c provider.Caps) {
	f.mu.Lock()
	f.caps = c
	f.mu.Unlock()
}

// SetFaults changes the fault injection under the lock, so a test may turn
// faults on and off while the daemon is calling in.
func (f *Fake) SetFaults(fn func(*Faults)) {
	f.mu.Lock()
	fn(&f.Faults)
	f.mu.Unlock()
}

// ListPageSize keeps cursor handling exercised by every test. It is exported
// so a test that asserts on call counts can state the paging cost of a
// directory instead of hard-coding a number that would silently drift.
const ListPageSize = 2

func (f *Fake) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	if err := f.enter(ctx, "List"); err != nil {
		return nil, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.nodes[dirID]
	if !ok || d.entry.Kind != provider.KindDir {
		return nil, "", provider.ErrNotFound
	}
	names := make([]string, 0, len(d.children))
	for n := range d.children {
		names = append(names, n)
	}
	sort.Strings(names)
	page := ListPageSize
	start := 0
	if cursor != "" {
		start, _ = strconv.Atoi(cursor)
	}
	end := start + page
	if end > len(names) {
		end = len(names)
	}
	out := make([]provider.Entry, 0, end-start)
	for _, n := range names[start:end] {
		out = append(out, f.view(f.nodes[d.children[n]].entry))
	}
	next := ""
	if end < len(names) {
		next = strconv.Itoa(end)
	}
	return out, next, nil
}

func (f *Fake) Stat(ctx context.Context, id string) (provider.Entry, error) {
	if err := f.enter(ctx, "Stat"); err != nil {
		return provider.Entry{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[id]
	if !ok {
		return provider.Entry{}, provider.ErrNotFound
	}
	return f.view(n.entry), nil
}

func (f *Fake) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	if err := f.enter(ctx, "ReadRange"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	nd, ok := f.nodes[id]
	if !ok || nd.entry.Kind != provider.KindFile {
		return nil, provider.ErrNotFound
	}
	if version != "" && version != nd.entry.Version {
		return nil, provider.ErrConflict
	}
	f.readBytes += n
	if n > f.maxReadLen {
		f.maxReadLen = n
	}
	if off < 0 || off > int64(len(nd.data)) {
		return nil, fmt.Errorf("fakeprovider: range %d out of bounds", off)
	}
	end := off + n
	if n < 0 || end > int64(len(nd.data)) {
		end = int64(len(nd.data))
	}
	if f.Faults.ShortRead > 0 && end-off > f.Faults.ShortRead {
		end = off + f.Faults.ShortRead
	}
	return io.NopCloser(bytes.NewReader(nd.data[off:end])), nil
}

func (f *Fake) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	if err := f.enter(ctx, "DownloadURL"); err != nil {
		return provider.Link{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.nodes[id]; !ok {
		return provider.Link{}, provider.ErrNotFound
	}
	ttl := f.Faults.LinkTTL
	if ttl == 0 {
		ttl = f.caps.LinkTTL
	}
	exp := time.Now().Add(ttl)
	tok := fmt.Sprintf("%s-%d", id, exp.UnixNano())
	f.links[tok] = exp
	return provider.Link{URL: "fake://" + f.name + "/" + tok, ExpiresAt: exp, Headers: f.caps.LinkHeaders}, nil
}

// LinkValid reports whether a URL from DownloadURL is still valid (tests use
// it to model CDN 403 on expired links).
func (f *Fake) LinkValid(url string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for tok, exp := range f.links {
		if "fake://"+f.name+"/"+tok == url {
			return time.Now().Before(exp)
		}
	}
	return false
}

// noRoom reports whether the backend would refuse to store extra more
// bytes. Caller holds the lock.
func (f *Fake) noRoom(extra int64) bool {
	if f.Faults.QuotaExceeded {
		return true
	}
	if f.Faults.QuotaAfterBytes <= 0 {
		return false
	}
	var held int64
	for _, n := range f.nodes {
		held += int64(len(n.data))
	}
	for _, u := range f.uploads {
		for _, part := range u.parts {
			held += int64(len(part))
		}
	}
	return held+extra > f.Faults.QuotaAfterBytes
}

// errFull is what a backend with no room answers.
func (f *Fake) errFull() error {
	return fmt.Errorf("%w: %s is full", provider.ErrQuotaExceeded, f.name)
}

func (f *Fake) BeginUpload(ctx context.Context, parentID, name string, size int64, h provider.Hashes) (provider.UploadSession, error) {
	if err := f.enter(ctx, "BeginUpload"); err != nil {
		return provider.UploadSession{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noRoom(0) {
		return provider.UploadSession{}, f.errFull()
	}
	p, ok := f.nodes[parentID]
	if !ok || p.entry.Kind != provider.KindDir {
		return provider.UploadSession{}, provider.ErrNotFound
	}
	if err := f.checkName(parentID, name); err != nil {
		return provider.UploadSession{}, err
	}
	if sum := h[provider.HashSHA1]; sum != "" {
		if data, ok := f.blobs[sum]; ok && int64(len(data)) == size {
			e, _ := f.putFile(parentID, name, data)
			e = f.view(e)
			return provider.UploadSession{ID: "rapid-" + e.ID, RapidDone: true, Entry: &e}, nil
		}
	}
	f.nextID++
	id := "u" + strconv.Itoa(f.nextID)
	f.uploads[id] = &upload{parentID: parentID, name: name, size: size, parts: map[int][]byte{}}
	return provider.UploadSession{ID: id, PartSize: f.caps.PartSize, Opaque: map[string]string{"upload_id": id}}, nil
}

func (f *Fake) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	if err := f.enter(ctx, "UploadPart"); err != nil {
		return provider.PartToken{}, err
	}
	data, err := io.ReadAll(io.LimitReader(r, n))
	if err != nil {
		return provider.PartToken{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.uploads[s.ID]
	if !ok {
		return provider.PartToken{}, provider.ErrNotFound
	}
	if f.noRoom(int64(len(data))) {
		return provider.PartToken{}, f.errFull()
	}
	u.parts[idx] = data
	return provider.PartToken{Index: idx, ETag: sha1sum(data)}, nil
}

func (f *Fake) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	if err := f.enter(ctx, "CompleteUpload"); err != nil {
		return provider.Entry{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.uploads[s.ID]
	if !ok {
		// Idempotent: a completed upload id resolves to the resulting entry.
		if id, done := f.completedUpload(s.ID); done {
			return f.view(f.nodes[id].entry), nil
		}
		return provider.Entry{}, provider.ErrNotFound
	}
	var buf bytes.Buffer
	for _, p := range parts {
		d, ok := u.parts[p.Index]
		if !ok || sha1sum(d) != p.ETag {
			return provider.Entry{}, fmt.Errorf("fakeprovider: part %d missing or etag mismatch", p.Index)
		}
		buf.Write(d)
	}
	if int64(buf.Len()) != u.size {
		return provider.Entry{}, fmt.Errorf("fakeprovider: size mismatch: got %d want %d", buf.Len(), u.size)
	}
	if f.noRoom(0) {
		return provider.Entry{}, f.errFull()
	}
	e, ok := f.putFile(u.parentID, u.name, buf.Bytes())
	if !ok {
		delete(f.uploads, s.ID)
		return provider.Entry{}, fmt.Errorf("%w: parent directory of %s is gone", provider.ErrNotFound, u.name)
	}
	delete(f.uploads, s.ID)
	f.nodes[e.ID].entry.Hashes["upload_id"] = s.ID // remember for idempotent Complete
	return f.view(e), nil
}

func (f *Fake) completedUpload(uploadID string) (string, bool) {
	for id, n := range f.nodes {
		if n.entry.Hashes["upload_id"] == uploadID {
			return id, true
		}
	}
	return "", false
}

// putFile creates or overwrites name under parentID. Caller holds the lock.
// It reports false when the parent is gone, which a real drive answers with
// not-found: an upload can outlive the directory it was started in.
func (f *Fake) putFile(parentID, name string, data []byte) (provider.Entry, bool) {
	parent, ok := f.nodes[parentID]
	if !ok || parent.entry.Kind != provider.KindDir {
		return provider.Entry{}, false
	}
	if id, exists := parent.children[name]; exists {
		n := f.nodes[id]
		sum := sha1sum(data)
		n.data = data
		n.entry.Size = int64(len(data))
		n.entry.Version = sum
		n.entry.Hashes = provider.Hashes{provider.HashSHA1: sum}
		n.entry.ModTime = time.Now()
		f.blobs[sum] = data
		e := n.entry
		f.record(provider.Change{Op: provider.ChangeUpsert, ID: id, ParentID: parentID, Entry: &e})
		return e, true
	}
	return f.newNode(parentID, name, provider.KindFile, data), true
}

func (f *Fake) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	if err := f.enter(ctx, "Mkdir"); err != nil {
		return provider.Entry{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.nodes[parentID]
	if !ok || p.entry.Kind != provider.KindDir {
		return provider.Entry{}, provider.ErrNotFound
	}
	if _, exists := p.children[name]; exists {
		return provider.Entry{}, provider.ErrExists
	}
	if err := f.checkName(parentID, name); err != nil {
		return provider.Entry{}, err
	}
	return f.newNode(parentID, name, provider.KindDir, nil), nil
}

func (f *Fake) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	if err := f.enter(ctx, "Rename"); err != nil {
		return provider.Entry{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[id]
	if !ok || id == f.rootID {
		return provider.Entry{}, provider.ErrNotFound
	}
	p := f.nodes[n.entry.ParentID]
	if _, exists := p.children[newName]; exists {
		return provider.Entry{}, provider.ErrExists
	}
	if err := f.checkName(n.entry.ParentID, newName); err != nil {
		return provider.Entry{}, err
	}
	delete(p.children, n.entry.Name)
	n.entry.Name = newName
	newID := id
	if f.pathIDs {
		newID = f.childID(n.entry.ParentID, newName)
		f.rekeyLocked(id, newID)
	}
	p.children[newName] = newID
	e := f.nodes[newID].entry
	f.record(provider.Change{Op: provider.ChangeUpsert, ID: newID, ParentID: e.ParentID, Entry: &e})
	return e, nil
}

func (f *Fake) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	if err := f.enter(ctx, "Move"); err != nil {
		return provider.Entry{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[id]
	if !ok || id == f.rootID {
		return provider.Entry{}, provider.ErrNotFound
	}
	np, ok := f.nodes[newParentID]
	if !ok || np.entry.Kind != provider.KindDir {
		return provider.Entry{}, provider.ErrNotFound
	}
	if _, exists := np.children[n.entry.Name]; exists {
		return provider.Entry{}, provider.ErrExists
	}
	delete(f.nodes[n.entry.ParentID].children, n.entry.Name)
	n.entry.ParentID = newParentID
	newID := id
	if f.pathIDs {
		newID = f.childID(newParentID, n.entry.Name)
		f.rekeyLocked(id, newID)
	}
	np.children[f.nodes[newID].entry.Name] = newID
	e := f.nodes[newID].entry
	f.record(provider.Change{Op: provider.ChangeUpsert, ID: newID, ParentID: newParentID, Entry: &e})
	return e, nil
}

func (f *Fake) Delete(ctx context.Context, id string) error {
	if err := f.enter(ctx, "Delete"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[id]
	if !ok || id == f.rootID {
		return provider.ErrNotFound
	}
	f.deleteLocked(id, n)
	return nil
}

func (f *Fake) deleteLocked(id string, n *node) {
	for _, cid := range n.children {
		f.deleteLocked(cid, f.nodes[cid])
	}
	delete(f.nodes[n.entry.ParentID].children, n.entry.Name)
	delete(f.nodes, id)
	f.record(provider.Change{Op: provider.ChangeDelete, ID: id, ParentID: n.entry.ParentID})
}

// Changes implements provider.ChangeLister. The cursor is the index into the
// change log; "" means "from the beginning".
func (f *Fake) Changes(ctx context.Context, cursor string) ([]provider.Change, string, error) {
	if err := f.enter(ctx, "Changes"); err != nil {
		return nil, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Faults.CursorReset {
		f.Faults.CursorReset = false
		return nil, "", &provider.CursorResetError{Cursor: strconv.Itoa(len(f.changes))}
	}
	start := 0
	if cursor != "" {
		start, _ = strconv.Atoi(cursor)
	}
	if start > len(f.changes) {
		start = len(f.changes)
	}
	out := append([]provider.Change(nil), f.changes[start:]...)
	return out, strconv.Itoa(len(f.changes)), nil
}

// Cursor returns a cursor positioned after all changes so far.
func (f *Fake) Cursor() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strconv.Itoa(len(f.changes))
}

func sha1sum(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}

func splitPath(p string) []string {
	var out []string
	cur := ""
	for _, r := range p {
		if r == '/' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

var _ provider.Provider = (*Fake)(nil)
var _ provider.ChangeLister = (*Fake)(nil)

func init() {
	provider.Register("fake", func(name string, cfg map[string]any) (provider.Provider, error) {
		if key, _ := cfg["shared"].(string); key != "" {
			return Shared(key, name), nil
		}
		return New(name), nil
	})
}

var (
	sharedMu sync.Mutex
	shared   = map[string]*Fake{}
)

// Shared returns the one Fake registered under key, creating it with name on
// first use. A remote configured as `{type: fake, shared: <key>}` resolves to
// it from every daemon opened in the process, which is what a test of two
// daemons on one cache directory needs: in production both would talk to the
// same account, and a second empty in-memory backend would make the second
// daemon see an empty drive instead.
func Shared(key, name string) *Fake {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	f, ok := shared[key]
	if !ok {
		f = New(name)
		shared[key] = f
	}
	return f
}

// Content returns what the backend holds at path. Tests use it to check that
// what left the machine is what the application wrote.
func (f *Fake) Content(path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.lookupLocked(path)
	if !ok {
		return nil, false
	}
	return append([]byte(nil), f.nodes[id].data...), true
}

// IDOf resolves a slash path to the backend's id for it.
func (f *Fake) IDOf(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookupLocked(path)
}
