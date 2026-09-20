//go:build !windows

package fusefs

import (
	"fmt"
	"sort"
	"testing"

	"cloudfs/internal/config"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// kernelCaps names the INIT capability bits go-fuse knows about, so a report
// can say which ones this kernel offers rather than printing a bitmask.
var kernelCaps = map[string]uint64{
	"ASYNC_READ":           fuse.CAP_ASYNC_READ,
	"POSIX_LOCKS":          fuse.CAP_POSIX_LOCKS,
	"FILE_OPS":             fuse.CAP_FILE_OPS,
	"ATOMIC_O_TRUNC":       fuse.CAP_ATOMIC_O_TRUNC,
	"EXPORT_SUPPORT":       fuse.CAP_EXPORT_SUPPORT,
	"BIG_WRITES":           fuse.CAP_BIG_WRITES,
	"DONT_MASK":            fuse.CAP_DONT_MASK,
	"SPLICE_WRITE":         fuse.CAP_SPLICE_WRITE,
	"SPLICE_MOVE":          fuse.CAP_SPLICE_MOVE,
	"SPLICE_READ":          fuse.CAP_SPLICE_READ,
	"FLOCK_LOCKS":          fuse.CAP_FLOCK_LOCKS,
	"IOCTL_DIR":            fuse.CAP_IOCTL_DIR,
	"AUTO_INVAL_DATA":      fuse.CAP_AUTO_INVAL_DATA,
	"READDIRPLUS":          fuse.CAP_READDIRPLUS,
	"READDIRPLUS_AUTO":     fuse.CAP_READDIRPLUS_AUTO,
	"ASYNC_DIO":            fuse.CAP_ASYNC_DIO,
	"WRITEBACK_CACHE":      fuse.CAP_WRITEBACK_CACHE,
	"NO_OPEN_SUPPORT":      fuse.CAP_NO_OPEN_SUPPORT,
	"PARALLEL_DIROPS":      fuse.CAP_PARALLEL_DIROPS,
	"HANDLE_KILLPRIV":      fuse.CAP_HANDLE_KILLPRIV,
	"POSIX_ACL":            fuse.CAP_POSIX_ACL,
	"ABORT_ERROR":          fuse.CAP_ABORT_ERROR,
	"MAX_PAGES":            fuse.CAP_MAX_PAGES,
	"CACHE_SYMLINKS":       fuse.CAP_CACHE_SYMLINKS,
	"SECURITY_CTX":         fuse.CAP_SECURITY_CTX,
	"HAS_INODE_DAX":        fuse.CAP_HAS_INODE_DAX,
	"CREATE_SUPP_GROUP":    fuse.CAP_CREATE_SUPP_GROUP,
	"HAS_EXPIRE_ONLY":      fuse.CAP_HAS_EXPIRE_ONLY,
	"DIRECT_IO_ALLOW_MMAP": fuse.CAP_DIRECT_IO_ALLOW_MMAP,
	"PASSTHROUGH":          fuse.CAP_PASSTHROUGH,
	"NO_EXPORT_SUPPORT":    fuse.CAP_NO_EXPORT_SUPPORT,
	"HAS_RESEND":           fuse.CAP_HAS_RESEND,
	"ALLOW_IDMAP":          fuse.CAP_ALLOW_IDMAP,
	"OVER_IO_URING":        fuse.CAP_OVER_IO_URING,
	"REQUEST_TIMEOUT":      fuse.CAP_REQUEST_TIMEOUT,
}

// TestKernelOffersCapabilities records what this kernel actually offers in the
// FUSE INIT handshake.
//
// Two optimisations in this package are gated on capabilities the kernel has to
// offer first, and go-fuse can only keep a capability the kernel proposed:
// initFuse masks the reply with a fixed set plus MountOptions.ExtraCapabilities
// (go-fuse fuse/opcode.go), so a bit the kernel never sent cannot be turned on
// from here.
//
//   - READDIRPLUS decides whether dirHandle.Lookup is ever reached. Without it
//     the kernel sends a separate LOOKUP per directory entry and the
//     zero-query listing answer in OpendirHandle is dead code; MountFS reads
//     this same bit to turn on the compensating entry cache instead
//     (entry_cache.go).
//   - WRITEBACK_CACHE decides whether the kernel may buffer writes. Note that
//     the "conflicts with passthrough" reason recorded in MountOptions is a
//     Linux-only conflict: PassthroughAvailable reports false on macOS, so
//     there is nothing to trade away there.
//
// This is a report, not an assertion: the answer differs per platform and per
// FUSE implementation, and hard-coding either one would make the test a
// statement about the machine that last ran it.
func TestKernelOffersCapabilities(t *testing.T) {
	env := newMount(t, config.ModeWriteback)
	settings := env.mnt.server.KernelSettings()
	if settings == nil {
		t.Fatal("no INIT settings recorded; the mount did not complete a handshake")
	}
	offered := settings.Flags64()

	var have, missing []string
	for name, bit := range kernelCaps {
		if offered&bit != 0 {
			have = append(have, name)
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(have)
	sort.Strings(missing)

	t.Logf("FUSE implementation: %s", platformName)
	t.Logf("protocol version: %d.%d", settings.Major, settings.Minor)
	t.Logf("raw flags: %#016x", offered)
	t.Logf("offered (%d): %v", len(have), have)
	t.Logf("not offered (%d): %v", len(missing), missing)

	for _, q := range []struct {
		name string
		bit  uint64
		why  string
	}{
		{"READDIRPLUS", fuse.CAP_READDIRPLUS, "dirHandle.Lookup answers readdirplus entries with no meta query"},
		{"READDIRPLUS_AUTO", fuse.CAP_READDIRPLUS_AUTO, "kernel adapts between readdir and readdirplus"},
		{"WRITEBACK_CACHE", fuse.CAP_WRITEBACK_CACHE, "kernel buffers writes instead of one WRITE per chunk"},
		{"PASSTHROUGH", fuse.CAP_PASSTHROUGH, "cached reads bypass this process"},
		{"SPLICE_READ", fuse.CAP_SPLICE_READ, "read replies move by pipe rather than copy"},
		{"ATOMIC_O_TRUNC", fuse.CAP_ATOMIC_O_TRUNC, "O_TRUNC arrives with OPEN instead of a separate SETATTR"},
		{"PARALLEL_DIROPS", fuse.CAP_PARALLEL_DIROPS, "lookups in one directory run concurrently"},
		{"NO_OPEN_SUPPORT", fuse.CAP_NO_OPEN_SUPPORT, "kernel may skip OPEN entirely"},
	} {
		t.Logf("  %-18s %-5v  %s", q.name, offered&q.bit != 0, q.why)
	}

	t.Logf("MaxWrite requested by this mount: %s", byteCount(int64(maxWriteFor())))
}

// maxWriteFor reports the MaxWrite this package asks for on this platform.
func maxWriteFor() int {
	var o fuse.MountOptions
	applyPlatformOptions(&o)
	return o.MaxWrite
}

func byteCount(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}
