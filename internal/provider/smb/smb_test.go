package smb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	smb2 "github.com/hirochachacha/go-smb2"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
)

// memShare is an in-memory stand-in for a mounted SMB share. It reproduces the
// two server behaviours the driver has to work around: a rename that refuses
// an existing destination, and a remove that refuses a non-empty directory.
type memShare struct {
	mu     sync.Mutex
	nodes  map[string]*memNode
	fsInfo smb2.FileFsInfo

	opens     int
	openNames []string
	// failNext, when set, is returned by the next operation and then cleared.
	failNext error
	// readLimit caps how many bytes one ReadAt returns, which is how a real
	// server answers a read larger than its negotiated maximum.
	readLimit int
	dirBatch  int
}

func (s *memShare) Statfs(string) (smb2.FileFsInfo, error) {
	if s.fsInfo == nil {
		return nil, provider.ErrUnsupported
	}
	return s.fsInfo, nil
}

type memNode struct {
	name    string
	dir     bool
	data    []byte
	modTime time.Time
}

func newMemShare() *memShare {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s := &memShare{nodes: map[string]*memNode{
		"": {name: "", dir: true, modTime: now},
	}}
	s.mkdirRaw("data", now)
	s.mkdirRaw("data/sub", now)
	s.writeRaw("data/movie.bin", []byte("0123456789abcdef"), now)
	s.writeRaw("data/notes.txt", []byte("hello"), now)
	s.writeRaw("data/"+tempPrefix+"leftover.1", []byte("partial"), now)
	return s
}

func (s *memShare) mkdirRaw(name string, mod time.Time) {
	s.nodes[name] = &memNode{name: path.Base(name), dir: true, modTime: mod}
}

func (s *memShare) writeRaw(name string, data []byte, mod time.Time) {
	s.nodes[name] = &memNode{name: path.Base(name), data: data, modTime: mod}
}

func (s *memShare) take() error {
	err := s.failNext
	s.failNext = nil
	return err
}

func (s *memShare) Stat(name string) (os.FileInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take(); err != nil {
		return nil, err
	}
	n, ok := s.nodes[name]
	if !ok {
		return nil, &os.PathError{Op: "stat", Path: name, Err: os.ErrNotExist}
	}
	return memInfo{n}, nil
}

func (s *memShare) childrenLocked(dir string) []os.FileInfo {
	prefix := dir + "/"
	if dir == "" {
		prefix = ""
	}
	var out []os.FileInfo
	for full, n := range s.nodes {
		if full == dir || !strings.HasPrefix(full, prefix) {
			continue
		}
		if strings.Contains(strings.TrimPrefix(full, prefix), "/") {
			continue
		}
		out = append(out, memInfo{n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func (s *memShare) ReadDir(name string) ([]os.FileInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take(); err != nil {
		return nil, err
	}
	n, ok := s.nodes[name]
	if !ok {
		return nil, &os.PathError{Op: "readdir", Path: name, Err: os.ErrNotExist}
	}
	if !n.dir {
		return nil, &os.PathError{Op: "readdir", Path: name, Err: errors.New("not a directory")}
	}
	return s.childrenLocked(name), nil
}

func (s *memShare) Mkdir(name string, _ os.FileMode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take(); err != nil {
		return err
	}
	if _, ok := s.nodes[name]; ok {
		return &os.PathError{Op: "mkdir", Path: name, Err: os.ErrExist}
	}
	if _, ok := s.nodes[path.Dir(name)]; !ok && path.Dir(name) != "." {
		return &os.PathError{Op: "mkdir", Path: name, Err: os.ErrNotExist}
	}
	s.mkdirRaw(name, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	return nil
}

func (s *memShare) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take(); err != nil {
		return err
	}
	n, ok := s.nodes[name]
	if !ok {
		return &os.PathError{Op: "remove", Path: name, Err: os.ErrNotExist}
	}
	if n.dir && len(s.childrenLocked(name)) > 0 {
		// Matches SMB: a directory is only removable once it is empty.
		return &os.PathError{Op: "remove", Path: name, Err: errors.New("directory not empty")}
	}
	delete(s.nodes, name)
	return nil
}

// Rename refuses an existing destination, which is what go-smb2 does: it sends
// FileRenameInformation with ReplaceIfExists clear.
func (s *memShare) Rename(oldpath, newpath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take(); err != nil {
		return err
	}
	n, ok := s.nodes[oldpath]
	if !ok {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: os.ErrNotExist}
	}
	if _, exists := s.nodes[newpath]; exists {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: os.ErrExist}
	}
	if _, ok := s.nodes[path.Dir(newpath)]; !ok && path.Dir(newpath) != "." {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: os.ErrNotExist}
	}
	delete(s.nodes, oldpath)
	n.name = path.Base(newpath)
	s.nodes[newpath] = n
	return nil
}

func (s *memShare) Open(name string) (fileHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take(); err != nil {
		return nil, err
	}
	n, ok := s.nodes[name]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}
	s.opens++
	s.openNames = append(s.openNames, name)
	return &memFile{share: s, node: n, path: name}, nil
}

