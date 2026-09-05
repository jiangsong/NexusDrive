package fusefs

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// TestFlushSurvivesAnInterruptedRequest is the regression for a defect that
// only shows up under a real workload: the kernel cancels a FUSE request when
// the calling thread takes a signal, and Go preempts its own threads with
// SIGURG at arbitrary points. A commit that gave up on cancellation therefore
// made close(2) fail with EINTR at random — and close(2) cannot be retried, so
// the caller lost the write with no way to recover.
func TestFlushSurvivesAnInterruptedRequest(t *testing.T) {
	e := newMount(t, "")
	ctx := context.Background()

	h, err := e.fs.Create(ctx, rootIno(t, e), "interrupted.txt")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("this write must survive the interrupt")
	if _, err := e.fs.Write(ctx, h, body, 0); err != nil {
		t.Fatal(err)
	}
	f := &file{root: e.mnt.root, handle: h}

	// A context that is already cancelled stands in for the interrupt the
	// kernel delivers mid-close.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	if errno := f.Flush(cancelled); errno != 0 {
		t.Fatalf("Flush on an interrupted request returned %v; close(2) would fail with it", errno)
	}
	if errno := f.Release(cancelled); errno != 0 && errno != syscall.EBADF {
		t.Fatalf("Release on an interrupted request returned %v", errno)
	}
	// The data is committed and readable, which is what close() returning
	// success promised.
	got, err := os.ReadFile(filepath.Join(e.dir, "interrupted.txt"))
	if err != nil {
		t.Fatalf("the file close() reported as written is not readable: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("content = %q, want %q", got, body)
	}
}

// rootIno returns the inode of the mount root as the VFS numbers it.
func rootIno(t *testing.T, e *mountEnv) uint64 {
	t.Helper()
	at, err := e.fs.StatPath(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	return at.Ino
}

// TestMutationsSurviveAnInterruptedRequest: the benchmark's dataset creation
// failed at random with ENOENT because a CREATE whose request the kernel had
// already interrupted ran on a cancelled context and reported the cancellation
// as "not found". Every operation that changes the tree must finish and answer
// the request it was given.
func TestMutationsSurviveAnInterruptedRequest(t *testing.T) {
	e := newMount(t, "")
	root := e.mnt.rootNode
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	var out fuse.EntryOut
	dirInode, errno := root.Mkdir(cancelled, "d", 0o755, &out)
	if errno != 0 {
		t.Fatalf("mkdir on an interrupted request: %v", errno)
	}
	dir := dirInode.Operations().(*node)
	_, fh, _, errno := dir.Create(cancelled, "f.txt", uint32(os.O_WRONLY|os.O_CREATE), 0o644, &out)
	if errno != 0 {
		t.Fatalf("create on an interrupted request: %v", errno)
	}
	f := fh.(*file)
	if _, errno := f.Write(context.Background(), []byte("payload"), 0); errno != 0 {
		t.Fatalf("write: %v", errno)
	}
	if errno := f.Flush(cancelled); errno != 0 {
		t.Fatalf("flush: %v", errno)
	}
	f.Release(cancelled)
	if errno := dir.Rename(cancelled, "f.txt", dir, "g.txt", 0); errno != 0 {
		t.Fatalf("rename on an interrupted request: %v", errno)
	}
	got, err := os.ReadFile(filepath.Join(e.dir, "d", "g.txt"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("after rename: %q, %v", got, err)
	}
	if errno := dir.Unlink(cancelled, "g.txt"); errno != 0 {
		t.Fatalf("unlink on an interrupted request: %v", errno)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "d", "g.txt")); !os.IsNotExist(err) {
		t.Fatalf("file survived unlink: %v", err)
	}
	if errno := root.Rmdir(cancelled, "d"); errno != 0 {
		t.Fatalf("rmdir on an interrupted request: %v", errno)
	}
}
