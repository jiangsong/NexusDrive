package fusefs

import (
	"strings"
	"testing"
)

func TestPassthroughRequiresExplicitExperimentalOptIn(t *testing.T) {
	t.Setenv("CLOUDFS_NO_PASSTHROUGH", "")
	t.Setenv("CLOUDFS_EXPERIMENTAL_PASSTHROUGH", "")
	available := func() (bool, string) { return true, "" }
	if ok, why := passthroughEnabled(available); ok || !strings.Contains(why, "not yet safe") {
		t.Fatalf("unsafe auto-enable: %v %s", ok, why)
	}
	t.Setenv("CLOUDFS_EXPERIMENTAL_PASSTHROUGH", "1")
	if ok, why := passthroughEnabled(available); !ok {
		t.Fatal(why)
	}
	if ok, why := passthroughEnabled(func() (bool, string) { return false, "missing kernel support" }); ok || why != "missing kernel support" {
		t.Fatalf("opt-in bypassed capabilities: %v %s", ok, why)
	}
	t.Setenv("CLOUDFS_NO_PASSTHROUGH", "1")
	if ok, why := passthroughEnabled(available); ok || !strings.Contains(why, "CLOUDFS_NO_PASSTHROUGH") {
		t.Fatalf("kill switch lost precedence: %v %s", ok, why)
	}
}