func (s *memShare) OpenFile(name string, _ int, _ os.FileMode) (fileHandle, error) {
	return s.Open(name)
}

func (s *memShare) Create(name string) (fileHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take(); err != nil {
		return nil, err
	}
	if _, ok := s.nodes[path.Dir(name)]; !ok && path.Dir(name) != "." {
		return nil, &os.PathError{Op: "create", Path: name, Err: os.ErrNotExist}
	}
	s.writeRaw(name, nil, time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC))
	s.opens++
	s.openNames = append(s.openNames, name)
	return &memFile{share: s, node: s.nodes[name], path: name}, nil
}

type memFile struct {
	share  *memShare
	node   *memNode
	path   string
	offset int
	closed bool
}

func (f *memFile) Close() error {
	f.share.mu.Lock()
	defer f.share.mu.Unlock()
	f.closed = true
	return nil
}

func (f *memFile) ReadAt(b []byte, off int64) (int, error) {
	f.share.mu.Lock()
	defer f.share.mu.Unlock()
	if f.closed {
		return 0, os.ErrClosed
	}
	if err := f.share.take(); err != nil {
		return 0, err
	}
	if off >= int64(len(f.node.data)) {
		return 0, io.EOF
	}
	n := copy(b, f.node.data[off:])
	if limit := f.share.readLimit; limit > 0 && n > limit {
		n = limit
	}
	if off+int64(n) >= int64(len(f.node.data)) {
		return n, io.EOF
	}
	return n, nil
}

func (f *memFile) WriteAt(b []byte, off int64) (int, error) {
	f.share.mu.Lock()
	defer f.share.mu.Unlock()
	if f.closed {
		return 0, os.ErrClosed
	}
	if err := f.share.take(); err != nil {
		return 0, err
	}
	need := int(off) + len(b)
	if need > len(f.node.data) {
		grown := make([]byte, need)
		copy(grown, f.node.data)
		f.node.data = grown
	}
	copy(f.node.data[off:], b)
	return len(b), nil
}

func (f *memFile) Readdir(n int) ([]os.FileInfo, error) {
	f.share.mu.Lock()
	defer f.share.mu.Unlock()
	if err := f.share.take(); err != nil {
		return nil, err
	}
	if !f.node.dir {
		return nil, &os.PathError{Op: "readdir", Path: f.path, Err: errors.New("not a directory")}
	}
	all := f.share.childrenLocked(f.path)
	if f.offset >= len(all) {
		return nil, io.EOF
	}
	batch := n
	if limit := f.share.dirBatch; limit > 0 && limit < batch {
		batch = limit
	}
	end := f.offset + batch
	if end > len(all) {
		end = len(all)
	}
	out := all[f.offset:end]
	f.offset = end
	return out, nil
}

func (f *memFile) Stat() (os.FileInfo, error) {
	f.share.mu.Lock()
	defer f.share.mu.Unlock()
	return memInfo{f.node}, nil
}

type memInfo struct{ n *memNode }

func (i memInfo) Name() string { return i.n.name }
func (i memInfo) Size() int64 {
	if i.n.dir {
		return 0
	}
	return int64(len(i.n.data))
}
func (i memInfo) Mode() os.FileMode {
	if i.n.dir {
		return os.ModeDir | 0o755
	}
	return 0o644
}
func (i memInfo) ModTime() time.Time { return i.n.modTime }
func (i memInfo) IsDir() bool        { return i.n.dir }
func (i memInfo) Sys() any           { return nil }

