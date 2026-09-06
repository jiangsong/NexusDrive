//go:build windows && winfsp

// Package winfs mounts a cloudfs vfs.FS on Windows through WinFsp, using the
// path-oriented cgofuse binding. It is the Windows counterpart of
// internal/fusefs and keeps the same shape: a thin adapter that translates the
// kernel's requests into vfs.FS calls and does no filesystem policy of its own.
//
// It is a separate package rather than a build-tagged fork inside fusefs
// because cgofuse's callback interface is shaped differently from go-fuse's
// (path arguments, not inode nodes), and mixing the two behind one set of files
// would need a Windows variant of every file. fusefs stays the façade the rest
// of the tree calls; on a `-tags winfsp` Windows build a single delegating file
// there forwards to this package.
//
// cgofuse v1.6.0 has a no-cgo Windows backend that loads winfsp-x64.dll at run
// time, so this builds with CGO_ENABLED=0 and cross-compiles from any host.
// What it cannot do is run anywhere but a Windows machine with WinFsp
// installed; every behavioural claim below is therefore marked UNVERIFIED until
// the acceptance checklist in docs/distribution.md has been run on real
// hardware.
package winfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/winfsp/cgofuse/fuse"

	"cloudfs/internal/vfs"
)

// Options mirrors fusefs.Options so callers compile unchanged across platforms.
type Options struct {
	FS              *vfs.FS
	AttrTimeout     time.Duration
	EntryTimeout    time.Duration
	KernelDirCache  bool
	NegativeTimeout time.Duration
	UID, GID        uint32
}

// MountOptions mirrors fusefs.MountOptions.
type MountOptions struct {
	Options
	Path       string
	AllowOther bool
	Debug      bool
	ReadOnly   bool
}

// OpStats mirrors fusefs.OpStats.
type OpStats struct {
	Ops       map[string]int64
	ReadBytes int64
	ReadSizes map[string]int64
}

// Mount is a live WinFsp mount handle.
type Mount struct {
	host *fuse.FileSystemHost
	fsop *filesystem
	path string
	done chan struct{}
	once sync.Once
}

// MountFS mounts opt.FS at opt.Path and serves it in the background. The path
// is either a drive letter (for example "Z:") or an empty NTFS directory.
func MountFS(opt MountOptions) (*Mount, error) {
	if opt.FS == nil {
		return nil, errors.New("winfs: no filesystem to mount")
	}
	if err := VerifyMountable(opt.Path); err != nil {
		return nil, err
	}
	fsop := newFilesystem(opt)
	host := fuse.NewFileSystemHost(fsop)
	// The remote namespace is POSIX and case-sensitive; keep the mount the
	// same so two names differing only in case do not collide. UNVERIFIED:
	// on a volume WinFsp exposes case-insensitively this still risks a clash;
	// see naming.go.
	host.SetCapCaseInsensitive(false)
	// Readdir fills a full stat for every entry, so ask WinFsp not to stat
	// each name again after listing.
	host.SetCapReaddirPlus(true)
	host.SetUseIno(true)

	m := &Mount{host: host, fsop: fsop, path: opt.Path, done: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		// Mount blocks until Unmount; a false return is a mount failure.
		// UNVERIFIED: cgofuse reports failure only through the boolean, so a
		// bad mount point surfaces as a generic error here.
		defer close(m.done)
		ok := host.Mount(opt.Path, mountArgs(opt))
		if !ok {
			select {
			case started <- errors.New("winfs: WinFsp refused the mount; is the driver installed and the mount point free?"):
			default:
			}
		}
	}()
	// Give a failing mount a moment to report before we call it started.
	select {
	case err := <-started:
		return nil, err
	case <-time.After(500 * time.Millisecond):
	}
	return m, nil
}

// ServeBackground is MountFS; the mount already serves in the background.
func ServeBackground(ctx context.Context, opt MountOptions) (*Mount, error) {
	return MountFS(opt)
}

