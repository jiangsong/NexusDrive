package main

import (
	"context"
	"strings"
	"testing"
)

// TestPoolDestructiveActionsRequireConfirm: draining a member moves every
// file off it, removing one drops its copies from the pool, and rebuilding
// throws the index away — the daemon gates all three on an explicit
// confirmation, and the CLI used to supply that confirmation itself, so a
// mistyped word ran the whole thing. The flag is the user's to give, as it
// already is for `copies forget` and `uploads discard`.
func TestPoolDestructiveActionsRequireConfirm(t *testing.T) {
	_, cfgPath := uploadCLIConfig(t)
	for _, args := range [][]string{
		{"drain", "home", "ali"},
		{"remove", "home", "ali"},
		{"rebuild", "home"},
	} {
		err := cmdPool(context.Background(), append(args, "--config", cfgPath))
		if err == nil {
			t.Fatalf("%v ran without --confirm", args)
		}
		if !strings.Contains(err.Error(), "--confirm") {
			t.Fatalf("%v: error does not name the flag: %v", args, err)
		}
	}
	// The read-only and non-destructive actions are unaffected: with no
	// daemon listening they fail for that reason, not for a missing flag.
	for _, args := range [][]string{{"enable", "home", "ali"}, {"repair", "home"}} {
		err := cmdPool(context.Background(), append(args, "--config", cfgPath))
		if err != nil && strings.Contains(err.Error(), "--confirm") {
			t.Fatalf("%v should not ask for --confirm: %v", args, err)
		}
	}
}
