package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/config"
)

// TestMountPrintsConfigWarnings: a rule config.Validate accepts with a
// warning is reported one line per warning, and a clean config prints
// nothing, so the daemon's stderr stays quiet in the common case.
func TestMountPrintsConfigWarnings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("mounts:\n  - path: /tmp/m\ntriggers:\n  - name: inbox\n    paths: [\"/work/**\"]\n    action:\n      exec: { command: [\"/bin/true\", \"{path}\"] }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	printConfigWarnings(&out, cfg)
	if got := out.String(); !strings.HasPrefix(got, "warning: trigger inbox:") || strings.Count(got, "\n") != 1 {
		t.Fatalf("stderr = %q", got)
	}
	cfg.Warnings = nil
	out.Reset()
	printConfigWarnings(&out, cfg)
	if out.Len() != 0 {
		t.Fatalf("a clean config printed %q", out.String())
	}
}
