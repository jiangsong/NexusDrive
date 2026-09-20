package perf

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"cloudfs/internal/agent"
	"cloudfs/internal/vfs"
)

// TestReadingManyFilesDoesNotQueryPathsInline (docs/agent-first-design.md
// §6.3): an agent shell that greps its way through a tree reads hundreds
// of distinct files, and every one of them is a fresh inode for the read
// observer — the ten-minute debounce in the VFS folds repeated reads of
// one file, not a burst across many. Resolving an inode to a path is a
// recursive SQLite walk to the root, so doing it inside the observer puts
// one such walk on the foreground read path per file read. The observer
// records inodes and resolves them when it writes the batch, thirty
// seconds later and off the read path: this test counts the resolutions
// and demands none of them happen while the reads are happening.
func TestReadingManyFilesDoesNotQueryPathsInline(t *testing.T) {
	const files = 200
	h := newHarness(t, 4096, 0, 0)
	ctx := context.Background()
	for i := 0; i < files; i++ {
		h.fake.Seed(fmt.Sprintf("burst/f%03d.txt", i), []byte("content"))
	}
	if _, err := h.fs.ReadDirPath(ctx, "/burst"); err != nil {
		t.Fatal(err)
	}
	st, err := agent.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// The daemon's wiring (internal/daemon/daemon.go): the kind of reader
	// is classified inline, because the request context is only valid
	// during the call; the path is left to the flush.
	heat := agent.NewReadObserver(st)
	var resolved atomic.Int64
	heat.SetPathResolver(func(ctx context.Context, ino uint64) (string, error) {
		resolved.Add(1)
		return h.fs.Meta().Path(ctx, ino)
	})
	h.fs.SetReadObserver(func(ctx context.Context, ino uint64) {
		if vfs.IsFromKernel(ctx) {
			heat.ObserveIno(ino, agent.ReadByKernel)
		}
	})

	kctx := vfs.FromKernel(ctx)
	for i := 0; i < files; i++ {
		if _, err := h.fs.ReadFileRange(kctx, fmt.Sprintf("/burst/f%03d.txt", i), 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if n := resolved.Load(); n != 0 {
		t.Fatalf("reading %d files resolved %d inodes to paths on the read path, want 0", files, n)
	}
	if p := heat.Pending(); p != files {
		t.Fatalf("the observer holds %d pending reads for %d files, want %d", p, files, files)
	}

	before := h.fake.TotalCalls()
	if err := heat.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if extra := h.fake.TotalCalls() - before; extra != 0 {
		t.Fatalf("the flush cost %d provider calls, want 0", extra)
	}
	if n := resolved.Load(); n != files {
		t.Fatalf("the flush resolved %d inodes, want %d", n, files)
	}
	hot, err := st.HotPaths(ctx, "/burst", 7, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(hot) != files {
		t.Fatalf("heat has %d paths after %d reads, want %d", len(hot), files, files)
	}
	for _, p := range hot {
		if p.Reads != 1 || p.ByKind[agent.ReadByKernel] != 1 {
			t.Fatalf("deferring the path lost the count: %+v", p)
		}
	}
}
