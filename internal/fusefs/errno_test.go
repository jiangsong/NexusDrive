package fusefs

import (
	"fmt"
	"os"
	"syscall"
	"testing"

	"cloudfs/internal/cache"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
)

func TestSpaceErrorsReachKernelAsENOSPC(t *testing.T) {
	for _, err := range []error{vfs.ErrNoSpace, cache.ErrNoSpace, syscall.ENOSPC, &os.PathError{Op: "write", Path: "staging", Err: syscall.ENOSPC}} {
		if got := errno(fmt.Errorf("journal: %w", err)); got != syscall.ENOSPC {
			t.Fatalf("%v became %v", err, got)
		}
	}
}

func TestCancelledUploadIsBusyRatherThanIOFailure(t *testing.T) {
	for _, err := range []error{vfs.ErrUploadCancelled, vfs.ErrUploadPurging} {
		if got := errno(fmt.Errorf("retained version: %w", err)); got != syscall.EBUSY {
			t.Fatalf("stopped upload %v mapped to %v", err, got)
		}
	}
}

// TestUnreachableBackendIsHostDownNotIOError: a file whose every replica
// sits on a drive that cannot be reached is not broken; the kernel is told
// the host is down so a shell can tell the difference.
func TestUnreachableBackendIsHostDownNotIOError(t *testing.T) {
	if got := errno(fmt.Errorf("pool: %w", provider.ErrUnavailable)); got != syscall.EHOSTDOWN {
		t.Fatalf("unreachable backend mapped to %v", got)
	}
}
