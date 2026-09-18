//go:build darwin

package fusefs

import (
	"fmt"
	"os"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// platformName identifies the FUSE implementation in use.
const platformName = "macFUSE"

// errNoAttr is "no such extended attribute". macOS gives it its own number;
// Linux spells the same fact ENODATA, and a tool that checks for the wrong one
// reads a missing attribute as an unexpected error.
const errNoAttr = syscall.ENOATTR

// applyPlatformOptions adds the macOS-specific mount options. macFUSE caps
// max_write far below Linux, and Finder wants a volume name.
func applyPlatformOptions(o *fuse.MountOptions) {
	// macFUSE negotiates a smaller maximum write than Linux; asking for more
	// makes the mount fail outright rather than degrade.
	o.MaxWrite = 1 << 16
	o.MaxReadAhead = 1 << 16
	o.Options = append(o.Options,
		"volname=cloudfs",
		// Keep Finder from writing .DS_Store into every remote directory,
		// which would create a write per browsed folder.
		"noappledouble",
		// Deliberately not "noapplexattr": it answers every com.apple.*
		// extended-attribute call with EPERM, and copyfile(3) — which is
		// what the Finder, cp -p and ditto all copy with — stops on EPERM
		// and reports "you do not have permission to access some of these
		// items" for the whole copy. The filesystem stores no extended
		// attributes, and the way to say that is ENOTSUP, which copyfile
		// skips over; see the node's Setxattr.
		// UNVERIFIED: macFUSE 4.x honours iosize; Fuse-T ignores it.
		"iosize=1048576",
	)
}

// checkPlatform reports whether this host can mount, and why not when it
// cannot.
func checkPlatform() (bool, string) {
	for _, p := range []string{"/Library/Filesystems/macfuse.fs", "/Library/Filesystems/fuse-t.fs"} {
		if _, err := os.Stat(p); err == nil {
			return true, ""
		}
	}
	if _, err := os.Stat("/dev/macfuse0"); err == nil {
		return true, ""
	}
	return false, "neither macFUSE nor Fuse-T is installed; get macFUSE from https://macfuse.io (needs a reboot to approve the system extension) or Fuse-T from https://www.fuse-t.org for a kext-free option"
}

// PassthroughAvailable reports whether FUSE passthrough is usable. macFUSE has
// no equivalent, so cached reads always go through the daemon.
func PassthroughAvailable() (bool, string) {
	return false, fmt.Sprintf("%s has no passthrough mode; cached reads are served by the cloudfs process", platformName)
}
