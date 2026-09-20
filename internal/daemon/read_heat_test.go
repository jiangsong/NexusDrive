package daemon

import (
	"context"
	"testing"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/vfs"
)

// TestReadHeatResolvesPathsWhenItFlushes: the observer the daemon installs
// records the inode that was read and nothing else — the path is resolved
// when the batch is written, not on the read path. That is observable
// without a stopwatch: a file renamed between the read and the flush is
// counted under the name it has when the row is written, and a file
// deleted before the flush is counted nowhere at all, because heat is a
// statistic about files that exist rather than a log of names.
func TestReadHeatResolvesPathsWhenItFlushes(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.ReadHeat == nil {
		t.Fatal("heat is on by default; the daemon installed no observer")
	}
	for _, name := range []string{"kept.txt", "before.txt", "gone.txt"} {
		if _, err := d.FS.WriteFile(ctx, "/demo/"+name, []byte("payload"), false); err != nil {
			t.Fatal(err)
		}
	}
	kctx := vfs.FromKernel(ctx)
	for _, name := range []string{"kept.txt", "before.txt", "gone.txt"} {
		if _, err := d.FS.ReadFileRange(kctx, "/demo/"+name, 0, 0); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
	}
	dir, err := d.FS.StatPath(ctx, "/demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.FS.Rename(ctx, dir.Ino, "before.txt", dir.Ino, "after.txt"); err != nil {
		t.Fatal(err)
	}
	if err := d.FS.Remove(ctx, dir.Ino, "gone.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := d.ReadHeat.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	hot, err := d.Agent.HotPaths(ctx, "/", 7, 50)
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]int64{}
	for _, h := range hot {
		reads[h.Path] = h.Reads
	}
	if reads["/demo/kept.txt"] != 1 {
		t.Fatalf("the file nobody touched has %d reads, want 1 (%+v)", reads["/demo/kept.txt"], hot)
	}
	if reads["/demo/after.txt"] != 1 {
		t.Fatalf("the renamed file has %d reads under its current name, want 1 (%+v)", reads["/demo/after.txt"], hot)
	}
	if n, ok := reads["/demo/before.txt"]; ok {
		t.Fatalf("the read was counted under the name the file no longer has (%d reads): the path was resolved on the read path", n)
	}
	if n, ok := reads["/demo/gone.txt"]; ok {
		t.Fatalf("a deleted file has %d heat reads: the path was resolved on the read path", n)
	}
	// The kind of reader is still classified inline, where the request
	// context is still valid.
	for _, h := range hot {
		if h.Path == "/demo/kept.txt" && h.ByKind[agent.ReadByKernel] != 1 {
			t.Fatalf("reader kind lost in the deferral: %+v", h)
		}
	}
}
