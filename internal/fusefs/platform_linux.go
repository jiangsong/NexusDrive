//go:build linux

package fusefs

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// platformName identifies the FUSE implementation in use.
const platformName = "libfuse (Linux)"

// errNoAttr is "no such extended attribute", which Linux spells ENODATA.
const errNoAttr = syscall.ENODATA

// applyPlatformOptions adds the Linux-specific mount options: a large
// max_write to match the block size, and the kernel's own read-ahead.
func applyPlatformOptions(o *fuse.MountOptions) {
	o.MaxWrite = 1 << 20
	// go-fuse's own MaxReadAhead doc comment claims the kernel caps this at
	// 128 KiB, but that is stale: since Linux moved read-ahead to
	// max_readahead-derived ra_pages, the kernel honours values well past
	// that, up to MaxWrite. 4 MiB keeps a sequential reader's window ahead
	// of a single 4 MiB block fetch instead of trailing it.
	o.MaxReadAhead = 4 << 20
	if v := os.Getenv("CLOUDFS_KERNEL_READAHEAD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			o.MaxReadAhead = n
		}
	}
	// Let more READ requests stay in flight at once (go-fuse's default of
	// 12 serialises the kernel's own read-ahead into small sequential
	// bursts); this is what turns a `cp`'s 128 KiB reads into pipelined
	// 1 MiB block fetches.
	o.MaxBackground = 64
	// Let the kernel cache pages across opens; the VFS invalidates actively
	// when a write or a delta says the content changed.
	o.ExplicitDataCacheControl = false
	// noatime: a cloud drive has no access time to report, and without it
	// the kernel invalidates a directory's attributes after every readdir
	// (to refresh atime), so the stat that follows each opendir in a tree
	// walk crossed into this process again.
	o.Options = append(o.Options, "noatime")
}

// checkPlatform reports whether this host can mount, and why not when it
// cannot.
func checkPlatform() (bool, string) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		return false, "/dev/fuse is missing; install the fuse3 package"
	}
	f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	if err != nil {
		return false, fmt.Sprintf("/dev/fuse exists but this user cannot open it: %v", err)
	}
	f.Close()
	return true, ""
}

// PassthroughAvailable reports whether the kernel supports FUSE passthrough,
// which lets a fully cached file be read at local-disk speed. It is an
// optimisation: everything works without it.
func PassthroughAvailable() (bool, string) {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return false, "cannot read the kernel version"
	}
	rel := strings.TrimSpace(string(b))
	parts := strings.FieldsFunc(rel, func(r rune) bool { return r == '.' || r == '-' })
	if len(parts) < 2 {
		return false, "unrecognised kernel version " + rel
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false, "unrecognised kernel version " + rel
	}
	if major < 6 || (major == 6 && minor < 9) {
		return false, fmt.Sprintf("kernel %s is older than 6.9, so FUSE passthrough is unavailable", rel)
	}
	// Registering a backing file with the kernel needs CAP_SYS_ADMIN; without
	// it the registration is refused and go-fuse turns passthrough off for
	// the whole mount. Better to say so than to claim a speed-up that is not
	// happening.
	if !hasCapSysAdmin() {
		return false, "FUSE passthrough needs CAP_SYS_ADMIN (run as root or grant the capability); cached reads are served by the cloudfs process"
	}
	return true, ""
}

// hasCapSysAdmin reads the effective capability set from /proc.
func hasCapSysAdmin() bool {
	if os.Geteuid() == 0 {
		return true
	}
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), 16, 64)
		if err != nil {
			return false
		}
		const capSysAdmin = 21
		return v&(1<<capSysAdmin) != 0
	}
	return false
}