// memRunner hands the driver a fixed share and counts how often the session
// was dropped, which is how the reconnect path is observed.
type memRunner struct {
	share  *memShare
	drops  int
	closed bool
}

func (r *memRunner) use(ctx context.Context, _ ratelimit.Class, fn func(fileSystem) error) error {
	return runWithRetry(ctx, func(context.Context) (fileSystem, error) {
		if r.closed {
			return nil, errors.New("smb: provider is closed")
		}
		return r.share, nil
	}, func() { r.drops++ }, fn)
}

func (r *memRunner) Close() error { r.closed = true; return nil }

func newTestProvider(t *testing.T, share *memShare, opts ...func(*Options)) *Provider {
	t.Helper()
	opt := Options{Name: "nas", Root: "", PartSize: 8, share: &memRunner{share: share}}
	for _, apply := range opts {
		apply(&opt)
	}
	p, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func TestListHidesStagingFilesAndDotEntries(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	entries, next, err := p.List(context.Background(), "/data", "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "" {
		t.Fatalf("SMB has no resumable directory cursor, got %q", next)
	}
	names := map[string]provider.Kind{}
	for _, e := range entries {
		names[e.Name] = e.Kind
		if e.ParentID != "/data" {
			t.Fatalf("entry %q reports parent %q", e.Name, e.ParentID)
		}
	}
	if _, leaked := names[tempPrefix+"leftover.1"]; leaked {
		t.Fatal("a half-written staging file was listed as a real file")
	}
	want := map[string]provider.Kind{"sub": provider.KindDir, "movie.bin": provider.KindFile, "notes.txt": provider.KindFile}
	if len(names) != len(want) {
		t.Fatalf("listing returned %v, want %v", names, want)
	}
	for name, kind := range want {
		if names[name] != kind {
			t.Fatalf("entry %q is %v, want %v", name, names[name], kind)
		}
	}
}

func TestListStreamDeliversEveryBatch(t *testing.T) {
	share := newMemShare()
	share.dirBatch = 1
	for i := 0; i < 5; i++ {
		share.writeRaw(fmt.Sprintf("data/extra-%d.bin", i), []byte("x"), time.Now())
	}
	p := newTestProvider(t, share)
	seen := map[string]int{}
	if err := p.ListStream(context.Background(), "/data", func(e provider.Entry) error {
		seen[e.Name]++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 8 {
		t.Fatalf("stream visited %d entries, want 8", len(seen))
	}
	for name, count := range seen {
		if count != 1 {
			t.Fatalf("entry %q visited %d times", name, count)
		}
	}
}

func TestListStreamStopsOnVisitorError(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	sentinel := errors.New("stop")
	calls := 0
	err := p.ListStream(context.Background(), "/data", func(provider.Entry) error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) || calls != 1 {
		t.Fatalf("ListStream returned %v after %d visits", err, calls)
	}
}

func TestPathsCannotEscapeTheShareRoot(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	ctx := context.Background()
	for _, id := range []string{"/../secret", "/..", `\..\secret`, "/data/../../etc"} {
		if _, err := p.Stat(ctx, id); err == nil {
			t.Fatalf("Stat accepted escaping path %q", id)
		}
	}
}

func TestConfiguredRootConfinesEveryPath(t *testing.T) {
	share := newMemShare()
	p := newTestProvider(t, share, func(o *Options) { o.Root = "/data" })
	entries, _, err := p.List(context.Background(), "/", "")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["movie.bin"] || names["data"] {
		t.Fatalf("root listing returned %v, want the contents of the configured subdirectory", names)
	}
	// The exposed root must map onto the subdirectory, not the share root.
	e, err := p.Stat(context.Background(), "/movie.bin")
	if err != nil || e.Size != 16 {
		t.Fatalf("Stat under the configured root returned %+v, %v", e, err)
	}
}

func TestStatReportsTheRootAsADirectory(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	e, err := p.Stat(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != provider.KindDir || e.ID != RootID || e.Version == "" {
		t.Fatalf("root stat is %+v", e)
	}
}

func TestStatMapsAMissingFileToNotFound(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	if _, err := p.Stat(context.Background(), "/data/absent"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("Stat of a missing file returned %v, want ErrNotFound", err)
	}
}

func TestVersionFingerprintsSizeAndTime(t *testing.T) {
	share := newMemShare()
	p := newTestProvider(t, share)
	before, err := p.Stat(context.Background(), "/data/notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	share.mu.Lock()
	share.nodes["data/notes.txt"].data = []byte("hello world")
	share.nodes["data/notes.txt"].modTime = time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	share.mu.Unlock()
	after, err := p.Stat(context.Background(), "/data/notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	// SMB has no change token, so the fingerprint is all that keeps the block
	// cache from serving the old bytes under the new content.
	if before.Version == after.Version {
		t.Fatal("an edited file kept its version")
	}
}

func TestReadRangeReturnsTheRequestedWindow(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	body, err := p.ReadRange(context.Background(), "/data/movie.bin", "v1", 4, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if string(got) != "456789" {
		t.Fatalf("read %q, want %q", got, "456789")
	}
}

func TestReadRangeToEndOfFile(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	body, err := p.ReadRange(context.Background(), "/data/movie.bin", "v1", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if string(got) != "abcdef" {
		t.Fatalf("read %q, want %q", got, "abcdef")
	}
}

func TestShortReadsAreFollowedUpRatherThanTreatedAsEOF(t *testing.T) {
	share := newMemShare()
	// A server that caps a read below the request answers with fewer bytes.
	share.readLimit = 3
	p := newTestProvider(t, share)
	buf := make([]byte, 16)
	n, err := p.ReadRangeAt(context.Background(), "/data/movie.bin", "v1", 0, buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 16 || string(buf[:n]) != "0123456789abcdef" {
		t.Fatalf("read %d bytes %q; a capped read must be continued, not reported short", n, buf[:n])
	}
}

func TestReadsReuseOneHandle(t *testing.T) {
	share := newMemShare()
	p := newTestProvider(t, share)
	ctx := context.Background()
	buf := make([]byte, 4)
	for i := 0; i < 5; i++ {
		if _, err := p.ReadRangeAt(ctx, "/data/movie.bin", "v1", int64(i), buf); err != nil {
			t.Fatal(err)
		}
	}
	opens, _ := p.HandleStats()
	if opens != 1 {
		t.Fatalf("five reads opened %d handles; opening an SMB file is a full round trip", opens)
	}
}

func TestAHandleIsReplacedWhenTheVersionChanges(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	ctx := context.Background()
	buf := make([]byte, 4)
	if _, err := p.ReadRangeAt(ctx, "/data/movie.bin", "v1", 0, buf); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadRangeAt(ctx, "/data/movie.bin", "v2", 0, buf); err != nil {
		t.Fatal(err)
	}
	opens, closes := p.HandleStats()
	if opens != 2 || closes != 1 {
		t.Fatalf("version change produced %d opens and %d closes; the stale handle must be replaced", opens, closes)
	}
}

func TestPublishingAnUploadDropsTheCachedHandle(t *testing.T) {
	share := newMemShare()
	p := newTestProvider(t, share)
	ctx := context.Background()
	buf := make([]byte, 4)
	if _, err := p.ReadRangeAt(ctx, "/data/notes.txt", "v1", 0, buf); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PutFile(ctx, "/data", "notes.txt", strings.NewReader("rewritten"), 9, nil); err != nil {
		t.Fatal(err)
	}
	_, closes := p.HandleStats()
	if closes != 1 {
		t.Fatalf("replacing a file left %d handles closed; the server refuses to unlink a name we hold open", closes)
	}
}

func TestPutFileNeverExposesAPartialFileUnderTheFinalName(t *testing.T) {
	share := newMemShare()
	p := newTestProvider(t, share)
	if _, err := p.PutFile(context.Background(), "/data", "fresh.bin", strings.NewReader("abcdef"), 6, nil); err != nil {
		t.Fatal(err)
	}
	share.mu.Lock()
	defer share.mu.Unlock()
	node, ok := share.nodes["data/fresh.bin"]
	if !ok || string(node.data) != "abcdef" {
		t.Fatalf("published file is %v", node)
	}
	for name := range share.nodes {
		if strings.Contains(name, tempPrefix) && !strings.Contains(name, "leftover") {
			t.Fatalf("staging file %q was left behind", name)
		}
	}
}

func TestPutFileReplacesAnExistingNameDespiteRenameRefusing(t *testing.T) {
	share := newMemShare()
	p := newTestProvider(t, share)
	e, err := p.PutFile(context.Background(), "/data", "notes.txt", strings.NewReader("replacement"), 11, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Size != 11 {
		t.Fatalf("published entry is %+v", e)
	}
	share.mu.Lock()
	defer share.mu.Unlock()
	if got := string(share.nodes["data/notes.txt"].data); got != "replacement" {
		t.Fatalf("file holds %q; the publish must unlink before renaming because SMB rename does not replace", got)
	}
}

func TestPutFileRejectsAShortBody(t *testing.T) {
	share := newMemShare()
	p := newTestProvider(t, share)
	if _, err := p.PutFile(context.Background(), "/data", "short.bin", strings.NewReader("ab"), 9, nil); err == nil {
		t.Fatal("PutFile accepted a body shorter than the declared size")
	}
	share.mu.Lock()
	defer share.mu.Unlock()
	if _, ok := share.nodes["data/short.bin"]; ok {
		t.Fatal("a rejected upload still published a file")
	}
}

func TestChunkedUploadWritesEachPartAtItsOffset(t *testing.T) {
	share := newMemShare()
	p := newTestProvider(t, share)
	ctx := context.Background()
	payload := "0123456789ABCDEFGHIJ"
	s, err := p.BeginUpload(ctx, "/data", "big.bin", int64(len(payload)), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Out of order on purpose: parts carry their own offset, so a resumed
	// upload may send whatever is still missing.
	order := []int{2, 0, 1}
	var parts []provider.PartToken
	for _, idx := range order {
		off := int64(idx) * s.PartSize
		end := off + s.PartSize
		if end > int64(len(payload)) {
			end = int64(len(payload))
		}
		token, err := p.UploadPart(ctx, s, idx, strings.NewReader(payload[off:end]), end-off)
		if err != nil {
			t.Fatalf("part %d: %v", idx, err)
		}
		parts = append(parts, token)
	}
	e, err := p.CompleteUpload(ctx, s, parts)
	if err != nil {
		t.Fatal(err)
	}
	if e.Size != int64(len(payload)) {
		t.Fatalf("published size is %d, want %d", e.Size, len(payload))
	}
	share.mu.Lock()
	defer share.mu.Unlock()
	if got := string(share.nodes["data/big.bin"].data); got != payload {
		t.Fatalf("published content is %q, want %q", got, payload)
	}
}

func TestUploadPartRejectsAChunkLargerThanThePartSize(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	ctx := context.Background()
	s, err := p.BeginUpload(ctx, "/data", "big.bin", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UploadPart(ctx, s, 0, strings.NewReader(strings.Repeat("x", 64)), 64); err == nil {
		t.Fatal("UploadPart accepted a chunk above the part size; parts are addressed by index times part size")
	}
}

func TestCompleteUploadRefusesAStagedFileOfTheWrongSize(t *testing.T) {
	share := newMemShare()
	p := newTestProvider(t, share)
	ctx := context.Background()
	s, err := p.BeginUpload(ctx, "/data", "big.bin", 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := p.UploadPart(ctx, s, 0, strings.NewReader("01234567"), 8)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.CompleteUpload(ctx, s, []provider.PartToken{token})
	if !errors.Is(err, provider.ErrTransient) {
		t.Fatalf("CompleteUpload returned %v; a truncated staging file must not be published", err)
	}
	share.mu.Lock()
	defer share.mu.Unlock()
	if _, ok := share.nodes["data/big.bin"]; ok {
		t.Fatal("a truncated upload was published anyway")
	}
}

func TestAbortUploadRemovesTheStagingFileAndToleratesItsAbsence(t *testing.T) {
	share := newMemShare()
	p := newTestProvider(t, share)
	ctx := context.Background()
	s, err := p.BeginUpload(ctx, "/data", "big.bin", 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AbortUpload(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := p.AbortUpload(ctx, s); err != nil {
		t.Fatalf("aborting twice returned %v; a missing staging file is already the desired state", err)
	}
	share.mu.Lock()
	defer share.mu.Unlock()
	for name := range share.nodes {
		if strings.Contains(name, "big.bin") {
			t.Fatalf("staging file %q survived the abort", name)
		}
	}
}

func TestMkdirReportsAnExistingName(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	if _, err := p.Mkdir(context.Background(), "/data", "sub"); !errors.Is(err, provider.ErrExists) {
		t.Fatalf("Mkdir over an existing directory returned %v, want ErrExists", err)
	}
}

func TestMkdirCreatesADirectory(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	e, err := p.Mkdir(context.Background(), "/data", "new")
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != provider.KindDir || e.ID != "/data/new" || e.ParentID != "/data" {
		t.Fatalf("Mkdir returned %+v", e)
	}
}

func TestRenameAndMoveKeepTheEntryIdentityConsistent(t *testing.T) {
	share := newMemShare()
	p := newTestProvider(t, share)
	ctx := context.Background()
	e, err := p.Rename(ctx, "/data/notes.txt", "renamed.txt")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "/data/renamed.txt" || e.ParentID != "/data" {
		t.Fatalf("rename returned %+v", e)
	}
	e, err = p.Move(ctx, "/data/renamed.txt", "/data/sub")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "/data/sub/renamed.txt" || e.ParentID != "/data/sub" {
		t.Fatalf("move returned %+v", e)
	}
}

func TestMoveDoesNotClobberAnExistingDestination(t *testing.T) {
	share := newMemShare()
	share.writeRaw("data/sub/notes.txt", []byte("existing"), time.Now())
	p := newTestProvider(t, share)
	// Publishing an upload unlinks first by design; a move must not, or it
	// would silently destroy an unrelated file.
	if _, err := p.Move(context.Background(), "/data/notes.txt", "/data/sub"); !errors.Is(err, provider.ErrExists) {
		t.Fatalf("Move returned %v, want ErrExists", err)
	}
	share.mu.Lock()
	defer share.mu.Unlock()
	if got := string(share.nodes["data/sub/notes.txt"].data); got != "existing" {
		t.Fatalf("destination now holds %q; the move overwrote it", got)
	}
}

func TestRenameAndMoveRefuseTheRoot(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	ctx := context.Background()
	if _, err := p.Rename(ctx, "/", "x"); err == nil {
		t.Fatal("Rename accepted the share root")
	}
	if _, err := p.Move(ctx, "/", "/data"); err == nil {
		t.Fatal("Move accepted the share root")
	}
}

func TestDeleteRemovesADirectoryTreeDepthFirst(t *testing.T) {
	share := newMemShare()
	share.writeRaw("data/sub/a.txt", []byte("a"), time.Now())
	share.mkdirRaw("data/sub/deep", time.Now())
	share.writeRaw("data/sub/deep/b.txt", []byte("b"), time.Now())
	p := newTestProvider(t, share)
	if err := p.Delete(context.Background(), "/data/sub"); err != nil {
		t.Fatal(err)
	}
	share.mu.Lock()
	defer share.mu.Unlock()
	for name := range share.nodes {
		if strings.HasPrefix(name, "data/sub") {
			t.Fatalf("%q survived the recursive delete", name)
		}
	}
	if _, ok := share.nodes["data/movie.bin"]; !ok {
		t.Fatal("the recursive delete reached outside its subtree")
	}
}

func TestDeleteRefusesTheShareRoot(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	if err := p.Delete(context.Background(), "/"); err == nil {
		t.Fatal("Delete accepted the share root")
	}
}

func TestDeleteOfAMissingPathIsNotFound(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	if err := p.Delete(context.Background(), "/data/absent"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("Delete of a missing path returned %v, want ErrNotFound", err)
	}
}

func TestRecursiveDeleteIsDepthBounded(t *testing.T) {
	share := newMemShare()
	dir := "data/deep"
	share.mkdirRaw(dir, time.Now())
	for i := 0; i <= maxDeleteDepth+2; i++ {
		dir += "/d"
		share.mkdirRaw(dir, time.Now())
	}
	p := newTestProvider(t, share)
	err := p.Delete(context.Background(), "/data/deep")
	if err == nil || !strings.Contains(err.Error(), "nesting exceeds") {
		t.Fatalf("a pathologically deep tree returned %v, want a bounded failure", err)
	}
}

func TestUnsafeNamesNeverReachTheServer(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	ctx := context.Background()
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "a:b", "a*b", "a?b", `a"b`, "a<b", "a>b", "a|b", "nul\x00"} {
		if _, err := p.Mkdir(ctx, "/data", name); err == nil {
			t.Fatalf("Mkdir accepted unsafe name %q", name)
		}
		if _, err := p.PutFile(ctx, "/data", name, strings.NewReader(""), 0, nil); err == nil {
			t.Fatalf("PutFile accepted unsafe name %q", name)
		}
	}
}

func TestADroppedConnectionIsRetriedOnce(t *testing.T) {
	share := newMemShare()
	runner := &memRunner{share: share}
	p, err := New(Options{Name: "nas", share: runner})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// The first attempt fails the way a dead session does; the second must run
	// on a fresh mount rather than replaying onto the broken one.
	share.failNext = errors.New("connection error: broken pipe")
	if _, err := p.Stat(context.Background(), "/data/notes.txt"); err != nil {
		t.Fatalf("a dropped connection was not retried: %v", err)
	}
	if runner.drops != 1 {
		t.Fatalf("session was dropped %d times, want exactly 1", runner.drops)
	}
}

func TestAPersistentFailureIsNotRetriedForever(t *testing.T) {
	attempts := 0
	err := runWithRetry(context.Background(), func(context.Context) (fileSystem, error) {
		return newMemShare(), nil
	}, func() {}, func(fileSystem) error {
		attempts++
		return errors.New("connection error: broken pipe")
	})
	if err == nil {
		t.Fatal("a link that stays broken eventually has to fail")
	}
	if attempts != 2 {
		t.Fatalf("fn ran %d times, want 2", attempts)
	}
}

func TestACancelledContextIsNotRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempts := 0
	err := runWithRetry(ctx, func(context.Context) (fileSystem, error) {
		return newMemShare(), nil
	}, func() { t.Fatal("a cancelled call must not drop the session") }, func(fileSystem) error {
		attempts++
		return errors.New("connection error: broken pipe")
	})
	if err == nil || attempts != 1 {
		t.Fatalf("cancelled call ran %d times and returned %v", attempts, err)
	}
}

func TestErrorsAreClassifiedForRetryAndBreaker(t *testing.T) {
	cases := []struct {
		err  error
		want error
	}{
		{&os.PathError{Op: "stat", Path: "x", Err: os.ErrNotExist}, provider.ErrNotFound},
		{&os.LinkError{Op: "rename", Old: "a", New: "b", Err: os.ErrExist}, provider.ErrExists},
		{&os.PathError{Op: "open", Path: "x", Err: os.ErrPermission}, provider.ErrAuth},
		{errors.New("connection error: use of closed network connection"), provider.ErrTransient},
		{io.ErrUnexpectedEOF, provider.ErrTransient},
	}
	for _, tc := range cases {
		if got := mapErr(tc.err); !errors.Is(got, tc.want) {
			t.Fatalf("%v mapped to %v, want %v", tc.err, got, tc.want)
		}
	}
	if mapErr(nil) != nil {
		t.Fatal("mapErr invented an error")
	}
}

func TestCapabilitiesMatchWhatSMBCanActuallyDo(t *testing.T) {
	p := newTestProvider(t, newMemShare())
	caps := p.Capabilities()
	if len(caps.HashTypes) != 0 {
		t.Fatal("SMB computes no content hash; advertising one would make the uploader wait for a value that never arrives")
	}
	if caps.Delta {
		t.Fatal("SMB has no change feed; claiming delta would stop directories being refreshed by TTL")
	}
	if caps.ServerCopy {
		t.Fatal("the client library exposes no server-side copy")
	}
	if !caps.PathIDs {
		t.Fatal("an SMB id is a path, so renaming a directory changes every id beneath it; the VFS needs to be told")
	}
	if caps.LinkShareable {
		t.Fatal("an SMB URL would carry this process's session")
	}
	if !caps.StreamList || !caps.RangeRead {
		t.Fatal("streaming enumeration and range reads are both implemented and must be advertised")
	}
}

func TestNewRejectsUnusableConfiguration(t *testing.T) {
	cases := []struct {
		name string
		opt  Options
	}{
		{"no host", Options{Share: "public", User: "u"}},
		{"no share", Options{Host: "nas", User: "u"}},
		{"no user", Options{Host: "nas", Share: "public"}},
		{"share with a server prefix", Options{Host: "nas", Share: `\\nas\public`, User: "u"}},
		{"root escaping the share", Options{Host: "nas", Share: "public", User: "u", Root: "../other"}},
		{"port out of range", Options{Host: "nas", Share: "public", User: "u", Port: 70000}},
		{"newline in host", Options{Host: "nas\nx", Share: "public", User: "u"}},
		{"part size too large", Options{Host: "nas", Share: "public", User: "u", PartSize: 2 << 30}},
	}
	for _, tc := range cases {
		if _, err := New(tc.opt); err == nil {
			t.Fatalf("New accepted %s", tc.name)
		}
	}
}

func TestFactoryParsesTheRemoteBlock(t *testing.T) {
	dialed := false
	dial := provider.DialFunc(func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("not used")
	})
	built, err := Factory("nas", map[string]any{
		"host": "server.local:4450", "share": "public", "user": "u", "password": "p",
		"root": "/media", "part_size": "1MiB", "concurrency": "8", "idle_timeout": "30s",
		provider.ConfigDialer: dial,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := built.(*Provider)
	if p.root != "media" {
		t.Fatalf("root parsed as %q", p.root)
	}
	if p.caps.PartSize != 1<<20 || p.caps.UploadParallel != 8 {
		t.Fatalf("caps are %+v", p.caps)
	}
	s, ok := p.share.(*session)
	if !ok {
		t.Fatal("factory did not build a live session")
	}
	if s.addr != "server.local:4450" {
		t.Fatalf("address parsed as %q; a host:port in the host field must be honoured", s.addr)
	}
	// The dialler must be the proxy-aware one: SMB is not HTTP, so it cannot
	// inherit routing through the shared HTTP client.
	if s.opt.Dial == nil {
		t.Fatal("factory dropped the proxy-aware dialler")
	}
	_, _ = s.opt.Dial(context.Background(), "tcp", "x")
	if !dialed {
		t.Fatal("the injected dialler was not the one stored")
	}
}

func TestFactoryRejectsBadValues(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"host": "nas", "share": "public", "user": "u"}
	}
	cases := map[string]string{
		"port": "0", "part_size": "banana", "concurrency": "-1",
		"idle_timeout": "soon", "ntlm_hash": "abcd",
	}
	for key, value := range cases {
		cfg := base()
		cfg[key] = value
		if _, err := Factory("nas", cfg); err == nil {
			t.Fatalf("factory accepted %s=%q", key, value)
		}
	}
}

func TestFactoryConflictingPortsAreRejected(t *testing.T) {
	if _, err := Factory("nas", map[string]any{
		"host": "server:445", "port": "4450", "share": "public", "user": "u",
	}); err == nil {
		t.Fatal("factory accepted a host port that disagrees with the port field")
	}
}

func TestRegisteredUnderItsOwnType(t *testing.T) {
	found := false
	for _, typ := range provider.Types() {
		if typ == "smb" {
			found = true
		}
	}
	if !found {
		t.Fatal("smb is not registered; main.go's blank import would link nothing")
	}
}

func TestAFullShareIsClassifiedAsQuota(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"ntstatus", &os.PathError{Op: "write", Path: "/one.txt", Err: &smb2.ResponseError{Code: statusDiskFull}}},
		{"errno", &os.PathError{Op: "write", Path: "/one.txt", Err: syscall.ENOSPC}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapErr(tc.err)
			if !errors.Is(got, provider.ErrQuotaExceeded) {
				t.Fatalf("%v mapped to %v, want ErrQuotaExceeded", tc.err, got)
			}
			if c := retry.Classify(got); c != retry.ClassQuota {
				t.Fatalf("Classify = %v, want quota: a full share must not be retried", c)
			}
			// Re-mapping an already mapped error must not downgrade it.
			if c := retry.Classify(mapErr(got)); c != retry.ClassQuota {
				t.Fatalf("re-mapped Classify = %v, want quota", c)
			}
		})
	}
	// Another NTSTATUS stays whatever it was.
	if errors.Is(mapErr(&smb2.ResponseError{Code: 0xC0000022}), provider.ErrQuotaExceeded) {
		t.Fatal("STATUS_ACCESS_DENIED must not be read as a full share")
	}
}
