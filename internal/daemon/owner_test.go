package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/config"
)

// TestSecondOwnerIsRefusedBeforeItOpensAnything: a mounting daemon takes
// ownership before it opens the metadata store or the cache. A restart script
// that saw no control socket yet — the first daemon was still loading its
// cache — started a second one, which reloaded the same cache directory and
// opened the same meta.db underneath the first. Ownership is the first thing
// Open does, and the refusal names ErrOwned so the front door can explain.
func TestSecondOwnerIsRefusedBeforeItOpensAnything(t *testing.T) {
	stateDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	path := filepath.Join(stateDir, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(baseConfig, cacheDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := Open(ctx, Options{Config: cfg, Version: "test", RequireOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	// Point the second daemon's cache somewhere fresh: if it gets as far as
	// creating it, ownership was not checked first.
	otherCache := filepath.Join(t.TempDir(), "second-cache")
	cfg2, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg2.Cache.Dir = otherCache
	second, err := Open(ctx, Options{Config: cfg2, Version: "test", RequireOwner: true})
	if err == nil {
		second.Close()
		t.Fatal("a second daemon opened the storage another one owns")
	}
	if !errors.Is(err, ErrOwned) {
		t.Fatalf("second Open failed with %v, want ErrOwned", err)
	}
	if _, statErr := os.Stat(otherCache); !os.IsNotExist(statErr) {
		t.Fatalf("the refused daemon created its cache directory (stat err=%v); ownership must come before every other open", statErr)
	}
}
