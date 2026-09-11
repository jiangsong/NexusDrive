package daemon

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
	"cloudfs/test/fakeprovider"
)

// TestBuildMountsLeavesReadaheadUnconfiguredByDefault: with no cache.policy
// anywhere in the config, every vfs.Mount buildMounts produces must carry
// ReadaheadMax == 0 and ReadaheadRequest == 0 ("not configured"), so vfs
// falls back to its own Options (Options.ReadAheadBlocks /
// Options.ReadaheadRequest, i.e. CLOUDFS_READAHEAD_BLOCKS /
// CLOUDFS_READAHEAD_REQUEST). Before the fix, presetCachePolicy's "" / "none"
// baseline baked in ReadaheadMax = 64MiB, so this mount would have carried a
// non-zero value and silently shadowed the global tuning on every
// daemon-built mount, not just ones that actually configure cache.policy.
func TestBuildMountsLeavesReadaheadUnconfiguredByDefault(t *testing.T) {
	cfg, err := config.Parse([]byte(`
remotes:
  a: {type: fake}
mounts:
  - path: /m
    layout:
      /a: {remote: a, root: root}
`))
	if err != nil {
		t.Fatal(err)
	}
	providers := map[string]provider.Provider{"a": fakeprovider.New("a")}
	mounts, err := buildMounts(cfg.Mounts[0], providers, map[string]string{}, cfg.Cache.Policy, int64(cfg.Cache.BlockSize))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Fatalf("mounts = %+v", mounts)
	}
	if mounts[0].Policy.ReadaheadMax != 0 || mounts[0].Policy.ReadaheadRequest != 0 {
		t.Fatalf("unconfigured cache.policy must leave ReadaheadMax/ReadaheadRequest at 0, got %+v", mounts[0].Policy)
	}
}

// TestBuildMountsMediaPresetSetsReadaheadMax is the positive counterpart:
// a preset that does set readahead values must reach vfs.Mount.Policy.
func TestBuildMountsMediaPresetSetsReadaheadMax(t *testing.T) {
	cfg, err := config.Parse([]byte(`
cache:
  policy: {preset: media}
remotes:
  a: {type: fake}
mounts:
  - path: /m
    layout:
      /a: {remote: a, root: root}
`))
	if err != nil {
		t.Fatal(err)
	}
	providers := map[string]provider.Provider{"a": fakeprovider.New("a")}
	mounts, err := buildMounts(cfg.Mounts[0], providers, map[string]string{}, cfg.Cache.Policy, int64(cfg.Cache.BlockSize))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Fatalf("mounts = %+v", mounts)
	}
	if mounts[0].Policy.ReadaheadMax != 128<<20 {
		t.Fatalf("media preset should set ReadaheadMax to 128MiB, got %d", mounts[0].Policy.ReadaheadMax)
	}
	if mounts[0].Policy.ReadaheadRequest != 16<<20 {
		t.Fatalf("media preset should set ReadaheadRequest to 16MiB, got %d", mounts[0].Policy.ReadaheadRequest)
	}
}

// TestUnconfiguredPolicyFallsBackToGlobalReadaheadBlocks exercises the real
// production path end to end: config.Parse -> buildMounts -> vfs.New, with
// no cache.policy anywhere and a block size that is deliberately not 4MiB
// (256KiB), so a resurfaced "bake 64MiB into the baseline" bug would compute
// a wildly different (and wildly larger) window cap than intended
// (64MiB / 256KiB = 256 blocks) instead of respecting Options.ReadAheadBlocks
// (the CLOUDFS_READAHEAD_BLOCKS-controlled global, simulated here as 4).
//
// Eight sequential block-aligned reads arm the sequential window (seqArm=3)
// and then let it grow by doubling once per subsequent read. With the window
// correctly capped at 4 blocks, growth stops at 1->2->4 and prefetch never
// reaches more than a handful of blocks past the last demand read,	so the
// total ReadRange calls stay in the ~12 range (8 demand + at most 4
// read-ahead). With the bug reintroduced, the window keeps doubling
// (1->2->4->8->16->32->64) and prefetch reaches all the way to the end of
// the 40-block file, pushing the call count close to 40.
func TestUnconfiguredPolicyFallsBackToGlobalReadaheadBlocks(t *testing.T) {
	const block = 256 << 10 // not 4MiB: a stale 64MiB-baked default would
	// compute an obviously wrong (256-block) window instead of falling back
	// to Options.ReadAheadBlocks.
	const totalBlocks = 40

	cfg, err := config.Parse([]byte(`
cache: {block_size: 256KiB}
remotes:
  a: {type: fake}
mounts:
  - path: /m
    layout:
      /a: {remote: a, root: root}
`))
	if err != nil {
		t.Fatal(err)
	}
	fake := fakeprovider.New("a")
	providers := map[string]provider.Provider{"a": fake}
	mounts, err := buildMounts(cfg.Mounts[0], providers, map[string]string{}, cfg.Cache.Policy, int64(cfg.Cache.BlockSize))
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: this is exactly what TestBuildMountsLeavesReadaheadUnconfiguredByDefault
	// already checks, restated here so a failure of this test's real point
	// (the window cap) is not confused with that one regressing too.
	if mounts[0].Policy.ReadaheadMax != 0 {
		t.Fatalf("test setup: expected an unconfigured ReadaheadMax, got %d", mounts[0].Policy.ReadaheadMax)
	}

	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ca, err := cache.New(cache.Options{
		Dir: filepath.Join(dir, "cache"), BlockSize: block,
		FreeSpace: func(string) (int64, error) { return 1 << 40, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()

	fake.Seed("big.bin", bytes.Repeat([]byte("x"), block*totalBlocks))

	fsys, err := vfs.New(vfs.Options{
		Meta: store, Cache: ca, Mounts: mounts, AttrTTL: time.Minute, DefaultDirTTL: time.Minute,
		// Simulates CLOUDFS_READAHEAD_BLOCKS=4 with no cache.policy set.
		ReadAheadBlocks:  4,
		ReadaheadRequest: block, // pin coalescing so the count reflects blocks, not merged ranges
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	ctx := context.Background()

	if _, err := fsys.ReadDirPath(ctx, "/a"); err != nil {
		t.Fatal(err)
	}
	node, err := store.Resolve(ctx, "/a/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	h, err := fsys.Open(ctx, node.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Release(ctx, h)

	buf := make([]byte, block)
	for i := int64(0); i < 8; i++ {
		if _, err := fsys.Read(ctx, h, buf, i*block); err != nil {
			t.Fatal(err)
		}
	}
	// Give any background prefetch launched by the arming reads time to
	// reach the provider.
	time.Sleep(300 * time.Millisecond)

	calls := fake.Calls("ReadRange")
	if calls > 20 {
		t.Fatalf("8 demand reads with ReadAheadBlocks=4 (no cache.policy) caused %d ReadRange calls, want <= 20; "+
			"this many implies the window cap ignored Options.ReadAheadBlocks (likely a resolved ReadaheadMax baked "+
			"in instead of staying 0)", calls)
	}
	if calls < 8 {
		t.Fatalf("8 demand reads caused only %d ReadRange calls", calls)
	}
}
