package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetCacheBudgetRewritesOnlyTheTwoKeys: the console changes the budget
// in place; the cache dir, the comments and every other section stay as
// they were, and the file reads back with the new figures.
func TestSetCacheBudgetRewritesOnlyTheTwoKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	body := "# keep me\ncache:\n    dir: \"/var/cache/cloudfs\"\n    max_size: 50GiB\nremotes: {}\nmounts: []\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetCacheBudget(p, 20<<30, 1<<30); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{"# keep me", `dir: "/var/cache/cloudfs"`, "max_size: 20.0GiB", "min_free: 1.0GiB"} {
		if !strings.Contains(text, want) {
			t.Fatalf("rewritten file lacks %q:\n%s", want, text)
		}
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.MaxSize != 20<<30 || cfg.Cache.MinFree != 1<<30 || cfg.Cache.Dir != "/var/cache/cloudfs" {
		t.Fatalf("reloaded cache = %+v", cfg.Cache)
	}
	if err := SetCacheBudget(p, 0, 0); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(p); err != nil || cfg.Cache.MaxSize != 0 || cfg.Cache.MinFree != 0 {
		t.Fatalf("zero budget: %+v %v", cfg.Cache, err)
	}
	if err := SetCacheBudget(p, -1, 0); err == nil {
		t.Fatal("negative budget accepted")
	}
}
