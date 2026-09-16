// Package prompttext assembles the text CloudFS hands an agent at run time:
// the MCP server's instructions (returned by initialize, so the agent reads
// them before its first tool call) and the four prompts an MCP client can
// list. The sentences live in internal/i18n so the control plane can show
// the same text to a person in their language; the MCP server always
// renders English. Neither mcpsrv nor control owns the wording, which is
// why it sits below both (docs/agent-first-design.md §5.1).
package prompttext

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"cloudfs/internal/i18n"
)

// Caps says which features the server in front of the agent actually has.
// A sentence about a tool that is not registered would send the agent
// after a tool it cannot call, so every optional paragraph is gated.
type Caps struct {
	// Allow lists the mount-relative prefixes the agent may read; empty
	// means the whole mount.
	Allow []string
	// ReadOnly says every mutating tool refuses.
	ReadOnly bool
	// Index says the semantic_search / read_extracted_text tools exist.
	Index bool
	// Memory says the memory_* tools exist; MemoryRoot is where they write.
	Memory     bool
	MemoryRoot string
	// Sessions says begin_session / finish_session exist.
	Sessions bool
	// Preimages says writes record a preimage and rollback_session exists.
	Preimages bool
	// Export says the export tools exist.
	Export bool
	// NonOwner says this is a stdio server beside a running mount, so every
	// write is refused until the HTTP transport is registered.
	NonOwner bool
	// MaxReadBytes is what one read_text may return, for the sentence that
	// tells the agent how to page.
	MaxReadBytes int
	// MaxTokens is the per-result token budget, 0 when off.
	MaxTokens int
}

// Instructions renders the server instructions for caps in lang. The text
// is paid on every session, so each paragraph is one or two sentences and
// the whole stays under about 600 tokens.
func Instructions(c Caps, lang i18n.Lang) string {
	var parts []string
	add := func(key string, args ...any) { parts = append(parts, i18n.T(lang, key, args...)) }
	add("agent.instructions.intro")
	if c.NonOwner {
		add("agent.instructions.non_owner")
	}
	switch {
	case len(c.Allow) > 0:
		add("agent.instructions.allow", strings.Join(c.Allow, ", "))
	default:
		add("agent.instructions.allow_all")
	}
	if c.ReadOnly {
		add("agent.instructions.read_only")
	}
	add("agent.instructions.search")
	if c.Index {
		add("agent.instructions.index")
	}
	if c.MaxReadBytes > 0 {
		add("agent.instructions.large", c.MaxReadBytes/1024)
	} else {
		add("agent.instructions.large", 256)
	}
	if c.MaxTokens > 0 {
		add("agent.instructions.tokens", c.MaxTokens)
	}
	if !c.ReadOnly && !c.NonOwner {
		add("agent.instructions.write")
		if c.Sessions {
			add("agent.instructions.session")
		}
		if c.Preimages {
			add("agent.instructions.rollback")
		}
	}
	if c.Memory {
		root := c.MemoryRoot
		if root == "" {
			root = "memory.root"
		}
		add("agent.instructions.memory", root)
	}
	if c.Export {
		add("agent.instructions.export")
	}
	add("agent.instructions.trust")
	return strings.Join(parts, "\n")
}

// Prompt is one of the prompts the server lists.
type Prompt struct {
	Name        string
	Description string
	// Args lists the argument names in order; Required says which must be
	// given.
	Args     []string
	Required []string
}

// prompts is the closed set, in the order prompts/list returns them.
var prompts = []Prompt{
	{Name: "onboard", Description: "How to start working in this mount: roots, coverage, sessions.", Args: []string{"path"}},
	{Name: "search-this-tree", Description: "Find files about something under a path without downloading the tree.", Args: []string{"path", "what"}, Required: []string{"path", "what"}},
	{Name: "write-safely", Description: "Change a file so the change is small, attributable and reversible.", Args: []string{"path"}, Required: []string{"path"}},
	{Name: "finish", Description: "Close the session with a summary the person can read.", Args: nil},
}

// Prompts lists the prompts, in order.
func Prompts() []Prompt { return append([]Prompt(nil), prompts...) }

// Names lists the prompt names, sorted.
func Names() []string {
	names := make([]string, 0, len(prompts))
	for _, p := range prompts {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

// ErrUnknownPrompt is returned by Render for a name not in Prompts.
var ErrUnknownPrompt = errors.New("prompttext: unknown prompt")

// ErrMissingArgument is returned by Render when a required argument is
// absent.
var ErrMissingArgument = errors.New("prompttext: missing argument")

// Render builds the text of prompt name for caps in lang.
func Render(name string, args map[string]string, c Caps, lang i18n.Lang) (string, error) {
	var p *Prompt
	for i := range prompts {
		if prompts[i].Name == name {
			p = &prompts[i]
		}
	}
	if p == nil {
		return "", fmt.Errorf("%w: %s", ErrUnknownPrompt, name)
	}
	for _, r := range p.Required {
		if strings.TrimSpace(args[r]) == "" {
			return "", fmt.Errorf("%w: %s", ErrMissingArgument, r)
		}
	}
	var lines []string
	add := func(key string, a ...any) { lines = append(lines, i18n.T(lang, key, a...)) }
	switch name {
	case "onboard":
		root := strings.TrimSpace(args["path"])
		if root == "" {
			root = "/"
		}
		add("agent.prompt.onboard.intro", root)
		add("agent.prompt.onboard.coverage")
		if c.Sessions {
			add("agent.prompt.onboard.session")
		}
		if c.Memory {
			add("agent.prompt.onboard.memory")
		}
	case "search-this-tree":
		add("agent.prompt.search.intro", args["what"], args["path"])
		add("agent.prompt.search.steps")
		if c.Index {
			add("agent.prompt.search.index")
		}
		add("agent.prompt.search.report")
	case "write-safely":
		add("agent.prompt.write.intro", args["path"])
		add("agent.prompt.write.steps")
		if c.Preimages {
			add("agent.prompt.write.reversible")
		}
		add("agent.prompt.write.state")
	case "finish":
		if c.Sessions {
			add("agent.prompt.finish.session")
		}
		add("agent.prompt.finish.summary")
	}
	return strings.Join(lines, "\n"), nil
}
