package perf

import (
	"context"
	"testing"

	"cloudfs/internal/vfs"
)

// small_file_whole is the policy that says a file below the small-file
// threshold is worth fetching in one request instead of a block at a time.
// The sibling prefetch in read_dir_ahead.go already does that for files it
// pulls speculatively; these tests are about the foreground read, which is
// the only path an agent reading one scattered file at a time ever takes.
//
// The observable is the second read. Fetching a block at a time means the
// tail of a file costs its own range request; fetching the file whole means
// the first read paid for all of it.

const (
	smallFileSize = 100 << 10 // 100 KiB, well above the 4 KiB block size
	smallFileHead = 128
)

func seedOneFile(t *testing.T, r *dirAheadRig, path string, size int) {
	t.Helper()
	content := make([]byte, size)
	for i := range content {
		content[i] = byte('a' + i%26)
	}
	r.fake.Seed(path, content)
}

// readHeadThenTail reads the first bytes of a file and then its last bytes,
// reporting what the tail cost on top of the head.
func readHeadThenTail(t *testing.T, r *dirAheadRig, path string, size int) int {
	t.Helper()
	ctx := context.Background()
	if _, err := r.fs.ReadFileRange(ctx, path, 0, smallFileHead); err != nil {
		t.Fatal(err)
	}
	r.settle(t)
	before := r.fake.Calls("ReadRange")
	got, err := r.fs.ReadFileRange(ctx, path, int64(size-smallFileHead), smallFileHead)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != smallFileHead {
		t.Fatalf("tail read %d bytes, want %d", len(got), smallFileHead)
	}
	return r.fake.Calls("ReadRange") - before
}

// TestSmallFileWholeFetchesTheWholeFileOnce: with the policy on, touching a
// small file brings all of it down, so any later read of the same file is
// local. One directory entry, one read to start: the sibling prefetch needs
// three reads in listing order to arm, so nothing here is its doing.
func TestSmallFileWholeFetchesTheWholeFileOnce(t *testing.T) {
	policy := smallFilePolicy()
	r := newDirAheadRig(t, policy)
	seedOneFile(t, r, "notes/only.txt", smallFileSize)
	if _, err := r.fs.ReadDirPath(context.Background(), "/notes"); err != nil {
		t.Fatal(err)
	}
	before := r.fake.Calls("ReadRange")
	if extra := readHeadThenTail(t, r, "/notes/only.txt", smallFileSize); extra != 0 {
		t.Errorf("the tail of a whole-fetched file cost %d range requests, want 0", extra)
	}
	// The whole file must arrive in one request, not one per block.
	if total := r.fake.Calls("ReadRange") - before; total != 1 {
		t.Errorf("reading a %d KiB file cost %d range requests, want 1", smallFileSize>>10, total)
	}
}

// TestWithoutSmallFileWholeTheTailCostsItsOwnRequest is the control. It
// pins what the policy actually changes: with it off, the same two reads
// fetch two separate blocks. Without this the test above would still pass
// if the file were being fetched whole for some unrelated reason.
func TestWithoutSmallFileWholeTheTailCostsItsOwnRequest(t *testing.T) {
	policy := smallFilePolicy()
	policy.SmallFileWhole = false
	r := newDirAheadRig(t, policy)
	seedOneFile(t, r, "notes/only.txt", smallFileSize)
	if _, err := r.fs.ReadDirPath(context.Background(), "/notes"); err != nil {
		t.Fatal(err)
	}
	if extra := readHeadThenTail(t, r, "/notes/only.txt", smallFileSize); extra == 0 {
		t.Error("the tail was free with small_file_whole off; the policy is not what made the difference")
	}
}

// TestSmallFileWholeHasItsOwnThreshold: the foreground fetch and the sibling
// prefetch are bounded separately, because they are spending different money.
// The prefetch is a guess and wants a small bound; the foreground fetch serves
// a read somebody is waiting on and only pays off above the block size, which
// a source-tree bound of a megabyte is not once block_size is 4 MiB. With one
// threshold for both, the `code` preset's megabyte silently turned the
// foreground half off.
func TestSmallFileWholeHasItsOwnThreshold(t *testing.T) {
	policy := vfs.CachePolicy{
		DirReadahead:            32,
		SmallFileThreshold:      4 << 10, // too small for this file to be prefetched
		SmallFileWholeThreshold: 1 << 20, // but a foreground read may take it whole
		SmallFileWhole:          true,
	}
	r := newDirAheadRig(t, policy)
	seedOneFile(t, r, "src/main.go", smallFileSize)
	if _, err := r.fs.ReadDirPath(context.Background(), "/src"); err != nil {
		t.Fatal(err)
	}
	if extra := readHeadThenTail(t, r, "/src/main.go", smallFileSize); extra != 0 {
		t.Errorf("the tail cost %d range requests: the whole-file bound was not used, the prefetch bound was", extra)
	}
}

// TestSmallFileWholeLeavesBigFilesAlone: the threshold is the point. A file
// above it must keep streaming block by block, or a mount holding video
// would pull whole films to serve a seek.
func TestSmallFileWholeLeavesBigFilesAlone(t *testing.T) {
	policy := vfs.CachePolicy{DirReadahead: 32, SmallFileThreshold: 16 << 10, SmallFileWhole: true}
	r := newDirAheadRig(t, policy)
	seedOneFile(t, r, "notes/big.txt", smallFileSize)
	if _, err := r.fs.ReadDirPath(context.Background(), "/notes"); err != nil {
		t.Fatal(err)
	}
	if extra := readHeadThenTail(t, r, "/notes/big.txt", smallFileSize); extra == 0 {
		t.Errorf("a %d KiB file was fetched whole under a %d KiB threshold", smallFileSize>>10, 16)
	}
}
