package config

import (
	"fmt"
	"path"
	"strings"
)

// Memory configures the agent memory store (docs/agent-roadmap.md §3.11):
// plain Markdown files under <root>/memory/<agent>/ that the memory_* MCP
// tools read and write, synchronised across devices by the drive like any
// other file. There is no on/off switch: the tools exist whenever the
// server runs, and refuse with a configuration message when Root is empty
// or lies outside the caller's scope.
type Memory struct {
	// Root is the mount directory the memory/ tree lives under. Empty
	// follows mcp.workspace, and failing that the first mcp.allow prefix
	// plus /.agent, the same default begin_session uses; with neither it
	// stays empty and every memory tool explains what to configure.
	Root string `yaml:"root"`
	// MaxFactBytes bounds one fact file, frontmatter included (default
	// 64 KiB): a memory is a note, not a document.
	MaxFactBytes Size `yaml:"max_fact_bytes"`
	// MaxAgentBytes bounds the facts/ subtree of one agent (default
	// 32 MiB); a put that would exceed it is refused with the current usage.
	MaxAgentBytes Size `yaml:"max_agent_bytes"`
}

// Built-in memory limits, applied where the YAML is silent.
const (
	DefaultMemoryMaxFactBytes  Size = 64 << 10
	DefaultMemoryMaxAgentBytes Size = 32 << 20
)

// validate fills in the defaults, derives Root from the MCP section when
// it is not set, and checks the values. workspace and allow are the MCP
// section's, already validated.
func (m *Memory) validate(workspace string, allow []string) error {
	if m.MaxFactBytes == 0 {
		m.MaxFactBytes = DefaultMemoryMaxFactBytes
	}
	if m.MaxAgentBytes == 0 {
		m.MaxAgentBytes = DefaultMemoryMaxAgentBytes
	}
	if m.MaxFactBytes < 0 {
		return fmt.Errorf("config: memory.max_fact_bytes must be positive, got %d", m.MaxFactBytes)
	}
	if m.MaxAgentBytes < 0 {
		return fmt.Errorf("config: memory.max_agent_bytes must be positive, got %d", m.MaxAgentBytes)
	}
	if m.MaxFactBytes > m.MaxAgentBytes {
		return fmt.Errorf("config: memory.max_fact_bytes (%d) must not exceed memory.max_agent_bytes (%d)", m.MaxFactBytes, m.MaxAgentBytes)
	}
	if m.Root == "" {
		m.Root = defaultMemoryRoot(workspace, allow)
	}
	if r := m.Root; r != "" && (!strings.HasPrefix(r, "/") || path.Clean(r) != r || r == "/" || strings.ContainsAny(r, "\x00\\")) {
		return fmt.Errorf("config: memory.root must be a canonical non-root virtual path, got %q", r)
	}
	return nil
}

// defaultMemoryRoot mirrors agent.DefaultWorkspace for the configuration's
// own scope: the workspace when set, else the first allow prefix that is
// not the mount root joined with ".agent", else nothing.
func defaultMemoryRoot(workspace string, allow []string) string {
	if workspace != "" {
		return workspace
	}
	for _, p := range allow {
		p = path.Clean("/" + strings.TrimPrefix(p, "/"))
		if p != "/" {
			return path.Join(p, ".agent")
		}
	}
	return ""
}
