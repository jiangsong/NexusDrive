package config

import (
	"strings"
	"testing"
)

func TestMemoryDefaultsToTheWorkspace(t *testing.T) {
	cfg, err := Parse([]byte("mcp:\n  workspace: /work/.box\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.Root != "/work/.box" {
		t.Fatalf("memory.root must default to mcp.workspace, got %q", cfg.Memory.Root)
	}
	if cfg.Memory.MaxFactBytes != 64<<10 || cfg.Memory.MaxAgentBytes != 32<<20 {
		t.Fatalf("defaults: %+v", cfg.Memory)
	}
	// An explicit root wins over the workspace.
	cfg, err = Parse([]byte("mcp:\n  workspace: /work/.box\nmemory:\n  root: /notes/.mem\n  max_fact_bytes: 8KiB\n  max_agent_bytes: 1MiB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.Root != "/notes/.mem" || cfg.Memory.MaxFactBytes != 8<<10 || cfg.Memory.MaxAgentBytes != 1<<20 {
		t.Fatalf("%+v", cfg.Memory)
	}
	// Without a workspace the root follows the workspace's own default:
	// the first allow prefix plus /.agent.
	cfg, err = Parse([]byte("mcp:\n  allow: [/work, /gd]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.Root != "/work/.agent" {
		t.Fatalf("memory.root must follow the derived workspace, got %q", cfg.Memory.Root)
	}
	// With nothing to derive from it stays empty: the tools register and
	// refuse with a configuration message.
	cfg, err = Parse([]byte("mcp: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.Root != "" {
		t.Fatalf("no workspace and no allow must leave memory.root empty, got %q", cfg.Memory.Root)
	}
	// A limit that would let a single fact exceed the agent budget is a
	// configuration error.
	doc := "memory:\n  max_fact_bytes: 2MiB\n  max_agent_bytes: 1MiB\n"
	if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), "memory.max_fact_bytes") {
		t.Errorf("%q: expected a memory limit error, got %v", doc, err)
	}
}

func TestMemoryRootMustBeCanonical(t *testing.T) {
	for _, bad := range []string{"work/.mem", "/work/../x", "/", "/work/", "/work/./x", `/work\x`} {
		_, err := Parse([]byte("memory:\n  root: " + bad + "\n"))
		if err == nil || !strings.Contains(err.Error(), "memory.root") {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	cfg, err := Parse([]byte("memory:\n  root: /work/.mem\n"))
	if err != nil || cfg.Memory.Root != "/work/.mem" {
		t.Fatalf("%+v %v", cfg.Memory, err)
	}
}