func mountArgs(opt MountOptions) []string {
	args := []string{}
	if opt.Debug {
		args = append(args, "-o", "debug")
	}
	// WinFsp honours a read-only volume through this FUSE option. UNVERIFIED.
	if opt.ReadOnly {
		args = append(args, "-o", "ro")
	}
	return args
}

// Wait blocks until the mount is torn down.
func (m *Mount) Wait() { <-m.done }

// Unmount detaches the mount.
func (m *Mount) Unmount() error {
	var err error
	m.once.Do(func() {
		if !m.host.Unmount() {
			err = errors.New("winfs: WinFsp did not release the mount; a process may still hold a file open")
			return
		}
		<-m.done
	})
	return err
}

// Path reports the mount point.
func (m *Mount) Path() string { return m.path }

// OpStats reports the kernel-request counters.
func (m *Mount) OpStats() OpStats { return m.fsop.snapshot() }

// InvalidateFunc and InvalidateEntryFunc: WinFsp is told about changes through
// host.Notify rather than the pushed-invalidation callbacks go-fuse uses. Until
// that path is wired and verified, these are no-ops, which is correct but
// coarser: a directory changed on the remote is seen after its TTL, not
// immediately. UNVERIFIED.
func (m *Mount) InvalidateFunc() func(ino uint64) { return func(uint64) {} }
func (m *Mount) InvalidateEntryFunc() func(parent uint64, name string) {
	return func(uint64, string) {}
}

// DropKernelCaches has no cheap WinFsp equivalent (there is no single ioctl to
// empty the volume's cache), so a cold measurement on Windows drops only the
// VFS caches. UNVERIFIED.
func (m *Mount) DropKernelCaches() int { return 0 }

// Supported reports whether this build can mount, which on Windows means the
// WinFsp driver is installed.
func Supported() (bool, string) {
	if path, ok := winfspDLL(); ok {
		_ = path
		return true, ""
	}
	return false, "winfs: WinFsp is not installed; get it from https://winfsp.dev and mount again"
}

// Platform names the backend.
func Platform() string { return "WinFsp" }

// VerifyMountable checks the mount point before a mount is attempted. A drive
// letter must be free; a directory must exist and be empty.
func VerifyMountable(path string) error {
	if ok, why := Supported(); !ok {
		return errors.New(why)
	}
	if path == "" {
		return errors.New("winfs: no mount point; use a free drive letter (Z:) or an empty directory")
	}
	if isDriveLetter(path) {
		// A drive letter must not already be a volume.
		if _, err := os.Stat(path + `\`); err == nil {
			return fmt.Errorf("winfs: %s is already in use; choose a free drive letter", path)
		}
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("winfs: mount point %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("winfs: mount point %s is not a directory", path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("winfs: mount point %s: %w", path, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("winfs: mount point %s is not empty", path)
	}
	return nil
}

// PassthroughEnabled and PassthroughAvailable: WinFsp has no equivalent of the
// FUSE passthrough this project uses on Linux, so it is never available here.
func PassthroughEnabled() (bool, string)   { return false, "passthrough is not available on WinFsp" }
func PassthroughAvailable() (bool, string) { return false, "passthrough is not available on WinFsp" }

func isDriveLetter(path string) bool {
	return len(path) == 2 && path[1] == ':' &&
		((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z'))
}

// winfspDLL reports the WinFsp runtime DLL if present. cgofuse loads it by name
// once we call Mount; probing for it first lets `doctor` report a missing
// driver clearly instead of failing at mount time. UNVERIFIED: the install
// path and DLL name must be confirmed against a real WinFsp install.
func winfspDLL() (string, bool) {
	candidates := []string{}
	for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if base == "" {
			continue
		}
		candidates = append(candidates,
			filepath.Join(base, "WinFsp", "bin", "winfsp-x64.dll"),
			filepath.Join(base, "WinFsp", "bin", "winfsp-x86.dll"),
		)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, true
		}
	}
	return "", false
}
