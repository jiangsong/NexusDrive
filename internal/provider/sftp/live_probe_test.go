package sftp

import (
	"context"
	"os"
	"testing"

	"cloudfs/internal/provider"
)

// TestLiveProbe talks to a real server. It is skipped unless CLOUDFS_LIVE_SFTP
// names one, so the suite stays hermetic.
func TestLiveProbe(t *testing.T) {
	host := os.Getenv("CLOUDFS_LIVE_SFTP")
	if host == "" {
		t.Skip("set CLOUDFS_LIVE_SFTP=host to run against a real server")
	}
	cfg := map[string]any{
		"host":           host,
		"user":           os.Getenv("CLOUDFS_LIVE_USER"),
		"root":           os.Getenv("CLOUDFS_LIVE_ROOT"),
		"host_key_alias": os.Getenv("CLOUDFS_LIVE_ALIAS"),
	}
	p, err := Factory("live", cfg)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	ctx := context.Background()
	entries, _, err := p.List(ctx, RootID, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, e := range entries {
		t.Logf("%-16s kind=%d size=%d version=%s", e.Name, e.Kind, e.Size, e.Version)
	}
	if len(entries) == 0 {
		t.Log("root is empty")
	}
	var _ provider.Provider = p
}
