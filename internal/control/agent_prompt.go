package control

import (
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"

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
}

// promptHeadingMax bounds the heading a search hit passes along. A heading
// is one line of a document; anything longer is not one.
const promptHeadingMax = 512

func (s *Server) agentPrompt(w http.ResponseWriter, r *http.Request) {
	if !s.fsReady(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
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
