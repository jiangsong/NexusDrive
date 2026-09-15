package webdavsrv

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
)

// memBackend is an in-memory stand-in for the VFS write surface. It keeps the
// same shape the real one has — inode-addressed parents, offset-addressed
// writes, commit on release — so the adapter is exercised the way it will run,
// not against a path-keyed map that would hide a parent-resolution bug.
type memBackend struct {
	mu     sync.Mutex
	nodes  map[uint64]*memNode
	byPath map[string]uint64
	nextID uint64

	copies      int
	streamed    int
	link        provider.Link
	linkErr     error
	readOnlyFor map[string]bool
	// origins records the vfs origin name each Mkdir arrived with, so a test
	// can prove the adapter tags the contexts it hands the VFS.
	origins []string
}

type memNode struct {
	ino     uint64
	name    string
	parent  uint64
	isDir   bool
	data    []byte
	version int
	mtime   time.Time
	// staged holds the bytes of an open write until it is released, which is
	// where the real VFS commits.
	staged  []byte
	writers int
}

func newMemBackend() *memBackend {
	b := &memBackend{
		nodes: map[uint64]*memNode{}, byPath: map[string]uint64{}, nextID: 1,
		readOnlyFor: map[string]bool{},
	}
	b.mkdirRaw("/", "")
	b.mkdirRaw("/share", "share")
	b.mkdirRaw("/share/sub", "sub")
	b.writeRaw("/share/movie.txt", "movie.txt", []byte("0123456789"))
	b.writeRaw("/share/sub/inner.txt", "inner.txt", []byte("inner"))
	return b
}

func (b *memBackend) alloc(p, name string, isDir bool, data []byte) *memNode {
	b.nextID++
	parent := uint64(0)
	if p != "/" {
		parent = b.byPath[path.Dir(p)]
	}
	n := &memNode{ino: b.nextID, name: name, parent: parent, isDir: isDir,
		data: data, version: 1, mtime: time.Unix(1000, 0)}
	b.nodes[n.ino] = n
	b.byPath[p] = n.ino
	return n
}

func (b *memBackend) mkdirRaw(p, name string) { b.alloc(p, name, true, nil) }

func (b *memBackend) writeRaw(p, name string, data []byte) { b.alloc(p, name, false, data) }

func (b *memBackend) pathOf(ino uint64) string {
	for p, id := range b.byPath {
		if id == ino {
			return p
		}
	}
	return ""
}

func (b *memBackend) attrLocked(p string, n *memNode) vfs.Attr {
	size := int64(len(n.data))
	if n.isDir {
		size = 0
	}
	return vfs.Attr{
		Ino: n.ino, Name: n.name, IsDir: n.isDir, Size: size, MTime: n.mtime,
		Remote: "demo", Version: fmt.Sprintf("v%d", n.version),
	}
}

func (b *memBackend) StatPath(_ context.Context, p string) (vfs.Attr, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ino, ok := b.byPath[p]
	if !ok {
		return vfs.Attr{}, vfs.ErrNotFound
	}
	return b.attrLocked(p, b.nodes[ino]), nil
}

