package cache

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSidecarAtAnotherGranularityIsDropped: a cache directory written with
// 64 KiB sub-blocks reopened at 16 KiB would read every bitmap as covering
// the wrong ranges and serve zeros for bytes it never fetched. Reload must
// discard partial blocks whose sidecar granularity is not the current one.
func TestSidecarAtAnotherGranularityIsDropped(t *testing.T) {
	dir := t.TempDir()
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v"}
	const block = 64 << 10
	c, err := newClosingCache(t, Options{Dir: dir, BlockSize: block, SubBlockSize: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("s"), 16<<10)
	if err := c.PutRange(k, 0, 0, data, 2*block); err != nil {
		t.Fatal(err)
	}
	if !c.HasRange(k, 0, 0, 16<<10) {
		t.Fatal("sub-block not present after PutRange")
	}
	waitFlushed(t, c)

	// Same granularity: the partial block survives a reload.
	c, err = newClosingCache(t, Options{Dir: dir, BlockSize: block, SubBlockSize: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if !c.HasRange(k, 0, 0, 16<<10) {
		t.Fatal("partial block lost across a reload at the same granularity")
	}

	// Another granularity: it must be dropped, not misread.
	c, err = newClosingCache(t, Options{Dir: dir, BlockSize: block, SubBlockSize: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if c.HasRange(k, 0, 0, 16<<10) {
		t.Fatal("a 16 KiB sidecar was accepted by a 32 KiB cache")
	}
	var sidecars int
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, ".part") {
			sidecars++
		}
		return nil
	})
	if sidecars != 0 {
		t.Fatalf("%d stale sidecars left on disk", sidecars)
	}
}

// TestFullDiskIsCheckedAgainOnEveryWrite: the free-space check is throttled
// to once a second, but a failed check must not be what the throttle
// remembers, or a full disk would admit blocks until the second was up.
func TestFullDiskIsCheckedAgainOnEveryWrite(t *testing.T) {
	c, err := newClosingCache(t, Options{Dir: t.TempDir(), BlockSize: 4096, MinFree: 1 << 30,
		FreeSpace: func(string) (int64, error) { return 0, nil }})
	if err != nil {
		t.Fatal(err)
	}
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v"}
	for i := 0; i < 3; i++ {
		if err := c.PutAsync(k, int64(i), make([]byte, 4096), 3*4096); err == nil {
			t.Fatalf("write %d was admitted on a full disk", i)
		}
		if err := c.PutRange(k, int64(i), 0, make([]byte, 4096), 3*4096); err == nil {
			t.Fatalf("range write %d was admitted on a full disk", i)
		}
	}
	if s := c.Stats(); s.Blocks != 0 {
		t.Fatalf("blocks cached on a full disk: %+v", s)
	}
}

// TestPartialBlockFileNeverExistsWithoutItsClaim: reload reads a block file
// with no sidecar as a whole block, so a crash between creating a partial
// block file and claiming it would hand out bytes that were never fetched.
// The claim goes to disk first.
func TestPartialBlockFileNeverExistsWithoutItsClaim(t *testing.T) {
	dir := t.TempDir()
	c, err := newClosingCache(t, Options{Dir: dir, BlockSize: 64 << 10, SubBlockSize: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v"}
	if err := c.PutRange(k, 0, 0, bytes.Repeat([]byte("s"), 16<<10), 2*64<<10); err != nil {
		t.Fatal(err)
	}
	block := c.blockPath(k.hash(), 0)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(block); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the block file never reached disk")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(c.sidecarPath(k.hash(), 0)); err != nil {
		t.Fatalf("block file on disk with no claim beside it: %v", err)
	}
}

// TestClaimsCatchUpAfterTheWritesStop: the sidecar is allowed to lag the data
// while sub-blocks keep arriving — that is what keeps a random-read pass from
// rewriting a claim per miss — but it must catch up once the queue is quiet,
// or a restart would refetch everything the cache already holds.
func TestClaimsCatchUpAfterTheWritesStop(t *testing.T) {
	dir := t.TempDir()
	c, err := newClosingCache(t, Options{Dir: dir, BlockSize: 64 << 10, SubBlockSize: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v"}
	for _, off := range []int64{0, 16 << 10} {
		if err := c.PutRange(k, 0, off, bytes.Repeat([]byte("s"), 16<<10), 2*64<<10); err != nil {
			t.Fatal(err)
		}
	}
	waitFlushed(t, c) // waits for the sweep, forces nothing
	c2, err := newClosingCache(t, Options{Dir: dir, BlockSize: 64 << 10, SubBlockSize: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if !c2.HasRange(k, 0, 0, 32<<10) {
		t.Fatal("the claim never caught up: a reload lost sub-blocks that are on disk")
	}
}

// TestQuietBlocksGetTheirClaimWithoutAShutdown: the flush leaves the claim
// behind on purpose, so something has to write it while the daemon runs, or a
// crash would throw away a cache the process was still filling.
func TestQuietBlocksGetTheirClaimWithoutAShutdown(t *testing.T) {
	dir := t.TempDir()
	c, clk := newTest(t, Options{Dir: dir, BlockSize: 64 << 10, SubBlockSize: 16 << 10})
	t.Cleanup(func() { c.Close() })
	k := FileKey{Remote: "r", RemoteID: "id", Version: "v"}
	data := bytes.Repeat([]byte("s"), 16<<10)
	if err := c.PutRange(k, 0, 0, data, 2*64<<10); err != nil {
		t.Fatal(err)
	}
	for c.WriteBehindPending() != 0 {
		time.Sleep(time.Millisecond)
	}
	if !c.claimsLag() {
		t.Skip("the claim was written inside the flush; nothing to sweep")
	}
	// Time passes and another block is written: the worker sweeps what has
	// gone quiet, with no help from Close.
	clk.advance(2 * sidecarMaxLag)
	if err := c.PutRange(k, 1, 0, data, 2*64<<10); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(c.sidecarPath(k.hash(), 0)); err == nil {
			p, err := decodePartial(readFile(t, c.sidecarPath(k.hash(), 0)))
			if err == nil && p.have > 0 {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("a quiet block never got its claim written")
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
