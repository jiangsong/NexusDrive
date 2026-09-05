package vfs

import (
	"context"
	"testing"
)

// TestBlobIsCachedUnderTheRemoteKeyBeforeTheNodeMoves is the deterministic
// half of T-00b.
//
// When an upload completes the node switches from its local-only identity to
// the remote id and version. The block cache keys every byte on that identity,
// so if the staged blob is still registered under the old key at the moment
// the node moves, a read arriving in between misses the cache and asks the
// backend for content this process just uploaded. That is where the EIO seen
// once in the wild came from — the backend need not have the new version
// available yet.
//
// The window is well under a millisecond, so a stress test only catches it by
// luck. This asserts the ordering directly instead: at the publication seam,
// which fires immediately after the metadata update, a full read of the file
// must already be served locally. Moving cache.LinkFile after meta.UpdateByIno
// in write.go makes this fail with a non-zero download count.
func TestBlobIsCachedUnderTheRemoteKeyBeforeTheNodeMoves(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	payload := []byte("content that only exists locally until the upload lands")
	if _, err := e.fs.WriteFile(ctx, "/ali/published.txt", payload, false); err != nil {
		t.Fatal(err)
	}

	var (
		phases     []string
		readErr    error
		mismatch   bool
		downloads  int
		hookRanFor int
	)
	e.fs.publishFault = func(phase string) error {
		phases = append(phases, phase)
		if phase != "remote-identity" {
			return nil
		}
		hookRanFor++
		before := e.fake.Calls("ReadRange")
		got, err := e.fs.ReadFileRange(ctx, "/ali/published.txt", 0, int64(len(payload)))
		downloads += e.fake.Calls("ReadRange") - before
		if err != nil {
			readErr = err
			return nil
		}
		if string(got) != string(payload) {
			mismatch = true
		}
		return nil
	}
	if _, err := e.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	e.fs.publishFault = nil

	if hookRanFor != 1 {
		t.Fatalf("the publication seam fired %d times for one upload; phases=%v", hookRanFor, phases)
	}
	if readErr != nil {
		t.Fatalf("reading the file at the moment its node moved failed: %v", readErr)
	}
	if mismatch {
		t.Fatal("the read at the publication boundary returned the wrong bytes")
	}
	if downloads != 0 {
		t.Fatalf("%d backend reads while publishing; the blob must be linked under the "+
			"remote key before the node points there", downloads)
	}
}
