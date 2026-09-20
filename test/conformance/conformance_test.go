// Package conformance checks the mounted filesystem behaves the way programs
// expect: the same operations run against cloudfs and against a plain local
// directory must produce the same observable results.
//
// Anything cloudfs deliberately does differently (permissions kept locally
// because no remote has anywhere to put them, no hard links, close-to-open
// visibility) is asserted explicitly rather than left to chance.
package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/fusefs"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

// pair is one cloudfs mount plus a plain local directory to compare against.
type pair struct {
	cloud string
	local string
	j     *journal.Journal
	up    *upload.Uploader
}

func newPair(t *testing.T) *pair {
	t.Helper()
	if ok, why := fusefs.Supported(); !ok {
		t.Skipf("FUSE unavailable: %s", why)
	}
	base := t.TempDir()
	cloudDir := filepath.Join(base, "cloud")
	localDir := filepath.Join(base, "local")
	for _, d := range []string{cloudDir, localDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store, err := meta.Open(filepath.Join(base, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ca, err := cache.New(cache.Options{Dir: filepath.Join(base, "cache"), BlockSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(journal.Options{Dir: filepath.Join(base, "journal")})
	if err != nil {
		t.Fatal(err)
	}
	fake := fakeprovider.New("ali")
	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca,
		AttrTTL: time.Minute, DefaultDirTTL: time.Minute, NegativeTTL: 100 * time.Millisecond,
		WriteSettle: 150 * time.Millisecond,
		Mounts: []vfs.Mount{{
			Prefix: "/", Remote: "ali", RootID: fakeprovider.RootID,
			Provider: fake, Mode: config.ModeWriteback, DirTTL: time.Minute,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	up, err := upload.New(upload.Options{
		Journal:      j,
		Providers:    func(string) (provider.Provider, bool) { return fake, true },
		Policy:       retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: 5 * time.Millisecond}, MaxAttempts: 3},
		PollInterval: 10 * time.Millisecond,
		Hooks:        fsys.UploadHooks(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fsys.SetWriteBackend(j, up)
	ctx, cancel := context.WithCancel(context.Background())
	up.Start(ctx)

	m, err := fusefs.MountFS(fusefs.MountOptions{
		Options: fusefs.Options{FS: fsys, AttrTimeout: 200 * time.Millisecond, EntryTimeout: 200 * time.Millisecond},
		Path:    cloudDir,
	})
	if err != nil {
		cancel()
		t.Fatalf("mount: %v", err)
	}
	t.Cleanup(func() {
		m.Unmount()
		cancel()
		up.Stop()
		fsys.Close()
		j.Close()
		store.Close()
	})
	return &pair{cloud: cloudDir, local: localDir, j: j, up: up}
}

func (p *pair) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := p.j.Stats(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st.Pending == 0 && st.Uploading == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("upload queue did not drain")
}

// bothDo runs fn against each directory and returns the two results, so a test
// can assert they agree.
func (p *pair) bothDo(t *testing.T, fn func(root string) (any, error)) (cloudRes, localRes any, cloudErr, localErr error) {
	t.Helper()
	cloudRes, cloudErr = fn(p.cloud)
	localRes, localErr = fn(p.local)
	return
}

// sameErrno reports whether two errors are the same kind of failure.
func sameErrno(a, b error) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	var ea, eb syscall.Errno
	if errors.As(a, &ea) && errors.As(b, &eb) {
		return ea == eb
	}
	return os.IsNotExist(a) == os.IsNotExist(b) &&
		os.IsExist(a) == os.IsExist(b) &&
		os.IsPermission(a) == os.IsPermission(b)
}

func TestCreateReadWriteMatchLocal(t *testing.T) {
	p := newPair(t)
	content := []byte("the quick brown fox\n")

	_, _, ce, le := p.bothDo(t, func(root string) (any, error) {
		return nil, os.WriteFile(filepath.Join(root, "f.txt"), content, 0o644)
	})
	if !sameErrno(ce, le) {
		t.Fatalf("write: cloud=%v local=%v", ce, le)
	}
	p.settle(t)

	cr, lr, ce, le := p.bothDo(t, func(root string) (any, error) {
		return os.ReadFile(filepath.Join(root, "f.txt"))
	})
	if !sameErrno(ce, le) {
		t.Fatalf("read: cloud=%v local=%v", ce, le)
	}
	if !bytes.Equal(cr.([]byte), lr.([]byte)) {
		t.Fatalf("content differs: cloud=%q local=%q", cr, lr)
	}

	// Size and type agree.
	cs, ls, _, _ := p.bothDo(t, func(root string) (any, error) {
		return os.Stat(filepath.Join(root, "f.txt"))
	})
	cinfo, linfo := cs.(os.FileInfo), ls.(os.FileInfo)
	if cinfo.Size() != linfo.Size() {
		t.Errorf("size: cloud=%d local=%d", cinfo.Size(), linfo.Size())
	}
	if cinfo.IsDir() != linfo.IsDir() {
		t.Error("IsDir disagrees")
	}
	if cinfo.Mode().IsRegular() != linfo.Mode().IsRegular() {
		t.Error("IsRegular disagrees")
	}
}

func TestMissingFileGivesENOENT(t *testing.T) {
	p := newPair(t)
	for _, op := range []struct {
		name string
		fn   func(root string) (any, error)
	}{
		{"stat", func(root string) (any, error) { return os.Stat(filepath.Join(root, "nope")) }},
		{"open", func(root string) (any, error) { return os.Open(filepath.Join(root, "nope")) }},
		{"read", func(root string) (any, error) { return os.ReadFile(filepath.Join(root, "nope")) }},
		{"remove", func(root string) (any, error) { return nil, os.Remove(filepath.Join(root, "nope")) }},
		{"readdir", func(root string) (any, error) { return os.ReadDir(filepath.Join(root, "nope")) }},
	} {
		_, _, ce, le := p.bothDo(t, op.fn)
		if !sameErrno(ce, le) {
			t.Errorf("%s on a missing path: cloud=%v local=%v", op.name, ce, le)
		}
		if !os.IsNotExist(ce) {
			t.Errorf("%s should give ENOENT, got %v", op.name, ce)
		}
	}
}

func TestDirectoryOperationsMatchLocal(t *testing.T) {
	p := newPair(t)

	_, _, ce, le := p.bothDo(t, func(root string) (any, error) {
		return nil, os.Mkdir(filepath.Join(root, "d"), 0o755)
	})
	if !sameErrno(ce, le) {
		t.Fatalf("mkdir: cloud=%v local=%v", ce, le)
	}
	// Creating it twice fails the same way on both.
	_, _, ce, le = p.bothDo(t, func(root string) (any, error) {
		return nil, os.Mkdir(filepath.Join(root, "d"), 0o755)
	})
	if !sameErrno(ce, le) {
		t.Fatalf("duplicate mkdir: cloud=%v local=%v", ce, le)
	}
	if !os.IsExist(ce) {
		t.Errorf("duplicate mkdir should give EEXIST, got %v", ce)
	}

	// Removing a non-empty directory fails on both.
	_, _, ce, le = p.bothDo(t, func(root string) (any, error) {
		return nil, os.WriteFile(filepath.Join(root, "d", "x.txt"), []byte("x"), 0o644)
	})
	if !sameErrno(ce, le) {
		t.Fatalf("write into dir: cloud=%v local=%v", ce, le)
	}
	p.settle(t)
	_, _, ce, le = p.bothDo(t, func(root string) (any, error) {
		return nil, os.Remove(filepath.Join(root, "d"))
	})
	if (ce == nil) != (le == nil) {
		t.Fatalf("rmdir non-empty: cloud=%v local=%v", ce, le)
	}
	if ce == nil {
		t.Error("removing a non-empty directory should fail")
	}

	// Emptying it makes removal succeed on both.
	p.bothDo(t, func(root string) (any, error) {
		return nil, os.Remove(filepath.Join(root, "d", "x.txt"))
	})
	_, _, ce, le = p.bothDo(t, func(root string) (any, error) {
		return nil, os.Remove(filepath.Join(root, "d"))
	})
	if ce != nil || le != nil {
		t.Fatalf("rmdir empty: cloud=%v local=%v", ce, le)
	}
}

func TestReaddirOrderAndContentMatch(t *testing.T) {
	p := newPair(t)
	names := []string{"b.txt", "a.txt", "c.txt", "sub"}
	for _, root := range []string{p.cloud, p.local} {
		for _, n := range names {
			var err error
			if n == "sub" {
				err = os.Mkdir(filepath.Join(root, n), 0o755)
			} else {
				err = os.WriteFile(filepath.Join(root, n), []byte(n), 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	p.settle(t)

	cr, lr, ce, le := p.bothDo(t, func(root string) (any, error) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, e := range entries {
			kind := "f"
			if e.IsDir() {
				kind = "d"
			}
			out = append(out, kind+":"+e.Name())
		}
		sort.Strings(out)
		return out, nil
	})
	if ce != nil || le != nil {
		t.Fatalf("readdir: cloud=%v local=%v", ce, le)
	}
	cloudList, localList := cr.([]string), lr.([]string)
	if fmt.Sprint(cloudList) != fmt.Sprint(localList) {
		t.Fatalf("listings differ:\n cloud=%v\n local=%v", cloudList, localList)
	}
}

func TestSeekAndPartialReadsMatch(t *testing.T) {
	p := newPair(t)
	content := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	for _, root := range []string{p.cloud, p.local} {
		if err := os.WriteFile(filepath.Join(root, "seek.bin"), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p.settle(t)

	cr, lr, ce, le := p.bothDo(t, func(root string) (any, error) {
		f, err := os.Open(filepath.Join(root, "seek.bin"))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		var out [][]byte
		for _, off := range []int64{0, 10, 30, 35} {
			if _, err := f.Seek(off, io.SeekStart); err != nil {
				return nil, err
			}
			buf := make([]byte, 8)
			n, err := f.Read(buf)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			out = append(out, buf[:n])
		}
		// Seeking past the end reads nothing.
		if _, err := f.Seek(1000, io.SeekStart); err != nil {
			return nil, err
		}
		buf := make([]byte, 8)
		n, err := f.Read(buf)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		out = append(out, buf[:n])
		return out, nil
	})
	if ce != nil || le != nil {
		t.Fatalf("seek reads: cloud=%v local=%v", ce, le)
	}
	cloudReads, localReads := cr.([][]byte), lr.([][]byte)
	if len(cloudReads) != len(localReads) {
		t.Fatalf("different number of reads: %d vs %d", len(cloudReads), len(localReads))
	}
	for i := range cloudReads {
		if !bytes.Equal(cloudReads[i], localReads[i]) {
			t.Errorf("read %d differs: cloud=%q local=%q", i, cloudReads[i], localReads[i])
		}
	}
}

func TestAppendAndTruncateMatch(t *testing.T) {
	p := newPair(t)
	for _, root := range []string{p.cloud, p.local} {
		if err := os.WriteFile(filepath.Join(root, "log.txt"), []byte("line1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p.settle(t)

	_, _, ce, le := p.bothDo(t, func(root string) (any, error) {
		f, err := os.OpenFile(filepath.Join(root, "log.txt"), os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		if _, err := f.WriteString("line2\n"); err != nil {
			f.Close()
			return nil, err
		}
		return nil, f.Close()
	})
	if !sameErrno(ce, le) {
		t.Fatalf("append: cloud=%v local=%v", ce, le)
	}
	p.settle(t)

	cr, lr, _, _ := p.bothDo(t, func(root string) (any, error) {
		return os.ReadFile(filepath.Join(root, "log.txt"))
	})
	if !bytes.Equal(cr.([]byte), lr.([]byte)) {
		t.Fatalf("after append: cloud=%q local=%q", cr, lr)
	}

	_, _, ce, le = p.bothDo(t, func(root string) (any, error) {
		return nil, os.Truncate(filepath.Join(root, "log.txt"), 3)
	})
	if !sameErrno(ce, le) {
		t.Fatalf("truncate: cloud=%v local=%v", ce, le)
	}
	p.settle(t)
	cr, lr, _, _ = p.bothDo(t, func(root string) (any, error) {
		return os.ReadFile(filepath.Join(root, "log.txt"))
	})
	if !bytes.Equal(cr.([]byte), lr.([]byte)) {
		t.Fatalf("after truncate: cloud=%q local=%q", cr, lr)
	}

	// Truncate to grow: both should zero-fill.
	_, _, ce, le = p.bothDo(t, func(root string) (any, error) {
		return nil, os.Truncate(filepath.Join(root, "log.txt"), 10)
	})
	if !sameErrno(ce, le) {
		t.Fatalf("grow truncate: cloud=%v local=%v", ce, le)
	}
	p.settle(t)
	cr, lr, _, _ = p.bothDo(t, func(root string) (any, error) {
		return os.ReadFile(filepath.Join(root, "log.txt"))
	})
	if !bytes.Equal(cr.([]byte), lr.([]byte)) {
		t.Fatalf("after grow: cloud=%q (%d) local=%q (%d)", cr, len(cr.([]byte)), lr, len(lr.([]byte)))
	}
}

func TestRenameMatchesLocal(t *testing.T) {
	p := newPair(t)
	for _, root := range []string{p.cloud, p.local} {
		os.Mkdir(filepath.Join(root, "a"), 0o755)
		os.Mkdir(filepath.Join(root, "b"), 0o755)
		os.WriteFile(filepath.Join(root, "a", "f.txt"), []byte("data"), 0o644)
	}
	p.settle(t)

	// Rename within a directory.
	_, _, ce, le := p.bothDo(t, func(root string) (any, error) {
		return nil, os.Rename(filepath.Join(root, "a", "f.txt"), filepath.Join(root, "a", "g.txt"))
	})
	if !sameErrno(ce, le) {
		t.Fatalf("rename in place: cloud=%v local=%v", ce, le)
	}
	// Move across directories.
	_, _, ce, le = p.bothDo(t, func(root string) (any, error) {
		return nil, os.Rename(filepath.Join(root, "a", "g.txt"), filepath.Join(root, "b", "g.txt"))
	})
	if !sameErrno(ce, le) {
		t.Fatalf("rename across dirs: cloud=%v local=%v", ce, le)
	}
	p.settle(t)
	cr, lr, _, _ := p.bothDo(t, func(root string) (any, error) {
		return os.ReadFile(filepath.Join(root, "b", "g.txt"))
	})
	if !bytes.Equal(cr.([]byte), lr.([]byte)) {
		t.Fatalf("moved content: cloud=%q local=%q", cr, lr)
	}
	// The old path is gone on both.
	_, _, ce, le = p.bothDo(t, func(root string) (any, error) {
		return os.Stat(filepath.Join(root, "a", "g.txt"))
	})
	if !sameErrno(ce, le) {
		t.Fatalf("old path: cloud=%v local=%v", ce, le)
	}
}

func TestWriteThenReadWithoutClosingSeesOwnWrites(t *testing.T) {
	// A program that writes and reads through the same handle must see its own
	// data even before close, on both filesystems.
	p := newPair(t)
	cr, lr, ce, le := p.bothDo(t, func(root string) (any, error) {
		f, err := os.OpenFile(filepath.Join(root, "rw.txt"), os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if _, err := f.WriteAt([]byte("hello world"), 0); err != nil {
			return nil, err
		}
		buf := make([]byte, 5)
		if _, err := f.ReadAt(buf, 6); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return string(buf), nil
	})
	if !sameErrno(ce, le) {
		t.Fatalf("read-your-writes: cloud=%v local=%v", ce, le)
	}
	if cr != lr {
		t.Fatalf("read-your-writes differs: cloud=%q local=%q", cr, lr)
	}
	if cr != "world" {
		t.Fatalf("expected to read back %q, got %q", "world", cr)
	}
}

func TestEmptyFileHandling(t *testing.T) {
	p := newPair(t)
	_, _, ce, le := p.bothDo(t, func(root string) (any, error) {
		f, err := os.Create(filepath.Join(root, "empty.txt"))
		if err != nil {
			return nil, err
		}
		return nil, f.Close()
	})
	if !sameErrno(ce, le) {
		t.Fatalf("create empty: cloud=%v local=%v", ce, le)
	}
	p.settle(t)
	cs, ls, _, _ := p.bothDo(t, func(root string) (any, error) {
		return os.Stat(filepath.Join(root, "empty.txt"))
	})
	if cs.(os.FileInfo).Size() != 0 || ls.(os.FileInfo).Size() != 0 {
		t.Fatalf("empty file sizes: cloud=%d local=%d", cs.(os.FileInfo).Size(), ls.(os.FileInfo).Size())
	}
	cr, lr, _, _ := p.bothDo(t, func(root string) (any, error) {
		return os.ReadFile(filepath.Join(root, "empty.txt"))
	})
	if len(cr.([]byte)) != 0 || len(lr.([]byte)) != 0 {
		t.Fatal("reading an empty file should return nothing")
	}
}

func TestDeliberateDifferencesAreExplicit(t *testing.T) {
	p := newPair(t)
	for _, root := range []string{p.cloud, p.local} {
		if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p.settle(t)

	// Hard links are not supported: the remotes have no such concept.
	err := os.Link(filepath.Join(p.cloud, "f.txt"), filepath.Join(p.cloud, "hard.txt"))
	if err == nil {
		t.Error("hard links should be refused; the remotes have no equivalent")
	}

	// Symlinks are likewise unsupported.
	err = os.Symlink("f.txt", filepath.Join(p.cloud, "soft.txt"))
	if err == nil {
		t.Error("symlinks should be refused")
	}

	// chmod persists. No remote has anywhere to put a permission bit, so
	// cloudfs holds it itself — and what ls(1) then reports has to be what
	// the same call leaves on a plain local directory.
	for _, root := range []string{p.cloud, p.local} {
		if err := os.Chmod(filepath.Join(root, "f.txt"), 0o600); err != nil {
			t.Errorf("chmod under %s: %v", root, err)
		}
	}
	cinfo, err := os.Stat(filepath.Join(p.cloud, "f.txt"))
	if err != nil {
		t.Fatalf("the file should still be there after chmod: %v", err)
	}
	linfo, err := os.Stat(filepath.Join(p.local, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if cinfo.Mode().Perm() != linfo.Mode().Perm() {
		t.Errorf("mode after chmod = %#o, the local directory has %#o",
			cinfo.Mode().Perm(), linfo.Mode().Perm())
	}

	// Ownership still is not modelled: there is no remote concept to map it
	// onto, and every node reports the mounting user.
	if err := os.Chown(filepath.Join(p.cloud, "f.txt"), os.Getuid(), os.Getgid()); err != nil {
		t.Errorf("chown should be accepted as a no-op, got %v", err)
	}
}

func TestLargeFileRoundTrip(t *testing.T) {
	p := newPair(t)
	// Several blocks, with a partial last block.
	size := 4096*5 + 137
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i % 251)
	}
	for _, root := range []string{p.cloud, p.local} {
		if err := os.WriteFile(filepath.Join(root, "large.bin"), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p.settle(t)
	cr, lr, ce, le := p.bothDo(t, func(root string) (any, error) {
		return os.ReadFile(filepath.Join(root, "large.bin"))
	})
	if ce != nil || le != nil {
		t.Fatalf("read: cloud=%v local=%v", ce, le)
	}
	if !bytes.Equal(cr.([]byte), content) {
		t.Fatalf("cloud content mismatch: %d bytes", len(cr.([]byte)))
	}
	if !bytes.Equal(lr.([]byte), content) {
		t.Fatal("local fixture mismatch")
	}
}

// TestEditorSavePatternWorks covers the way almost every editor writes a file:
// create a temporary file next to the target, then rename it over the target.
// Both steps happen before the upload queue has drained, so the rename has to
// work on a file that exists only as a queued write.
func TestEditorSavePatternWorks(t *testing.T) {
	p := newPair(t)
	for _, root := range []string{p.cloud, p.local} {
		if err := os.WriteFile(filepath.Join(root, "doc.txt"), []byte("original\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Deliberately do NOT settle: the point is that the rename works while the
	// upload is still queued.
	_, _, ce, le := p.bothDo(t, func(root string) (any, error) {
		tmp := filepath.Join(root, ".doc.txt.swp")
		if err := os.WriteFile(tmp, []byte("edited\n"), 0o644); err != nil {
			return nil, err
		}
		return nil, os.Rename(tmp, filepath.Join(root, "doc.txt"))
	})
	if !sameErrno(ce, le) {
		t.Fatalf("editor save: cloud=%v local=%v", ce, le)
	}
	if ce != nil {
		t.Fatalf("the editor save pattern must work: %v", ce)
	}
	cr, lr, _, _ := p.bothDo(t, func(root string) (any, error) {
		return os.ReadFile(filepath.Join(root, "doc.txt"))
	})
	if !bytes.Equal(cr.([]byte), lr.([]byte)) {
		t.Fatalf("after editor save: cloud=%q local=%q", cr, lr)
	}
	// The swap file is gone on both.
	cs, ls, _, _ := p.bothDo(t, func(root string) (any, error) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		return names, nil
	})
	if fmt.Sprint(cs) != fmt.Sprint(ls) {
		t.Fatalf("directory after save: cloud=%v local=%v", cs, ls)
	}

	// It still reads correctly once the upload lands.
	p.settle(t)
	cr, _, _, _ = p.bothDo(t, func(root string) (any, error) {
		return os.ReadFile(filepath.Join(root, "doc.txt"))
	})
	if string(cr.([]byte)) != "edited\n" {
		t.Fatalf("after upload: %q", cr)
	}
}

// TestDeleteBeforeUploadCancelsTheWrite checks that removing a file that has
// not been uploaded yet cancels the queued write instead of leaving it to land
// on the remote after the local file is gone.
func TestDeleteBeforeUploadCancelsTheWrite(t *testing.T) {
	p := newPair(t)
	_, _, ce, le := p.bothDo(t, func(root string) (any, error) {
		f := filepath.Join(root, "scratch.txt")
		if err := os.WriteFile(f, []byte("temporary\n"), 0o644); err != nil {
			return nil, err
		}
		return nil, os.Remove(f)
	})
	if !sameErrno(ce, le) {
		t.Fatalf("create-then-delete: cloud=%v local=%v", ce, le)
	}
	// The queue must be empty: the write was cancelled, not merely orphaned.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st, err := p.j.Stats(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st.Pending == 0 && st.Uploading == 0 {
			if st.Dead != 0 {
				t.Fatalf("cancelled write was dead-lettered: %+v", st)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// And the file is gone on both.
	_, _, ce, le = p.bothDo(t, func(root string) (any, error) {
		return os.Stat(filepath.Join(root, "scratch.txt"))
	})
	if !sameErrno(ce, le) || !os.IsNotExist(ce) {
		t.Fatalf("deleted file: cloud=%v local=%v", ce, le)
	}
}

// TestRepeatedFlushDoesNotStackUploads checks that the kernel's habit of
// sending FLUSH for every closed descriptor does not queue one upload per
// flush. A shell redirection triggers this.
func TestRepeatedFlushDoesNotStackUploads(t *testing.T) {
	p := newPair(t)
	f, err := os.OpenFile(filepath.Join(p.cloud, "flushy.txt"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Duplicating and closing the descriptor makes the kernel send FLUSH while
	// the file stays open and writable.
	dup, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		f.Close()
		t.Skipf("dup unavailable: %v", err)
	}
	if err := syscall.Close(dup); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if _, err := f.WriteString("written after the flush\n"); err != nil {
		f.Close()
		t.Fatalf("writing after a flush must still work: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	p.settle(t)

	got, err := os.ReadFile(filepath.Join(p.cloud, "flushy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "written after the flush\n" {
		t.Fatalf("content = %q", got)
	}
	// One file, one surviving upload record at most.
	all, err := p.j.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var forFile int
	for _, u := range all {
		if u.Name == "flushy.txt" {
			forFile++
		}
	}
	if forFile > 1 {
		t.Fatalf("repeated flushes queued %d uploads for one file", forFile)
	}
}
