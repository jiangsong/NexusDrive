package mcpsrv

import (
	"fmt"
	"strings"
	"testing"

	"cloudfs/internal/vfs"
)

func TestUploadCleanupErrorDoesNotExposePrivateState(t *testing.T) {
	err := mapErr(fmt.Errorf("private-path/private-identity: %w", vfs.ErrUploadPurging), "/allowed/file")
	if err == nil || !strings.Contains(err.Error(), "cleanup is pending") || strings.Contains(err.Error(), "private-") {
		t.Fatalf("cleanup error mapping: %v", err)
	}
}
