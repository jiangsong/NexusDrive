package fusefs

import "os"

// PassthroughEnabled is the current activation policy, separate from platform
// capability. The experimental opt-in is a temporary safety gate, not a fix for
// mixed IO modes. See docs/fuse-passthrough.md and the open T-08 work item.
func PassthroughEnabled() (bool, string) {
	return passthroughEnabled(PassthroughAvailable)
}

func passthroughEnabled(available func() (bool, string)) (bool, string) {
	if os.Getenv("CLOUDFS_NO_PASSTHROUGH") != "" {
		return false, "disabled by CLOUDFS_NO_PASSTHROUGH"
	}
	if os.Getenv("CLOUDFS_EXPERIMENTAL_PASSTHROUGH") != "1" {
		return false, "experimental passthrough is disabled: mixed read/write and version transitions are not yet safe"
	}
	return available()
}
