package daemon

import (
	"context"
	"testing"
	"time"
)

// TestMemoryStoreIsWiredWithTheBuiltinIndexRule: every daemon carries a
// memory store over its VFS; with the index enabled the memory root gets a
// built-in rule so memory_search works without anyone adding one, and the
// store searches through that index.
func TestMemoryStoreIsWiredWithTheBuiltinIndexRule(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig+"index:\n  enabled: true\n")
	if cfg.Memory.Root != "/demo/.agent" {
		t.Fatalf("memory.root must follow the first allow prefix, got %q", cfg.Memory.Root)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test", NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Memory == nil || d.Memory.Root() != "/demo/.agent" || !d.Memory.HasIndex() {
		t.Fatalf("memory store: %+v", d.Memory)
	}
	rules, err := d.Index.Rules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Path != "/demo/.agent/memory" || rules[0].Source != "builtin" {
		t.Fatalf("rules: %+v", rules)
	}
	// Without the index the store exists and says so.
	cfg2, _ := writeConfig(t, baseConfig)
	d2, err := Open(ctx, Options{Config: cfg2, Version: "test", NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if d2.Memory == nil || d2.Memory.HasIndex() {
		t.Fatal("a daemon without an index must not hand the store one")
	}
}
