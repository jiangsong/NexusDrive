package control

import (
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"cloudfs/internal/agent"
	"cloudfs/internal/agent/prompttext"
	"cloudfs/internal/config"
	"cloudfs/internal/i18n"
	"cloudfs/internal/vfs"
)

// GET /agent/prompt?path=/file[&heading=...] is the "send to agent" button
// behind the scenes: a prompt the person pastes into their own MCP client
// (Claude Code, Codex) that names the file the way the MCP server names it —
// the full virtual path and the cloudfs:// resource URI — and the tools to
// use on it. It depends on nothing: no agent store, no index, no executor.
// What those add, when present, is a sentence each. The prompt is text the
// person will edit before sending, so it is short and in their language.

// AgentPromptResponse is GET /agent/prompt.
type AgentPromptResponse struct {
	Path   string `json:"path"`
	URI    string `json:"uri"`
	Prompt string `json:"prompt"`
	// Kind is "" for the per-path prompt, "instructions" for the text the
	// MCP server hands every session, or one of the prompt names.
	Kind string `json:"kind,omitempty"`
	// Tokens is the estimate of Prompt, so the console can show what a
	// session pays for the instructions.
	Tokens int `json:"tokens,omitempty"`
	// Prompts lists the prompt names the MCP server registers, with the
	// instructions kind, so the console can offer them.
	Prompts []string `json:"prompts,omitempty"`
}

// promptCaps derives what the run-time guidance may mention from the
// collector, the way mcpsrv derives it from its options: the two must
// agree, or the console shows a sentence the agent never gets.
func (s *Server) promptCaps() prompttext.Caps {
	c := prompttext.Caps{
		Index:    s.collector.Index != nil,
		Memory:   s.collector.Memory != nil,
		Sessions: s.collector.Agent != nil,
		// Preimages exist wherever sessions do in the owner process, and
		// the console only runs there.
		Preimages:    s.collector.Agent != nil,
		Export:       s.collector.Export != nil,
		MaxReadBytes: 256 << 10,
	}
	if cfg := s.collector.ConfigView(); cfg != nil {
		c.Allow = append([]string(nil), cfg.MCP.Allow...)
		c.ReadOnly = cfg.MCP.ReadOnly
		c.MemoryRoot = cfg.Memory.Root
		c.MaxTokens = cfg.MCP.Limits.MaxTokens
		if c.MaxTokens == 0 {
			c.MaxTokens = config.DefaultMCPLimits().MaxTokens
		}
		if c.MaxTokens < 0 {
			c.MaxTokens = 0
		}
	}
	return c
}

// agentGuidance is GET /agent/prompt?kind=instructions|<prompt name>: the
// same text the MCP server hands the agent, rendered here in the person's
// language so the console can show what the agent was told. Prompt
// arguments come from the query (path, what).
func (s *Server) agentGuidance(w http.ResponseWriter, r *http.Request, kind string) {
	lang := LangFrom(r)
	caps := s.promptCaps()
	resp := AgentPromptResponse{Kind: kind, Prompts: append([]string{"instructions"}, prompttext.Names()...)}
	if kind == "instructions" {
		resp.Prompt = prompttext.Instructions(caps, lang)
	} else {
		q := r.URL.Query()
		args := map[string]string{"path": q.Get("path"), "what": q.Get("what")}
		text, err := prompttext.Render(kind, args, caps, lang)
		switch {
		case errors.Is(err, prompttext.ErrUnknownPrompt):
			httpErrorT(w, r, http.StatusNotFound, "err.prompt_kind")
			return
		case errors.Is(err, prompttext.ErrMissingArgument):
			httpErrorT(w, r, http.StatusBadRequest, "err.prompt_argument")
			return
		case err != nil:
			httpErrorT(w, r, http.StatusInternalServerError, "err.prompt_kind")
			return
		}
		resp.Prompt, resp.Path = text, args["path"]
	}
	resp.Tokens = agent.EstimateTokens(resp.Prompt)
	writeJSON(w, resp)
}

// promptHeadingMax bounds the heading a search hit passes along. A heading
// is one line of a document; anything longer is not one.
const promptHeadingMax = 512

func (s *Server) agentPrompt(w http.ResponseWriter, r *http.Request) {
	if !s.fsReady(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	if kind := q.Get("kind"); kind != "" {
		s.agentGuidance(w, r, kind)
		return
	}
	p, ok := s.fsPath(w, q.Get("path"))
	if !ok {
		return
	}
	a, err := s.collector.FS.StatPath(r.Context(), p)
	if err != nil {
		// The VFS's error text names paths, which is fine on /fs, but this
		// route's reader is about to paste the answer somewhere else. A
		// missing path is just "not found".
		status := fsStatus(err)
		if errors.Is(err, vfs.ErrNotFound) || status == http.StatusInternalServerError {
			status = http.StatusNotFound
		}
		httpErrorT(w, r, status, "err.prompt_path")
		return
	}
	lang := LangFrom(r)
	uri := promptURI(s.collector.FS, p)
	lines := []string{i18n.T(lang, "agent.prompt.intro", p, uri)}
	if h := promptHeading(q.Get("heading")); h != "" {
		lines = append(lines, i18n.T(lang, "agent.prompt.heading", h))
	}
	if a.IsDir {
		lines = append(lines, i18n.T(lang, "agent.prompt.dir"))
	}
	lines = append(lines, i18n.T(lang, "agent.prompt.read"))
	if s.collector.Index != nil {
		lines = append(lines, i18n.T(lang, "agent.prompt.extracted"))
	}
	if s.collector.Agent != nil {
		lines = append(lines, i18n.T(lang, "agent.prompt.session"))
	}
	writeJSON(w, AgentPromptResponse{Path: p, URI: uri, Prompt: strings.Join(lines, "\n")})
}

// promptHeading keeps a search hit's heading to one clean line: control
// characters become spaces and the length is cut in runes, not bytes, so
// a Chinese heading is never cut mid-character.
func promptHeading(raw string) string {
	raw = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(raw))
	if rs := []rune(raw); len(rs) > promptHeadingMax {
		raw = string(rs[:promptHeadingMax])
	}
	return strings.TrimSpace(raw)
}

// promptURI is the resource URI the MCP server would answer for p:
// cloudfs://<remote>/<full virtual path>. The rule is the one in
// internal/mcpsrv/resources.go (resourceAuthority + resourceURI); the
// control plane does not import mcpsrv, so the two dozen lines live twice
// and the test asserts the shape. A path outside every mount — there is
// none once the root is mounted — gets an empty authority.
func promptURI(fs *vfs.FS, p string) string {
	remote := ""
	// Mounts are ordered deepest first, so the first match owns the path.
	for _, m := range fs.Mounts() {
		if m.Prefix == "/" || p == m.Prefix || strings.HasPrefix(p, m.Prefix+"/") {
			remote = m.Remote
			break
		}
	}
	return (&url.URL{Scheme: "cloudfs", Host: promptAuthority(remote), Path: p}).String()
}

func promptAuthority(remote string) string {
	if remote != "" {
		safe := true
		for _, r := range remote {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
				safe = false
				break
			}
		}
		if safe {
			return remote
		}
	}
	return "r~" + hex.EncodeToString([]byte(remote))
}
