package fusefs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"

	"golang.org/x/sys/unix"
)

// mountEnv is a live FUSE mount over a fake provider.
type mountEnv struct {
	dir  string
	fake *fakeprovider.Fake
	fs   *vfs.FS
	up   *upload.Uploader
	j    *journal.Journal
	mnt  *Mount
}

func requireFUSE(t *testing.T) {
	t.Helper()
	if ok, why := Supported(); !ok {
		t.Skipf("FUSE unavailable: %s", why)
	}
}

func newMount(t *testing.T, mode config.Mode) *mountEnv {
	return newMountWithTimeout(t, mode, time.Second)
}

// newMountWithTimeout mounts with the given kernel entry/attr timeout. Tests
// that assert the kernel answers a warm walk by itself need one longer than
// the walk takes under the race detector on a loaded machine.
func newMountWithTimeout(t *testing.T, mode config.Mode, kernelTTL time.Duration) *mountEnv {
	t.Helper()
	requireFUSE(t)
	base := t.TempDir()
	mountPoint := filepath.Join(base, "mnt")
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := meta.Open(filepath.Join(base, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Hydration's real trigger is a mount that has gone quiet for ten
	// seconds; no test wants to sit through that.
	ca, err := cache.New(cache.Options{Dir: filepath.Join(base, "cache"), BlockSize: 4096,
		HydrateAfter: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	j, err := journal.Open(journal.Options{Dir: filepath.Join(base, "journal")})
	if err != nil {
		t.Fatal(err)
	}
	fake := fakeprovider.New("ali")
	if mode == "" {
		mode = config.ModeWriteback
	}
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca,
		AttrTTL: time.Minute, DefaultDirTTL: time.Minute, NegativeTTL: time.Second,
		ReadAheadBlocks: 4,
		Mounts: []vfs.Mount{{
			Prefix: "/", Remote: "ali", RootID: fakeprovider.RootID,
			Provider: fake, Mode: mode, DirTTL: time.Minute,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	up, err := upload.New(upload.Options{
		Journal: j,
		Providers: func(remote string) (provider.Provider, bool) {
			if remote == "ali" {
				return fake, true
			}
			return nil, false
		},
		Policy:       retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: 10 * time.Millisecond}, MaxAttempts: 3},
		PollInterval: 10 * time.Millisecond,
		Hooks:        fsys.UploadHooks(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fsys.SetWriteBackend(j, up)

	ctx, cancel := context.WithCancel(context.Background())
	up.Start(ctx)

	m, err := MountFS(MountOptions{
		Options: Options{FS: fsys, AttrTimeout: kernelTTL, EntryTimeout: kernelTTL, NegativeTimeout: 100 * time.Millisecond},
		Path:    mountPoint,
		Debug:   os.Getenv("CLOUDFS_TEST_FUSE_DEBUG") == "1",
	})
	if err != nil {
		cancel()
		t.Fatalf("mount failed: %v", err)
	}
	e := &mountEnv{dir: mountPoint, fake: fake, fs: fsys, up: up, j: j, mnt: m}
	t.Cleanup(func() {
		m.Unmount()
		cancel()
		up.Stop()
		fsys.Close()
		j.Close()
		store.Close()
	})
	return e
}

// settle drains the upload queue so remote state is observable.
func (e *mountEnv) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := e.j.Stats(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st.Pending == 0 && st.Uploading == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := e.j.Stats(context.Background())
	t.Fatalf("upload queue did not drain: %+v", st)
}

func TestMountReadThroughSyscalls(t *testing.T) {
	e := newMount(t, "")
	e.fake.Seed("docs/readme.md", []byte("# hello from the cloud\n"))
	e.fake.Seed("docs/data.bin", bytes.Repeat([]byte("Z"), 10000))
	e.fake.Seed("top.txt", []byte("top level"))

	entries, err := os.ReadDir(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "docs" || names[1] != "top.txt" {
		t.Fatalf("readdir = %v", names)
	}

	got, err := os.ReadFile(filepath.Join(e.dir, "docs/readme.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "# hello from the cloud\n" {
		t.Fatalf("content = %q", got)
	}

	// A multi-block file reads back byte for byte.
	big, err := os.ReadFile(filepath.Join(e.dir, "docs/data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(big) != 10000 || !bytes.Equal(big, bytes.Repeat([]byte("Z"), 10000)) {
		t.Fatalf("big file = %d bytes", len(big))
	}

	// stat reports the right size and type.
	info, err := os.Stat(filepath.Join(e.dir, "docs/data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 10000 || info.IsDir() {
		t.Fatalf("stat = %d bytes, dir=%v", info.Size(), info.IsDir())
	}
	// A missing file gives ENOENT.
	if _, err := os.Stat(filepath.Join(e.dir, "nope.txt")); !os.IsNotExist(err) {
		t.Fatalf("missing file = %v", err)
	}
}

func TestMountWriteThroughSyscalls(t *testing.T) {
	e := newMount(t, "")
	target := filepath.Join(e.dir, "written.txt")
	payload := []byte("written through the kernel\n")

	if err := os.WriteFile(target, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	// Readable immediately, before the upload runs.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("read back = %q", got)
	}
	e.settle(t)

	// It landed on the remote.
	ctx := context.Background()
	entries, _, err := e.fake.List(ctx, fakeprovider.RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, en := range entries {
		if en.Name == "written.txt" && en.Size == int64(len(payload)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("file not on the remote: %+v", entries)
	}
	// And still reads correctly after the upload.
	got, err = os.ReadFile(target)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("post-upload read = %q, %v", got, err)
	}
}

func TestMountAppendAndPartialWrite(t *testing.T) {
	e := newMount(t, "")
	target := filepath.Join(e.dir, "log.txt")
	if err := os.WriteFile(target, []byte("line one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.settle(t)

	f, err := os.OpenFile(target, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("line two\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	e.settle(t)

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "line one\nline two\n" {
		t.Fatalf("after append = %q", got)
	}

	// An in-place overwrite must not truncate the rest.
	f, err = os.OpenFile(target, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("LINE"), 0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	e.settle(t)
	got, _ = os.ReadFile(target)
	if string(got) != "LINE one\nline two\n" {
		t.Fatalf("after partial write = %q", got)
	}
}

func TestMountMkdirRenameRemove(t *testing.T) {
	e := newMount(t, "")
	sub := filepath.Join(e.dir, "project")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.settle(t)

	if err := os.Rename(filepath.Join(sub, "a.txt"), filepath.Join(sub, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sub, "b.txt")); err != nil {
		t.Fatalf("renamed file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("old name should be gone")
	}
	// rmdir on a non-empty directory fails.
	if err := os.Remove(sub); err == nil {
		t.Fatal("rmdir on a non-empty directory should fail")
	}
	if err := os.Remove(filepath.Join(sub, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sub); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatal("directory should be gone")
	}
}

func TestMountTruncate(t *testing.T) {
	e := newMount(t, "")
	target := filepath.Join(e.dir, "trunc.txt")
	if err := os.WriteFile(target, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.settle(t)
	if err := os.Truncate(target, 4); err != nil {
		t.Fatal(err)
	}
	e.settle(t)
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "0123" {
		t.Fatalf("after truncate = %q", got)
	}
	info, _ := os.Stat(target)
	if info.Size() != 4 {
		t.Fatalf("size after truncate = %d", info.Size())
	}
}

func TestMountReadOnlyRefusesWrites(t *testing.T) {
	e := newMount(t, config.ModeReadonly)
	e.fake.Seed("locked.txt", []byte("read only content"))
	got, err := os.ReadFile(filepath.Join(e.dir, "locked.txt"))
	if err != nil || string(got) != "read only content" {
		t.Fatalf("read = %q, %v", got, err)
	}
	err = os.WriteFile(filepath.Join(e.dir, "new.txt"), []byte("x"), 0o644)
	if err == nil {
		t.Fatal("write to a read-only mount should fail")
	}
	if !strings.Contains(err.Error(), "read-only") && !os.IsPermission(err) {
		t.Fatalf("expected EROFS, got %v", err)
	}
}

func TestMountGrepAndFindWork(t *testing.T) {
	e := newMount(t, "")
	e.fake.Seed("src/main.go", []byte("package main\nfunc main() { println(\"needle\") }\n"))
	e.fake.Seed("src/util.go", []byte("package main\n// nothing here\n"))
	e.fake.Seed("docs/notes.md", []byte("no match\n"))

	if _, err := exec.LookPath("grep"); err != nil {
		t.Skip("grep not available")
	}
	out, err := exec.Command("grep", "-rl", "needle", e.dir).CombinedOutput()
	if err != nil {
		t.Fatalf("grep failed: %v (%s)", err, out)
	}
	if !strings.Contains(string(out), "main.go") {
		t.Fatalf("grep output = %q", out)
	}

	// find must walk the whole tree.
	if _, err := exec.LookPath("find"); err != nil {
		t.Skip("find not available")
	}
	out, err = exec.Command("find", e.dir, "-name", "*.go").CombinedOutput()
	if err != nil {
		t.Fatalf("find failed: %v (%s)", err, out)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) != 2 {
		t.Fatalf("find found %d go files: %q", len(lines), out)
	}
}

func TestMountXattrReportsState(t *testing.T) {
	e := newMount(t, "")
	// Hold the upload so the local-only window is observable.
	e.up.Stop()

	target := filepath.Join(e.dir, "state.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, err := getxattr(target, "user.cloudfs.state", buf)
	if err != nil {
		t.Skipf("xattr unsupported here: %v", err)
	}
	if string(buf[:n]) != "local" {
		t.Fatalf("state before upload = %q, want local", buf[:n])
	}
	// The file is still readable while it is only local.
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "x" {
		t.Fatalf("local-only read = %q, %v", got, err)
	}

	if _, err := e.up.DrainAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, err = getxattr(target, "user.cloudfs.state", buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "synced" {
		t.Fatalf("state after upload = %q, want synced", buf[:n])
	}
	n, err = getxattr(target, "user.cloudfs.remote", buf)
	if err != nil || string(buf[:n]) != "ali" {
		t.Fatalf("remote xattr = %q, %v", buf[:n], err)
	}
	// user.cloudfs.writer answers from the hook the daemon installs
	// (SetLastWriter, over the change record); without one it does not
	// exist, with one it is what the hook says for that inode.
	if _, err := getxattr(target, "user.cloudfs.writer", buf); err == nil {
		t.Fatal("writer xattr exists without a hook")
	}
	at, err := e.fs.StatPath(context.Background(), "/state.txt")
	if err != nil {
		t.Fatal(err)
	}
	e.fs.SetLastWriter(func(_ context.Context, ino uint64) (string, bool) {
		if ino == at.Ino {
			return "mcp 0123456789abcdef", true
		}
		return "", false
	})
	n, err = getxattr(target, "user.cloudfs.writer", buf)
	if err != nil || string(buf[:n]) != "mcp 0123456789abcdef" {
		t.Fatalf("writer xattr = %q, %v", buf[:n], err)
	}
}

func TestMountStatfsReportsCacheBudget(t *testing.T) {
	e := newMount(t, "")
	if _, err := exec.LookPath("df"); err != nil {
		t.Skip("df unavailable")
	}
	out, err := exec.Command("df", "-k", e.dir).CombinedOutput()
	if err != nil {
		t.Skipf("df failed: %v (%s)", err, out)
	}
	// The mount must report a non-zero capacity, otherwise tools refuse to
	// write to it before the cache backpressure ever kicks in.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		t.Fatalf("unexpected df output: %s", out)
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		t.Fatalf("unexpected df row: %q", lines[len(lines)-1])
	}
	if fields[1] == "0" || fields[3] == "0" {
		t.Fatalf("statfs reported no capacity: %q", lines[len(lines)-1])
	}
}

func TestMountLargeSequentialRead(t *testing.T) {
	e := newMount(t, "")
	// 1 MiB over 4 KiB blocks exercises read-ahead and block assembly.
	content := make([]byte, 1<<20)
	for i := range content {
		content[i] = byte(i % 251)
	}
	e.fake.Seed("large.bin", content)

	got, err := os.ReadFile(filepath.Join(e.dir, "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("large read mismatch: %d bytes", len(got))
	}
	// Reading again is served from the cache.
	before := e.fake.Calls("ReadRange")
	got2, err := os.ReadFile(filepath.Join(e.dir, "large.bin"))
	if err != nil || !bytes.Equal(got2, content) {
		t.Fatal("second large read mismatch")
	}
	if after := e.fake.Calls("ReadRange"); after != before {
		t.Fatalf("warm read made %d provider calls", after-before)
	}
}

// TestDropCachesMakesTheMountColdAgain: after dropping both the VFS and the
// kernel caches, a directory walk that was free must reach the backend again.
// Without the kernel half, 30-second entry timeouts would let the kernel
// answer the walk by itself and a "cold" measurement would be nothing of the
// kind.
func TestDropCachesMakesTheMountColdAgain(t *testing.T) {
	e := newMount(t, "")
	for i := 0; i < 5; i++ {
		e.fake.Seed(fmt.Sprintf("d/f%d.txt", i), []byte("x"))
	}
	walk := func() int {
		n := 0
		filepath.WalkDir(filepath.Join(e.dir, "d"), func(p string, d os.DirEntry, err error) error {
			if err == nil {
				if _, err := os.Stat(p); err == nil {
					n++
				}
			}
			return nil
		})
		return n
	}
	if walk() != 6 {
		t.Fatal("initial walk did not see the tree")
	}
	lists := e.fake.Calls("List")
	walk()
	if e.fake.Calls("List") != lists {
		t.Fatal("a warm walk should cost no listing")
	}
	if _, err := e.fs.DropCaches(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := e.mnt.DropKernelCaches(); n == 0 {
		t.Fatal("the kernel held entries for the walked tree; none were invalidated")
	}
	if walk() != 6 {
		t.Fatal("walk after drop did not see the tree")
	}
	if e.fake.Calls("List") == lists {
		t.Fatal("after a drop the walk should reach the backend again")
	}
}

// TestKernelDirCacheIsInvalidatedOnChange is the safety side of caching the
// directory stream in the kernel: a listing must reflect a create, a delete
// and a rename made through the mount, and a change that arrived from the
// backend once the VFS refreshed the directory.
func TestKernelDirCacheIsInvalidatedOnChange(t *testing.T) {
	e := newMount(t, "")
	dir := filepath.Join(e.dir, "d")
	e.fake.Seed("d/a.txt", []byte("a"))
	names := func() []string {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, en := range ents {
			out = append(out, en.Name())
		}
		sort.Strings(out)
		return out
	}
	if got := names(); strings.Join(got, ",") != "a.txt" {
		t.Fatalf("initial listing = %v", got)
	}
	// Second listing is served by the kernel: no VFS readdir.
	lists := e.fake.Calls("List")
	names()
	if e.fake.Calls("List") != lists {
		t.Fatal("a repeated listing should not reach the backend")
	}
	// Local changes must show up immediately.
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := names(); strings.Join(got, ",") != "a.txt,b.txt" {
		t.Fatalf("after create = %v", got)
	}
	if err := os.Rename(filepath.Join(dir, "b.txt"), filepath.Join(dir, "c.txt")); err != nil {
		t.Fatal(err)
	}
	if got := names(); strings.Join(got, ",") != "a.txt,c.txt" {
		t.Fatalf("after rename = %v", got)
	}
	// Let the rename's upload land before deleting, so the delete reaches
	// the backend rather than cancelling a queued write.
	e.settle(t)
	if err := os.Remove(filepath.Join(dir, "c.txt")); err != nil {
		t.Fatal(err)
	}
	if got := names(); strings.Join(got, ",") != "a.txt" {
		t.Fatalf("after remove = %v", got)
	}
	// A change made on the backend appears once the directory is refreshed.
	e.settle(t)
	e.fake.Seed("d/remote.txt", []byte("r"))
	at, err := e.fs.StatPath(context.Background(), "/d")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Refresh(context.Background(), at.Ino); err != nil {
		t.Fatal(err)
	}
	// The kernel is told asynchronously; give the notification a moment.
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := names()
		if strings.Join(got, ",") == "a.txt,remote.txt" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after backend change + refresh = %v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPassthroughServesHydratedFilesWithoutReadRequests: once a file is fully
// cached, reading it must not cross into this process at all. Files that are
// not fully cached, and files with a pending local write, keep taking the
// ordinary path.
func TestPassthroughServesHydratedFilesWithoutReadRequests(t *testing.T) {
	t.Setenv("CLOUDFS_EXPERIMENTAL_PASSTHROUGH", "1")
	e := newMount(t, "")
	if !e.mnt.root.passthrough {
		t.Skip("passthrough not available on this kernel")
	}
	body := make([]byte, 3*4096+100)
	for i := range body {
		body[i] = byte(i)
	}
	e.fake.Seed("big.bin", body)
	p := filepath.Join(e.dir, "big.bin")

	// First read faults the blocks in through us.
	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("first read: %v", err)
	}
	if e.mnt.root.ops[opRead].Load() == 0 {
		t.Fatal("the cold read should have gone through the process")
	}
	// Give hydration a moment to merge the blocks.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := e.fs.Cache().HydratedPath(cacheKey(t, e, "/big.bin")); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("file never hydrated")
		}
		time.Sleep(20 * time.Millisecond)
	}
	before := e.mnt.root.ops[opRead].Load()
	got, err = os.ReadFile(p)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("second read: %v", err)
	}
	if after := e.mnt.root.ops[opRead].Load(); after != before {
		t.Fatalf("a hydrated file cost %d READ requests; passthrough should have served it", after-before)
	}

	// A file with a pending local write must not be passed through: the
	// kernel would read the stale cached copy.
	if err := os.WriteFile(p, []byte("rewritten"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(p)
	if err != nil || string(got) != "rewritten" {
		t.Fatalf("read after local write = %q, %v", got, err)
	}
}

func cacheKey(t *testing.T, e *mountEnv, path string) cache.FileKey {
	t.Helper()
	n, err := e.fs.Meta().Resolve(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
}

// This is the real-kernel counterpart of the bridge-only lifetime test. It
// deliberately leaves a second passthrough reader open after the offering
// handle closes, then invalidates the cache name beneath the kernel's fd.
func TestPassthroughKernelLeaseSurvivesFirstCloseAndInvalidation(t *testing.T) {
	t.Setenv("CLOUDFS_EXPERIMENTAL_PASSTHROUGH", "1")
	e := newMount(t, "")
	if !e.mnt.root.passthrough {
		t.Skip("passthrough not available on this kernel")
	}
	body := []byte("kernel-held immutable payload")
	e.fake.Seed("lease.bin", body)
	if _, err := e.fs.StatPath(context.Background(), "/lease.bin"); err != nil {
		t.Fatal(err)
	}
	k := cacheKey(t, e, "/lease.bin")
	src := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(src, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Cache().LinkFile(k, src, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(e.dir, "lease.bin")
	first, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	releases := e.mnt.root.ops[opRelease].Load()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for e.mnt.root.ops[opRelease].Load() == releases {
		if time.Now().After(deadline) {
			t.Fatal("first handle was not released")
		}
		time.Sleep(time.Millisecond)
	}
	e.fs.Cache().Forget(k)
	if s := e.fs.Cache().Stats(); s.LeasedBytes != int64(len(body)) {
		t.Fatalf("kernel lease released early: %+v", s)
	}
	if err := e.fs.Cache().GC(); err != nil {
		t.Fatal(err)
	}
	reads := e.mnt.root.ops[opRead].Load()
	got := make([]byte, len(body))
	if _, err := second.ReadAt(got, 0); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("remaining reader: %q %v", got, err)
	}
	if e.mnt.root.ops[opRead].Load() != reads {
		t.Fatal("read did not use kernel passthrough")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for e.fs.Cache().Stats().LeasedBytes != 0 {
		if time.Now().After(deadline) {
			t.Fatal("last close did not release backing lease")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestReadOnlyOpenKeepsKernelCache: a read-only Open() returns
// FOPEN_KEEP_CACHE, so the kernel does not invalidate a file's page cache
// just because it was opened again. Reading the same file a second time,
// through a fresh file descriptor, should be served entirely out of that
// page cache and never reach this process's READ handler.
func TestReadOnlyOpenKeepsKernelCache(t *testing.T) {
	e := newMount(t, "")
	body := []byte("cache me across opens, please, kernel")
	e.fake.Seed("cached.txt", body)
	p := filepath.Join(e.dir, "cached.txt")

	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("first read: %v, %q", err, got)
	}
	if e.mnt.root.ops[opRead].Load() == 0 {
		t.Fatal("the first read should have gone through the process")
	}

	before := e.mnt.root.ops[opRead].Load()
	got, err = os.ReadFile(p)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("second read: %v, %q", err, got)
	}
	if after := e.mnt.root.ops[opRead].Load(); after != before {
		t.Fatalf("a second read-only open cost %d READ requests; FOPEN_KEEP_CACHE should have kept the kernel page cache warm", after-before)
	}
}

// TestLseekReportsNoHoles exercises FileLseeker through a real mount:
// SEEK_HOLE from the start of a file with no holes must land exactly on
// EOF, which is what GNU cp's --sparse=auto probe relies on.
func TestLseekReportsNoHoles(t *testing.T) {
	e := newMount(t, "")
	body := []byte("no holes anywhere in this file")
	e.fake.Seed("plain.txt", body)
	p := filepath.Join(e.dir, "plain.txt")

	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	off, err := unix.Seek(int(f.Fd()), 0, unix.SEEK_HOLE)
	if err != nil {
		t.Fatalf("SEEK_HOLE: %v", err)
	}
	if off != int64(len(body)) {
		t.Fatalf("SEEK_HOLE from 0 = %d, want %d (EOF, no interior holes)", off, len(body))
	}
}

// TestPathThroughAFileIsENOTDIR: pjdfstest expects open("d/file/x") to fail
// with ENOTDIR when "file" is a regular file. The kernel decides this from
// the attributes we report, so ENOENT here means we described a file wrongly
// or answered a lookup we should not have.
func TestPathThroughAFileIsENOTDIR(t *testing.T) {
	e := newMount(t, "")
	dir := filepath.Join(e.dir, "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := os.Open(filepath.Join(dir, "file", "test"))
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("open through a regular file = %v, want ENOTDIR", err)
	}
	_, err = os.OpenFile(filepath.Join(dir, "file", "test"), os.O_CREATE|os.O_WRONLY, 0o644)
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("create through a regular file = %v, want ENOTDIR", err)
	}
}

// TestWalkCostsOneRequestPerDirectoryAndThenNone pins the request budget of a
// tree walk. Cold: one opendir per directory and every entry answered from
// the listing (dir_lookup), never by a per-entry lookup against the metadata
// store. Warm, inside the entry and attribute timeouts: the kernel answers
// readdir and stat itself, so nothing but the opendirs reaches this process.
func TestWalkCostsOneRequestPerDirectoryAndThenNone(t *testing.T) {
	// The warm walk must land inside the kernel's entry timeout, or expired
	// dentries are re-looked-up and the count says nothing about caching.
	e := newMountWithTimeout(t, "", 30*time.Second)
	const dirs, files = 5, 20
	for d := 0; d < dirs; d++ {
		for f := 0; f < files; f++ {
			e.fake.Seed(fmt.Sprintf("d%d/f%02d.txt", d, f), []byte("x"))
		}
	}
	walk := func() int {
		n := 0
		err := filepath.WalkDir(e.dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				if _, err := os.Stat(p); err != nil {
					return err
				}
			}
			n++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	delta := func(f func()) map[string]int64 {
		before := e.mnt.OpStats().Ops
		f()
		after := e.mnt.OpStats().Ops
		d := map[string]int64{}
		for k, v := range after {
			if v-before[k] != 0 {
				d[k] = v - before[k]
			}
		}
		return d
	}
	cold := delta(func() {
		if n := walk(); n != 1+dirs+dirs*files {
			t.Fatalf("walk saw %d entries", n)
		}
	})
	t.Logf("cold walk: %v", cold)
	if cold["opendir"] != 1+dirs {
		t.Errorf("cold walk sent %d opendirs, want %d", cold["opendir"], 1+dirs)
	}
	// One listing per directory is the claim. The kernel may occasionally ask
	// again on a handle it has dropped, which builds one more listing and
	// still costs nothing per entry, so the count is a floor plus that
	// allowance; what must hold exactly is where the entries came from.
	if cold["readdir"] < int64(1+dirs) || cold["readdir"] > int64(2+dirs) {
		t.Errorf("cold walk built %d listings, want one per directory (%d)", cold["readdir"], 1+dirs)
	}
	if cold["lookup"] != 0 {
		t.Errorf("cold walk sent %d per-entry lookups to the node; readdirplus must be served from the listing", cold["lookup"])
	}
	if cold["dir_lookup"] != dirs+dirs*files {
		t.Errorf("cold walk answered %d entries from listings, want %d", cold["dir_lookup"], dirs+dirs*files)
	}
	// The one allowed getattr is the walk's own stat of the mount root.
	if cold["getattr"] > 1 {
		t.Errorf("cold walk sent %d getattrs; readdirplus already carried the attributes", cold["getattr"])
	}
	warm := delta(func() { walk() })
	t.Logf("warm walk: %v", warm)
	for op, n := range warm {
		switch op {
		case "opendir":
			// The kernel opens a directory through us even when it serves
			// the listing itself; the handle must not build a listing for
			// that (counted as readdir, and checked below with the rest).
		case "getattr":
			// After an uncached readdir the kernel drops that directory's
			// attributes once; the walk's stat of the root pays it here.
			if n > 1 {
				t.Errorf("warm walk sent %d getattrs, want at most the root's", n)
			}
		default:
			if n != 0 {
				t.Errorf("warm walk sent %d %s requests; the kernel should answer from its caches", n, op)
			}
		}
	}
}

// TestRewritingAFileSmallerDoesNotKeepTheOldTail: the kernel does not send
// O_TRUNC with the open; it opens the file and then truncates it through a
// handle of its own. A write handle that had already downloaded the old
// content would write the new bytes into a full-sized staging file and commit
// the old tail back — silently, and all the way to the backend.
func TestRewritingAFileSmallerDoesNotKeepTheOldTail(t *testing.T) {
	e := newMount(t, "")
	p := filepath.Join(e.dir, "rewrite.bin")
	big := bytes.Repeat([]byte("a"), 64<<10)
	if err := os.WriteFile(p, big, 0o644); err != nil {
		t.Fatal(err)
	}
	small := bytes.Repeat([]byte("b"), 4<<10)
	if err := os.WriteFile(p, small, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != int64(len(small)) {
		t.Fatalf("after rewriting, the file is %d bytes, want %d", st.Size(), len(small))
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, small) {
		t.Fatalf("read back %d bytes, want the %d just written", len(got), len(small))
	}
	// And the same once it has left for the backend.
	e.settle(t)
	if b, ok := e.fake.Content("rewrite.bin"); !ok || !bytes.Equal(b, small) {
		t.Fatalf("the backend holds %d bytes, want %d", len(b), len(small))
	}
}

// TestRewritingAFileDoesNotLandAsAConflictCopy: rewriting a file is two
// uploads — the truncate the kernel does first, then the content. The second
// one starts from the version the first one created, so if the node does not
// learn that its own upload moved the remote version, the file's new content
// is filed away as somebody else's conflicting change.
func TestRewritingAFileDoesNotLandAsAConflictCopy(t *testing.T) {
	e := newMount(t, "")
	p := filepath.Join(e.dir, "doc.bin")
	if err := os.WriteFile(p, bytes.Repeat([]byte("a"), 8<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	e.settle(t)
	second := bytes.Repeat([]byte("b"), 12<<10)
	if err := os.WriteFile(p, second, 0o644); err != nil {
		t.Fatal(err)
	}
	e.settle(t)

	if b, ok := e.fake.Content("doc.bin"); !ok || !bytes.Equal(b, second) {
		t.Fatalf("the backend holds %d bytes under the original name, want the %d just written", len(b), len(second))
	}
	ents, err := os.ReadDir(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range ents {
		if strings.Contains(ent.Name(), "conflict") {
			t.Fatalf("rewriting a file produced %q", ent.Name())
		}
	}
}

// TestRewritingDoesNotDownloadWhatItReplaces: the kernel turns O_TRUNC into a
// truncate to zero, and the content that truncate discards must not be
// fetched first — rewriting a 1 GiB file would otherwise download it.
func TestRewritingDoesNotDownloadWhatItReplaces(t *testing.T) {
	e := newMount(t, "")
	body := bytes.Repeat([]byte("a"), 128<<10)
	e.fake.Seed("big.bin", body)
	p := filepath.Join(e.dir, "big.bin")
	// Read nothing: the file only exists on the backend.
	before := e.fake.ReadBytes()
	if err := os.WriteFile(p, []byte("small"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.ReadBytes() - before; got != 0 {
		t.Fatalf("rewriting the file read %d bytes of the content it replaced", got)
	}
	e.settle(t)
	if b, ok := e.fake.Content("big.bin"); !ok || string(b) != "small" {
		t.Fatalf("the backend holds %d bytes, want the 5 just written", len(b))
	}
}
