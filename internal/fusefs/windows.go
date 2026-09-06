//go:build windows

// Package fusefs on Windows is a stub. FUSE mounting needs the WinFsp build
// (internal/winfs, built with -tags winfsp and cgo); a plain Windows binary
// links this instead, so cmd/cloudfs compiles and runs everything except a
// kernel mount. Every entry point here reports, honestly, that a mount is not
// available in this build rather than pretending — the same shape
// platform_darwin.go uses when macFUSE is absent.
package fusefs

import (
	"context"
	"errors"
	"time"

	"cloudfs/internal/vfs"
)

const platformName = "unsupported (Windows needs the WinFsp build)"

var errNoMount = errors.New("fusefs: FUSE mounting is not available in this build; use the WinFsp build (built with -tags winfsp) to mount on Windows")

// Options mirrors the real adapter's configuration so callers compile
// unchanged; a Windows build never constructs a working mount from it.
type Options struct {
	FS              *vfs.FS
	AttrTimeout     time.Duration
	EntryTimeout    time.Duration
	KernelDirCache  bool
	NegativeTimeout time.Duration
	UID, GID        uint32
}

// MountOptions mirrors the real one.
type MountOptions struct {
	Options
	Path       string
	AllowOther bool
	Debug      bool
	ReadOnly   bool
}

// OpStats mirrors the real counters; a stub reports none.
type OpStats struct {
	Ops       map[string]int64
	ReadBytes int64
	ReadSizes map[string]int64
}

// Mount is a stub handle. It is never returned by a successful MountFS on
// Windows, so its methods exist only to satisfy the type.
type Mount struct{}

func (m *Mount) Wait()          {}
func (m *Mount) Unmount() error { return errNoMount }
func (m *Mount) Path() string   { return "" }
func (m *Mount) OpStats() OpStats {
	return OpStats{Ops: map[string]int64{}, ReadSizes: map[string]int64{}}
}
func (m *Mount) InvalidateFunc() func(ino uint64) { return func(uint64) {} }
func (m *Mount) InvalidateEntryFunc() func(parent uint64, name string) {
	return func(uint64, string) {}
}
func (m *Mount) DropKernelCaches() int { return 0 }

// MountFS fails: there is no FUSE on this build.
func MountFS(opt MountOptions) (*Mount, error) { return nil, errNoMount }

// ServeBackground fails for the same reason.
func ServeBackground(ctx context.Context, opt MountOptions) (*Mount, error) { return nil, errNoMount }

// Supported reports that this build cannot mount, with the reason.
func Supported() (bool, string) { return false, errNoMount.Error() }

// Platform names the (absent) backend.
func Platform() string { return platformName }

// VerifyMountable always fails on this build.
func VerifyMountable(path string) error { return errNoMount }

// PassthroughEnabled and PassthroughAvailable are never true here.
func PassthroughEnabled() (bool, string)   { return false, "not applicable on Windows" }
func PassthroughAvailable() (bool, string) { return false, "not applicable on Windows" }
