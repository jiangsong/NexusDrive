package vfs

import (
	"context"
	"errors"
	"testing"
	"time"

	"cloudfs/internal/config"
)

// TestChmodIsVisibleToStat: permissions live only in this filesystem, so a
// chmod(2) has to be readable back through the same path the kernel stats.
func TestChmodIsVisibleToStat(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("script.sh", []byte("#!/bin/sh\n"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	node, err := e.store.Resolve(ctx, "/ali/script.sh")
	if err != nil {
		t.Fatal(err)
	}
	at, err := e.fs.Stat(ctx, node.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if at.Mode != 0o644 {
		t.Fatalf("mode of a fresh file = %#o, want %#o", at.Mode, 0o644)
	}
	if err := e.fs.Chmod(ctx, node.Ino, 0o755); err != nil {
		t.Fatal(err)
	}
	at, err = e.fs.Stat(ctx, node.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if at.Mode != 0o755 {
		t.Fatalf("mode after chmod = %#o, want %#o", at.Mode, 0o755)
	}
}

// TestChmodSurvivesADirectoryGoingStale is the case that sends git looking at
// a mode-only diff: the exec bit has to outlive the refresh that follows the
// directory's TTL expiring.
func TestChmodSurvivesADirectoryGoingStale(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("script.sh", []byte("#!/bin/sh\n"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	node, err := e.store.Resolve(ctx, "/ali/script.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Chmod(ctx, node.Ino, 0o755); err != nil {
		t.Fatal(err)
	}
	listings := e.fake.Calls("List")
	e.clk.advance(2 * time.Minute)
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	if e.fake.Calls("List") == listings {
		t.Fatal("the directory did not refresh, so this proves nothing")
	}
	at, err := e.fs.Stat(ctx, node.Ino)
	if err != nil {
		t.Fatal(err)
	}
	if at.Mode != 0o755 {
		t.Fatalf("mode after refresh = %#o, want %#o", at.Mode, 0o755)
	}
}

// TestChmodRefusedOnReadonlyMount: nothing in a read-only subtree changes,
// and permissions are part of the file as the person sees it.
func TestChmodRefusedOnReadonlyMount(t *testing.T) {
	e := newEnv(t, envOpt{mode: config.ModeReadonly})
	ctx := context.Background()
	e.fake.Seed("locked.sh", []byte("#!/bin/sh\n"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
	node, err := e.store.Resolve(ctx, "/ali/locked.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Chmod(ctx, node.Ino, 0o755); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("chmod on a read-only mount = %v, want ErrReadOnly", err)
	}
}
