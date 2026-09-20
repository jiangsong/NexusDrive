//go:build linux || darwin

package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// forcedDetach reports whether cmd is one of the forced detaches, the ones
// that take down a mount nobody serves.
func forcedDetach(cmd string) bool {
	for _, f := range []string{"umount -f ", "diskutil unmount force ", "fusermount3 -uz ", "fusermount -uz ", "umount -l "} {
		if strings.HasPrefix(cmd, f) {
			return true
		}
	}
	return false
}

// fakeHelpers replaces the unmount helper runner and the staleness probe for
// one test. The polite helpers fail, which is what a mount that refuses to
// come down does; the forced ones succeed, as they do against a mount whose
// server is gone. Every command run is recorded in order.
func fakeHelpers(t *testing.T, stale bool) *[]string {
	t.Helper()
	var ran []string
	origRun, origStale := runHelper, mountIsStale
	t.Cleanup(func() { runHelper, mountIsStale = origRun, origStale })
	runHelper = func(name string, args ...string) ([]byte, error) {
		cmd := strings.Join(append([]string{name}, args...), " ")
		ran = append(ran, cmd)
		if forcedDetach(cmd) {
			return nil, nil
		}
		return []byte("Unmount failed for /mnt/x"), errors.New("exit status 1")
	}
	mountIsStale = func(string) bool { return stale }
	return &ran
}

func TestStaleMountReadsTheErrnoOfBothKernels(t *testing.T) {
	// Linux answers a request to a mount whose server is gone with ENOTCONN;
	// macFUSE answers with ENXIO ("device not configured"). Reading only one
	// of the two leaves every stale mount on the other platform undetected.
	for _, e := range []syscall.Errno{syscall.ENOTCONN, syscall.ENXIO} {
		if !staleErrno(&os.PathError{Op: "open", Path: "/mnt/x", Err: e}) {
			t.Fatalf("errno %d (%v) not recognised as a mount nobody serves", int(e), e)
		}
	}
	for _, e := range []syscall.Errno{syscall.ENOENT, syscall.EACCES, syscall.EBUSY} {
		if staleErrno(&os.PathError{Op: "open", Path: "/mnt/x", Err: e}) {
			t.Fatalf("errno %d (%v) mistaken for a mount nobody serves", int(e), e)
		}
	}
	if staleErrno(nil) {
		t.Fatal("a nil error reported as a mount nobody serves")
	}
}

func TestStaleMountSaysNoForAnOrdinaryDirectory(t *testing.T) {
	dir := t.TempDir()
	if StaleMount(dir) {
		t.Fatalf("%s is a plain directory, not a mount nobody serves", dir)
	}
	if StaleMount(filepath.Join(dir, "absent")) {
		t.Fatal("a path that does not exist reported as a mount nobody serves")
	}
}

func TestUnmountForcesTheDetachOnceTheMountIsStale(t *testing.T) {
	// The polite unmount is exactly what a mount whose server is gone
	// refuses -- on macOS with "Unmount failed for <path>" and nothing
	// else -- so a daemon that died without unmounting blocked every later
	// mount until someone ran umount -f by hand.
	ran := fakeHelpers(t, true)
	if err := Unmount("/mnt/x"); err != nil {
		t.Fatalf("a mount nobody serves was not detached: %v", err)
	}
	for _, c := range *ran {
		if forcedDetach(c) {
			return
		}
	}
	t.Fatalf("no forced detach among the helpers run: %v", *ran)
}

func TestUnmountLeavesALiveMountAlone(t *testing.T) {
	// A live mount that refuses the unmount is busy, not stale. Detaching it
	// anyway would strand whoever is still reading it.
	ran := fakeHelpers(t, false)
	err := Unmount("/mnt/x")
	if err == nil {
		t.Fatal("a busy live mount reported as unmounted")
	}
	if !strings.Contains(err.Error(), "/mnt/x") {
		t.Fatalf("the error does not name the mount point: %v", err)
	}
	for _, c := range *ran {
		if forcedDetach(c) {
			t.Fatalf("a live mount was force-detached: %v", *ran)
		}
	}
}