func (b *memBackend) ReadDirPath(_ context.Context, p string) ([]vfs.Attr, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ino, ok := b.byPath[p]
	if !ok {
		return nil, vfs.ErrNotFound
	}
	if !b.nodes[ino].isDir {
		return nil, vfs.ErrNotDir
	}
	var out []vfs.Attr
	for childPath, childIno := range b.byPath {
		n := b.nodes[childIno]
		if n.parent != ino || childPath == p {
			continue
		}
		out = append(out, b.attrLocked(childPath, n))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (b *memBackend) Open(_ context.Context, ino uint64, write bool) (*vfs.Handle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.nodes[ino]
	if !ok {
		return nil, vfs.ErrNotFound
	}
	if n.isDir {
		return nil, vfs.ErrIsDir
	}
	if write {
		if b.readOnlyFor[b.pathOf(ino)] {
			return nil, vfs.ErrReadOnly
		}
		n.writers++
		n.staged = append([]byte(nil), n.data...)
	}
	return &vfs.Handle{Ino: ino}, nil
}

func (b *memBackend) Read(_ context.Context, h *vfs.Handle, p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.nodes[h.Ino]
	if !ok {
		return 0, vfs.ErrNotFound
	}
	b.streamed++
	if off >= int64(len(n.data)) {
		return 0, io.EOF
	}
	return copy(p, n.data[off:]), nil
}

func (b *memBackend) Write(_ context.Context, h *vfs.Handle, p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.nodes[h.Ino]
	if !ok {
		return 0, vfs.ErrNotFound
	}
	need := int(off) + len(p)
	if need > len(n.staged) {
		grown := make([]byte, need)
		copy(grown, n.staged)
		n.staged = grown
	}
	copy(n.staged[off:], p)
	return len(p), nil
}

func (b *memBackend) Truncate(_ context.Context, h *vfs.Handle, size int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.nodes[h.Ino]
	if !ok {
		return vfs.ErrNotFound
	}
	if size < int64(len(n.staged)) {
		n.staged = n.staged[:size]
	}
	return nil
}

// Release commits a staged write, mirroring the VFS: the bytes only become the
// file's content when the handle closes.
func (b *memBackend) Release(_ context.Context, h *vfs.Handle) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.nodes[h.Ino]
	if !ok {
		return nil
	}
	if n.writers > 0 {
		n.writers--
		n.data = n.staged
		n.staged = nil
		n.version++
		n.mtime = n.mtime.Add(time.Second)
	}
	return nil
}

func (b *memBackend) HandleAttr(_ context.Context, h *vfs.Handle) vfs.Attr {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.nodes[h.Ino]
	if !ok {
		return vfs.Attr{}
	}
	return b.attrLocked(b.pathOf(h.Ino), n)
}

func (b *memBackend) Create(_ context.Context, parent uint64, name string) (*vfs.Handle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	parentPath := b.pathOf(parent)
	if parentPath == "" {
		return nil, vfs.ErrNotFound
	}
	if b.readOnlyFor[parentPath] {
		return nil, vfs.ErrReadOnly
	}
	full := path.Join(parentPath, name)
	if _, exists := b.byPath[full]; exists {
		return nil, vfs.ErrExists
	}
	n := b.alloc(full, name, false, nil)
	n.writers++
	n.staged = nil
	return &vfs.Handle{Ino: n.ino}, nil
}

func (b *memBackend) Mkdir(ctx context.Context, parent uint64, name string) (vfs.Attr, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.origins = append(b.origins, vfs.OriginName(ctx))
	parentPath := b.pathOf(parent)
	if parentPath == "" {
		return vfs.Attr{}, vfs.ErrNotFound
	}
	if b.readOnlyFor[parentPath] {
		return vfs.Attr{}, vfs.ErrReadOnly
	}
	full := path.Join(parentPath, name)
	if _, exists := b.byPath[full]; exists {
		return vfs.Attr{}, vfs.ErrExists
	}
	n := b.alloc(full, name, true, nil)
	return b.attrLocked(full, n), nil
}

func (b *memBackend) Remove(_ context.Context, parent uint64, name string, recursive bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	parentPath := b.pathOf(parent)
	if parentPath == "" {
		return vfs.ErrNotFound
	}
	full := path.Join(parentPath, name)
	ino, ok := b.byPath[full]
	if !ok {
		return vfs.ErrNotFound
	}
	if b.nodes[ino].isDir {
		children := 0
		for p := range b.byPath {
			if strings.HasPrefix(p, full+"/") {
				children++
			}
		}
		if children > 0 && !recursive {
			return vfs.ErrNotDir
		}
		for p := range b.byPath {
			if strings.HasPrefix(p, full+"/") {
				delete(b.nodes, b.byPath[p])
				delete(b.byPath, p)
			}
		}
	}
	delete(b.nodes, ino)
	delete(b.byPath, full)
	return nil
}

func (b *memBackend) Rename(_ context.Context, oldParent uint64, oldName string, newParent uint64, newName string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	oldParentPath, newParentPath := b.pathOf(oldParent), b.pathOf(newParent)
	if oldParentPath == "" || newParentPath == "" {
		return vfs.ErrNotFound
	}
	from, to := path.Join(oldParentPath, oldName), path.Join(newParentPath, newName)
	ino, ok := b.byPath[from]
	if !ok {
		return vfs.ErrNotFound
	}
	if _, exists := b.byPath[to]; exists {
		return vfs.ErrExists
	}
	moved := map[string]string{from: to}
	for p := range b.byPath {
		if strings.HasPrefix(p, from+"/") {
			moved[p] = to + strings.TrimPrefix(p, from)
		}
	}
	for src, dst := range moved {
		id := b.byPath[src]
		delete(b.byPath, src)
		b.byPath[dst] = id
	}
	n := b.nodes[ino]
	n.name = newName
	n.parent = newParent
	return nil
}

// Copy is the server-side primitive: it never reads bytes through Read, which
// is what the adapter test asserts.
func (b *memBackend) Copy(_ context.Context, src, dst string) (vfs.Attr, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	srcIno, ok := b.byPath[src]
	if !ok {
		return vfs.Attr{}, vfs.ErrNotFound
	}
	if b.nodes[srcIno].isDir {
		return vfs.Attr{}, vfs.ErrIsDir
	}
	if _, exists := b.byPath[dst]; exists {
		return vfs.Attr{}, vfs.ErrExists
	}
	if _, exists := b.byPath[path.Dir(dst)]; !exists {
		return vfs.Attr{}, vfs.ErrNotFound
	}
	b.copies++
	n := b.alloc(dst, path.Base(dst), false, append([]byte(nil), b.nodes[srcIno].data...))
	return b.attrLocked(dst, n), nil
}

func (b *memBackend) DownloadURL(context.Context, string) (provider.Link, error) {
	return b.link, b.linkErr
}

func (b *memBackend) content(p string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ino, ok := b.byPath[p]
	if !ok {
		return "", false
	}
	return string(b.nodes[ino].data), true
}

func (b *memBackend) exists(p string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.byPath[p]
	return ok
}

type davClient struct {
	t    *testing.T
	base string
}

func (c *davClient) do(method, target string, body string, headers map[string]string) *http.Response {
	c.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+target, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp
}

func (c *davClient) status(method, target string, body string, headers map[string]string) int {
	c.t.Helper()
	resp := c.do(method, target, body, headers)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func startWritable(t *testing.T, backend *memBackend, writable bool) *davClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	running, err := Start(ctx, Options{
		FS: backend, Addr: "127.0.0.1:0", Prefix: "/dav", Root: "/share", Writable: writable,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { running.Close() })
	return &davClient{t: t, base: "http://" + running.Addr() + "/dav"}
}

func TestWritableOptionsAdvertisesTheWriteVerbsAndLocking(t *testing.T) {
	c := startWritable(t, newMemBackend(), true)
	resp := c.do(http.MethodOptions, "/", "", nil)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	allow := resp.Header.Get("Allow")
	for _, verb := range []string{"PUT", "DELETE", "MKCOL", "MOVE", "COPY", "LOCK", "UNLOCK"} {
		if !strings.Contains(allow, verb) {
			t.Fatalf("Allow %q is missing %s", allow, verb)
		}
	}
	if resp.Header.Get("DAV") != "1, 2" {
		t.Fatalf("DAV class is %q; locking is served, so class 2 must be claimed", resp.Header.Get("DAV"))
	}
}

func TestReadOnlyEndpointStillRefusesEveryWriteVerb(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, false)
	cases := []struct {
		method, target string
		headers        map[string]string
	}{
		{http.MethodPut, "/new.txt", nil},
		{http.MethodDelete, "/movie.txt", nil},
		{"MKCOL", "/newdir", nil},
		{"MOVE", "/movie.txt", map[string]string{"Destination": "/dav/moved.txt"}},
		{"COPY", "/movie.txt", map[string]string{"Destination": "/dav/copied.txt"}},
		{"PROPPATCH", "/movie.txt", nil},
		{"LOCK", "/movie.txt", nil},
		{"UNLOCK", "/movie.txt", nil},
	}
	for _, tc := range cases {
		if got := c.status(tc.method, tc.target, "", tc.headers); got != http.StatusMethodNotAllowed {
			t.Fatalf("%s returned %d on a read-only endpoint, want 405", tc.method, got)
		}
	}
	if !backend.exists("/share/movie.txt") {
		t.Fatal("a refused method still changed the tree")
	}
}

func TestPutCreatesAndReplaces(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)

	if got := c.status(http.MethodPut, "/notes.txt", "first version", nil); got != http.StatusCreated {
		t.Fatalf("PUT of a new file returned %d, want 201", got)
	}
	if body, ok := backend.content("/share/notes.txt"); !ok || body != "first version" {
		t.Fatalf("stored content is %q (present=%v)", body, ok)
	}

	// A shorter replacement must not leave the tail of the previous content.
	if got := c.status(http.MethodPut, "/notes.txt", "second", nil); got != http.StatusCreated &&
		got != http.StatusNoContent && got != http.StatusOK {
		t.Fatalf("PUT over an existing file returned %d", got)
	}
	if body, _ := backend.content("/share/notes.txt"); body != "second" {
		t.Fatalf("replacement left %q; the open must truncate", body)
	}
}

