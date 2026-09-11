package vfs

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/meta"
	"cloudfs/test/fakeprovider"
)

// TestMountPolicyReadaheadMaxOverridesGlobalWindow: two mounts share the same
// global Options (ReadAheadBlocks left low), but one mount's Policy sets a
// generous ReadaheadMax and the other's leaves it at zero. After three
// sequential reads arm the sequential window (seqArm), the mount with the
// larger effective window issues more ReadRange calls than the one that
// never grows past a single block, because it fires background prefetch and
// the other mount does not.
func TestMountPolicyReadaheadMaxOverridesGlobalWindow(t *testing.T) {
	const block = 4096
	const blocks = 64
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ca, err := cache.New(cache.Options{
		Dir: filepath.Join(dir, "cache"), BlockSize: block,
		FreeSpace: func(string) (int64, error) { return 1 << 40, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })

	mediaFake := fakeprovider.New("media")
	codeFake := fakeprovider.New("code")
	content := bytes.Repeat([]byte("x"), block*blocks)
	mediaFake.Seed("big.bin", content)
	codeFake.Seed("big.bin", content)

	fs, err := New(Options{
		Meta: store, Cache: ca, AttrTTL: time.Minute, DefaultDirTTL: time.Minute,
		// A small global window: the "code" mount, which never overrides
		// ReadaheadMax, inherits exactly this and so barely grows.
		ReadAheadBlocks:  1,
		ReadaheadRequest: block, // pin coalescing so it does not blur the count
		Mounts: []Mount{
			{
				Prefix: "/media", Remote: "media", RootID: fakeprovider.RootID, Provider: mediaFake,
				Policy: CachePolicy{ReadaheadMax: 8 * block},
			},
			{
				Prefix: "/code", Remote: "code", RootID: fakeprovider.RootID, Provider: codeFake,
				// No Policy override: falls back to Options.ReadAheadBlocks (1).
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	ctx := context.Background()

	readThree := func(prefix string, fake *fakeprovider.Fake) int {
		if _, err := fs.ReadDirPath(ctx, prefix); err != nil {
			t.Fatal(err)
		}
		node, err := store.Resolve(ctx, prefix+"/big.bin")
		if err != nil {
			t.Fatal(err)
		}
		h, err := fs.Open(ctx, node.Ino, false)
		if err != nil {
			t.Fatal(err)
		}
		defer fs.Release(ctx, h)
		buf := make([]byte, block)
		for i := int64(0); i < 3; i++ {
			if _, err := fs.Read(ctx, h, buf, i*block); err != nil {
				t.Fatal(err)
			}
		}
		// Give any background prefetch launched by the third (arming) read
		// time to reach the provider.
		time.Sleep(150 * time.Millisecond)
		return fake.Calls("ReadRange")
	}

	mediaCalls := readThree("/media", mediaFake)
	codeCalls := readThree("/code", codeFake)

	if mediaCalls <= codeCalls {
		t.Fatalf("media mount (ReadaheadMax=%d) issued %d ReadRange calls, code mount (no override, global=1 block) issued %d; want media > code",
			8*block, mediaCalls, codeCalls)
	}
	// The code mount, capped at a 1-block window, must not have prefetched
	// anything beyond its three demand reads.
	if codeCalls > 3 {
		t.Fatalf("code mount issued %d ReadRange calls for 3 demand reads with no readahead room, want <= 3", codeCalls)
	}
}
