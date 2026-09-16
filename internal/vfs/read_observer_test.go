package vfs

import (
	"context"
	"sync"
	"testing"
)

// TestReadObserverReportsOncePerInodeAndReaderKind: the hook fires once
// per inode per window for each kind of reader, whatever the number of
// blocks read, and never when no hook is installed.
func TestReadObserverReportsOncePerInodeAndReaderKind(t *testing.T) {
	e := newEnv(t, envOpt{})
	e.fake.Seed("a.txt", []byte("hello world"))
	var mu sync.Mutex
	got := map[string]int{}
	e.fs.SetReadObserver(func(ctx context.Context, ino uint64) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case IsFromKernel(ctx):
			got["kernel"]++
		default:
			got[OriginName(ctx)]++
		}
	})
	kctx := FromKernel(context.Background())
	for i := 0; i < 5; i++ {
		if _, err := e.fs.ReadFileRange(kctx, "/ali/a.txt", 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	mctx := WithOrigin(context.Background(), "mcp")
	if _, err := e.fs.ReadFileRange(mctx, "/ali/a.txt", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.ReadFileRange(mctx, "/ali/a.txt", 0, 0); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got["kernel"] != 1 || got["mcp"] != 1 || len(got) != 2 {
		t.Fatalf("%v", got)
	}
	e.fs.SetReadObserver(nil)
	mu.Unlock()
	if _, err := e.fs.ReadFileRange(kctx, "/ali/a.txt", 0, 0); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if got["kernel"] != 1 {
		t.Fatal("hook fired after removal")
	}
}