func TestPutIsVisibleToAFollowingGet(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	if got := c.status(http.MethodPut, "/roundtrip.txt", "written over DAV", nil); got != http.StatusCreated {
		t.Fatalf("PUT returned %d", got)
	}
	resp := c.do(http.MethodGet, "/roundtrip.txt", "", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "written over DAV" {
		t.Fatalf("GET after PUT = %d %q", resp.StatusCode, body)
	}
}

func TestPutDoesNotEmitAnETagForUncommittedBytes(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	resp := c.do(http.MethodPut, "/movie.txt", "brand new content", nil)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// The handler asks for the ETag before the write commits. Answering with
	// the previous version's validator would label new bytes as the old file.
	if etag := resp.Header.Get("ETag"); etag != "" {
		t.Fatalf("PUT returned ETag %q for content that had not been committed yet", etag)
	}
	// Once committed, a GET must carry a validator again.
	resp = c.do(http.MethodGet, "/movie.txt", "", nil)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.Header.Get("ETag") == "" {
		t.Fatal("a committed file must still carry an ETag")
	}
}

func TestPutOntoAReadOnlyMountIsForbidden(t *testing.T) {
	backend := newMemBackend()
	backend.readOnlyFor["/share"] = true
	c := startWritable(t, backend, true)
	if got := c.status(http.MethodPut, "/blocked.txt", "nope", nil); got != http.StatusForbidden {
		t.Fatalf("PUT onto a read-only mount returned %d, want 403", got)
	}
	if backend.exists("/share/blocked.txt") {
		t.Fatal("a refused PUT still created the file")
	}
}

func TestMkcolCreatesACollection(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	if got := c.status("MKCOL", "/fresh", "", nil); got != http.StatusCreated {
		t.Fatalf("MKCOL returned %d, want 201", got)
	}
	if !backend.exists("/share/fresh") {
		t.Fatal("MKCOL did not create the collection")
	}
	// A second MKCOL on the same name is a conflict, not a silent success.
	if got := c.status("MKCOL", "/fresh", "", nil); got != http.StatusMethodNotAllowed && got != http.StatusConflict {
		t.Fatalf("MKCOL over an existing collection returned %d", got)
	}
}

func TestDeleteRemovesASubtree(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	if got := c.status(http.MethodDelete, "/sub", "", nil); got != http.StatusNoContent {
		t.Fatalf("DELETE of a collection returned %d, want 204", got)
	}
	if backend.exists("/share/sub") || backend.exists("/share/sub/inner.txt") {
		t.Fatal("DELETE left part of the subtree behind")
	}
	if got := c.status(http.MethodDelete, "/absent", "", nil); got != http.StatusNotFound {
		t.Fatalf("DELETE of a missing path returned %d, want 404", got)
	}
}

func TestDeleteRefusesTheExportedRoot(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	// The root of the DAV namespace has no parent inside it; removing it would
	// take the whole exported subtree with it.
	if got := c.status(http.MethodDelete, "/", "", nil); got != http.StatusForbidden {
		t.Fatalf("DELETE of the exported root returned %d, want 403", got)
	}
	if !backend.exists("/share") {
		t.Fatal("the exported root was removed")
	}
}

func TestMoveRenamesWithinTheNamespace(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	got := c.status("MOVE", "/movie.txt", "", map[string]string{"Destination": "/dav/sub/moved.txt"})
	if got != http.StatusCreated {
		t.Fatalf("MOVE returned %d, want 201", got)
	}
	if backend.exists("/share/movie.txt") {
		t.Fatal("MOVE left the source behind")
	}
	if body, ok := backend.content("/share/sub/moved.txt"); !ok || body != "0123456789" {
		t.Fatalf("moved file holds %q (present=%v)", body, ok)
	}
}

func TestMoveOutsideTheNamespaceIsRefused(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	for _, destination := range []string{"/elsewhere/escaped.txt", "/davish/escaped.txt"} {
		got := c.status("MOVE", "/movie.txt", "", map[string]string{"Destination": destination})
		if got != http.StatusForbidden && got != http.StatusBadGateway {
			t.Fatalf("MOVE to %q returned %d, want a refusal", destination, got)
		}
	}
	if !backend.exists("/share/movie.txt") {
		t.Fatal("a refused MOVE still moved the file")
	}
}

func TestCopyUsesTheServerSidePrimitive(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	got := c.status("COPY", "/movie.txt", "", map[string]string{"Destination": "/dav/sub/copy.txt"})
	if got != http.StatusCreated {
		t.Fatalf("COPY returned %d, want 201", got)
	}
	if body, ok := backend.content("/share/sub/copy.txt"); !ok || body != "0123456789" {
		t.Fatalf("copied file holds %q (present=%v)", body, ok)
	}
	backend.mu.Lock()
	copies, streamed := backend.copies, backend.streamed
	backend.mu.Unlock()
	if copies != 1 {
		t.Fatalf("the VFS copy primitive ran %d times, want 1", copies)
	}
	// Letting the DAV handler do it would download and re-upload the file,
	// which is exactly the capability vfs.Copy exists to avoid.
	if streamed != 0 {
		t.Fatalf("COPY moved %d byte ranges through this process", streamed)
	}
}

func TestCopyHonoursOverwrite(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	if got := c.status(http.MethodPut, "/target.txt", "existing", nil); got != http.StatusCreated {
		t.Fatalf("setup PUT returned %d", got)
	}
	got := c.status("COPY", "/movie.txt", "", map[string]string{
		"Destination": "/dav/target.txt", "Overwrite": "F",
	})
	if got != http.StatusPreconditionFailed {
		t.Fatalf("COPY with Overwrite: F onto an existing file returned %d, want 412", got)
	}
	if body, _ := backend.content("/share/target.txt"); body != "existing" {
		t.Fatalf("a refused COPY changed the destination to %q", body)
	}
	got = c.status("COPY", "/movie.txt", "", map[string]string{"Destination": "/dav/target.txt"})
	if got != http.StatusNoContent {
		t.Fatalf("COPY over an existing file returned %d, want 204", got)
	}
	if body, _ := backend.content("/share/target.txt"); body != "0123456789" {
		t.Fatalf("overwriting COPY left %q", body)
	}
}

func TestCopyOfACollectionSaysSoPlainly(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	resp := c.do("COPY", "/sub", "", map[string]string{"Destination": "/dav/sub-copy"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("COPY of a collection returned %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(string(body), "collection") {
		t.Fatalf("the refusal does not say why: %q", body)
	}
	if backend.exists("/share/sub-copy") {
		t.Fatal("a refused collection COPY created something")
	}
}

func TestCopyToAnotherServerIsBadGateway(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	got := c.status("COPY", "/movie.txt", "", map[string]string{
		"Destination": "http://elsewhere.invalid/dav/copy.txt",
	})
	if got != http.StatusBadGateway {
		t.Fatalf("cross-server COPY returned %d, want 502", got)
	}
}

func TestCopyOntoItselfIsRefused(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	if got := c.status("COPY", "/movie.txt", "", map[string]string{"Destination": "/dav/movie.txt"}); got != http.StatusForbidden {
		t.Fatalf("COPY onto the same resource returned %d, want 403", got)
	}
	if body, _ := backend.content("/share/movie.txt"); body != "0123456789" {
		t.Fatalf("self-copy changed the file to %q", body)
	}
}

func TestLockAndUnlockRoundTrip(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	// Finder and Explorer LOCK before writing; without this they refuse the
	// share as read-only.
	lockBody := `<?xml version="1.0" encoding="utf-8"?>
<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope>
<D:locktype><D:write/></D:locktype><D:owner>cloudfs-test</D:owner></D:lockinfo>`
	resp := c.do("LOCK", "/movie.txt", lockBody, map[string]string{
		"Content-Type": "application/xml", "Timeout": "Second-60",
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("LOCK returned %d: %s", resp.StatusCode, body)
	}
	token := resp.Header.Get("Lock-Token")
	if token == "" {
		t.Fatalf("LOCK returned no Lock-Token: %s", body)
	}
	if got := c.status("UNLOCK", "/movie.txt", "", map[string]string{"Lock-Token": token}); got != http.StatusNoContent {
		t.Fatalf("UNLOCK returned %d, want 204", got)
	}
}

func TestPropfindStillWorksOnAWritableEndpoint(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	if got := c.status(http.MethodPut, "/added.txt", "hello", nil); got != http.StatusCreated {
		t.Fatalf("setup PUT returned %d", got)
	}
	resp := c.do("PROPFIND", "/", "", map[string]string{"Depth": "1"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND returned %d: %s", resp.StatusCode, body)
	}
	var multistatus struct {
		Responses []struct {
			Href string `xml:"href"`
		} `xml:"response"`
	}
	if err := xml.Unmarshal(body, &multistatus); err != nil {
		t.Fatalf("PROPFIND body is not valid XML: %v", err)
	}
	found := false
	for _, r := range multistatus.Responses {
		if strings.HasSuffix(r.Href, "/added.txt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the file created over DAV is not in the listing: %s", body)
	}
}

func TestOversizedXMLBodiesAreBounded(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	huge := strings.Repeat("x", 1<<20)
	// A PROPPATCH body is parsed in memory; an unbounded one is a way to make
	// the process do the work of allocating it.
	if got := c.status("PROPPATCH", "/movie.txt", huge, map[string]string{"Content-Type": "application/xml"}); got < 400 {
		t.Fatalf("an oversized PROPPATCH body returned %d, want a refusal", got)
	}
	// A PUT body is the file itself and must not be capped the same way.
	payload := strings.Repeat("y", 1<<20)
	if got := c.status(http.MethodPut, "/large.bin", payload, nil); got != http.StatusCreated {
		t.Fatalf("a 1 MiB PUT returned %d; file bodies must not hit the XML limit", got)
	}
	if body, _ := backend.content("/share/large.bin"); len(body) != len(payload) {
		t.Fatalf("stored %d bytes of a %d byte upload", len(body), len(payload))
	}
}

func TestWritableRefusesABackendThatCannotWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A read-only backend must not produce an endpoint that advertises the
	// write verbs and then fails at the first PUT.
	_, err := Start(ctx, Options{
		FS: &fakeBackend{body: []byte("x")}, Addr: "127.0.0.1:0",
		Prefix: "/dav", Root: "/share", Writable: true,
	})
	if err == nil {
		t.Fatal("Start accepted a writable endpoint over a read-only backend")
	}
	if !strings.Contains(err.Error(), "writable") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
}

func TestPathsWithSpacesAndUnicodeSurviveARoundTrip(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	// Percent-encoding is the client's job; assert the server decodes it once
	// and does not treat an encoded separator as a path separator.
	if got := c.status(http.MethodPut, "/%E7%89%87%20%E5%90%8D.txt", "unicode", nil); got != http.StatusCreated {
		t.Fatalf("PUT with an encoded name returned %d", got)
	}
	if body, ok := backend.content("/share/片 名.txt"); !ok || body != "unicode" {
		t.Fatalf("decoded path holds %q (present=%v)", body, ok)
	}
	resp := c.do(http.MethodGet, "/%E7%89%87%20%E5%90%8D.txt", "", nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != "unicode" {
		t.Fatalf("GET of the encoded name = %d %q", resp.StatusCode, got)
	}
}

func TestConcurrentPutsToDistinctNamesAllLand(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := fmt.Sprintf("/concurrent-%02d.txt", i)
			payload := strings.Repeat(fmt.Sprintf("%02d", i), 64)
			req, err := http.NewRequest(http.MethodPut, c.base+target, bytes.NewReader([]byte(payload)))
			if err != nil {
				errs <- err.Error()
				return
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err.Error()
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				errs <- fmt.Sprintf("%s -> %d", target, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Fatalf("concurrent PUT failed: %s", msg)
	}
	for i := 0; i < 16; i++ {
		p := fmt.Sprintf("/share/concurrent-%02d.txt", i)
		want := strings.Repeat(fmt.Sprintf("%02d", i), 64)
		if body, ok := backend.content(p); !ok || body != want {
			t.Fatalf("%s holds %q", p, body)
		}
	}
}

// TestWriteRequestsCarryTheWebDAVOrigin: the VFS reports a change with the
// origin its context carries, so the adapter must tag every request before
// the DAV handler turns it into filesystem calls.
func TestWriteRequestsCarryTheWebDAVOrigin(t *testing.T) {
	backend := newMemBackend()
	c := startWritable(t, backend, true)
	if code := c.status("MKCOL", "/tagged", "", nil); code != http.StatusCreated {
		t.Fatalf("MKCOL = %d", code)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.origins) != 1 || backend.origins[0] != "webdav" {
		t.Fatalf("Mkdir saw origins %q, want [webdav]", backend.origins)
	}
}
