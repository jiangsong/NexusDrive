//go:build darwin

package fusefs

import (
	"fmt"
	"os"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// platformName identifies the FUSE implementation in use.
const platformName = "macFUSE"

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
		"noapplexattr",
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
