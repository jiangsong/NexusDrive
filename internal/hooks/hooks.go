// Package hooks registers CloudFS in the lifecycle hooks of agent clients
// (Claude Code, Codex, Gemini CLI), so an agent working in a mounted
// directory through ordinary file tools — never calling an MCP tool —
// still gets what the MCP server would have told it: at the start of each
// turn, where it is and what changed since its last turn; after each read,
// a mark in the read heat; when it stops, its session finished
// (docs/agent-first-design.md §7, TODO.md T-54).
//
// Every client runs command hooks the same way (a shell command, the
// event JSON on stdin), so one command serves them all; what differs is
// the config file and the event names. The registration is user-level,
// written once per machine, and opens with a guard in pure shell that
// exits at once unless the working directory is inside a mount listed in
// the mounts registry — so outside CloudFS the hook costs one shell and
// no process. The hook configuration itself is never read from inside a
// mount: a shared folder is data, not orders.
package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Clients lists the supported clients, in the order they are reported.
var Clients = []string{"claude", "codex", "gemini", "hermes"}

// marker identifies our hook groups inside a config, for idempotency,
// status and removal. Removal is scoped to ourEvents as well, so a hook
// the person wrote that happens to call cloudfs elsewhere is left alone.
const marker = "cloudfs agent-hook"

// platform is one client's hook vocabulary.
type platform struct {
	// configPath is the user-level file, relative to the home directory.
	configPath []string
	// prompt fires before the model answers a turn; tool after a tool
	// ran; stop when the client's session ends. Not the per-turn "Stop"
	// (Claude Code fires it after every answer): finishing the MCP
	// session there would cut a multi-turn run after its first turn.
	prompt, tool, stop string
	// readMatcher names the tools whose runs count as reads; listing
	// tools are deliberately not among them. writeMatcher names the
	// tools that write files: what they wrote is the client's own change,
	// which the next turn must not report back to it as someone else's.
	readMatcher, writeMatcher string
	// timeout is in the unit the client counts hook timeouts in.
	timeout int
	// verified says the event names and file shape were exercised against
	// a real install; the others carry an UNVERIFIED comment and the
	// console says so.
	verified bool
	// yaml says the config file is YAML with flat hook entries (Hermes)
	// rather than the JSON groups of the other three.
	yaml bool
	// note is what the person still has to do after install.
	note string
}

var platforms = map[string]platform{
	"claude": {
		configPath:   []string{".claude", "settings.json"},
		prompt:       "UserPromptSubmit",
		tool:         "PostToolUse",
		stop:         "SessionEnd",
		readMatcher:  "Read|Grep|Bash",
		writeMatcher: "Write|Edit|MultiEdit|NotebookEdit",
		timeout:      10,
		verified:     true,
	},
	// The documented Codex events include UserPromptSubmit and SessionEnd.
	// The adapter remains UNVERIFIED until a real session exercises injection;
	// parsing hooks.json alone does not establish event execution or trust.
	"codex": {
		configPath:   []string{".codex", "hooks.json"},
		prompt:       "UserPromptSubmit",
		tool:         "PostToolUse",
		stop:         "SessionEnd",
		readMatcher:  "read_file|shell",
		writeMatcher: "apply_patch",
		timeout:      10,
		note:         "hooks must be enabled in ~/.codex/config.toml ([features] hooks = true); automatic injection remains unverified until exercised in the real client",
	},
	// UNVERIFIED: Gemini CLI event names (BeforeAgent, AfterTool,
	// SessionEnd) and millisecond timeouts; verify against a real install.
	// AfterAgent is per turn, like Claude's Stop, so it is not used.
	"gemini": {
		configPath:   []string{".gemini", "settings.json"},
		prompt:       "BeforeAgent",
		tool:         "AfterTool",
		stop:         "SessionEnd",
		readMatcher:  "read_file|read_many_files|search_file_content|run_shell_command",
		writeMatcher: "write_file|replace|edit",
		timeout:      10000,
	},
	// UNVERIFIED: Hermes hooks follow BearDrive's table as of 2026-09 —
	// ~/.hermes/config.yaml, hooks.<event> as flat lists of {matcher?,
	// command, timeout} under pre_llm_call / post_tool_call. Whether
	// Hermes has a session-end event is unknown; session_end is used
	// because an event that never fires costs nothing. The file is
	// rewritten through a YAML round trip, which drops comments.
	"hermes": {
		configPath:   []string{".hermes", "config.yaml"},
		prompt:       "pre_llm_call",
		tool:         "post_tool_call",
		stop:         "session_end",
		readMatcher:  "read_file|grep|bash",
		writeMatcher: "write_file|patch",
		timeout:      10,
		yaml:         true,
	},
}

// ourEvents is every event any platform registers or has registered
// under; uninstall only touches these, and install removes our group
// from an event the platform no longer uses (Stop and AfterAgent were
// the session-end events of an earlier shape).
var ourEvents = map[string]bool{
	"UserPromptSubmit": true, "PostToolUse": true, "SessionEnd": true,
	"BeforeAgent": true, "AfterTool": true,
	"Stop": true, "AfterAgent": true,
	"pre_llm_call": true, "post_tool_call": true, "session_end": true,
}

// ConfigPath is where a client's hooks live under home.
func ConfigPath(home, client string) (string, error) {
	p, ok := platforms[client]
	if !ok {
		return "", fmt.Errorf("unknown client %q (supported: %s)", client, strings.Join(Clients, ", "))
	}
	return filepath.Join(append([]string{home}, p.configPath...)...), nil
}

// MountsPath is the registry the guard reads: one absolute mount path
// per line, under the XDG config directory (never inside a mount).
func MountsPath(home string) string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "cloudfs", "mounts")
	}
	return filepath.Join(home, ".config", "cloudfs", "mounts")
}

// WriteMounts rewrites the registry with the given mount paths, cleaned,
// sorted and unique. The daemon writes it when a mount starts and
// install writes it from the configuration, so the guard works before
// the first mount.
func WriteMounts(path string, mounts []string) error {
	seen := map[string]bool{}
	var lines []string
	add := func(m string) {
		if m == "" || m == "." || strings.ContainsAny(m, "\n\r") || seen[m] {
			return
		}
		seen[m] = true
		lines = append(lines, m)
	}
	for _, m := range mounts {
		m = filepath.Clean(m)
		add(m)
		// Also record the path with symlinks resolved. The guard compares the
		// shell's $PWD, which a fresh shell takes from getcwd(2) and is
		// therefore fully resolved, against these lines; a mount configured
		// through a symlink would never match and the guard would exit 0 —
		// silently disabling every hook, with nothing to show for it. macOS
		// makes this the common case rather than the exotic one, since /var
		// and /tmp are symlinks into /private. Resolving here rather than in
		// the guard keeps the guard pure shell, which is what lets it cost no
		// process outside a mount.
		if resolved, err := filepath.EvalSymlinks(m); err == nil {
			add(filepath.Clean(resolved))
		}
	}
	sort.Strings(lines)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadMounts reads the registry; a missing file is an empty registry.
func ReadMounts(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

// Guard is the pure-shell prelude of every hook command: exit 0 unless
// the working directory (the client's project directory when it says
// one) is at or below a mount in the registry, and cloudfs is on PATH.
// No process is spawned on the way out; grep runs only for directories
// with a registry to check. $d walks up one component at a time so a
// session started deep inside a mount still matches its root.
func Guard() string {
	return `cd "${CLAUDE_PROJECT_DIR:-.}" 2>/dev/null || exit 0; ` +
		`m="${XDG_CONFIG_HOME:-$HOME/.config}/cloudfs/mounts"; [ -r "$m" ] || exit 0; ` +
		`d=$PWD; while :; do grep -qxF -- "$d" "$m" && break; case "$d" in ""|/) exit 0;; esac; d=${d%/*}; [ -n "$d" ] || d=/; done; ` +
		`command -v cloudfs >/dev/null || exit 0; `
}

// Command is the hook command for one event: the guard, then
// `cloudfs agent-hook <event> --client <client>` with the client's event
// JSON still on stdin. stdout is the client's contract for prompt
// (additionalContext JSON), so only stderr is discarded there.
func Command(event, client string) string {
	switch event {
	case "prompt":
		return `sh -c '` + Guard() + `exec cloudfs agent-hook prompt --client ` + client + ` 2>/dev/null'`
	default:
		return `sh -c '` + Guard() + `cloudfs agent-hook ` + event + ` --client ` + client + ` >/dev/null 2>&1 || true'`
	}
}

// Result reports what Install or Uninstall did for one client.
type Result struct {
	Client  string `json:"client"`
	Path    string `json:"path"`
	Changed bool   `json:"changed"`
	Note    string `json:"note,omitempty"`
}

// Status is one client's registration as Status reports it.
type Status struct {
	Client    string `json:"client"`
	Path      string `json:"path"`
	Installed bool   `json:"installed"`
	// Present says the client itself seems to be on this machine (its
	// config directory exists).
	Present bool `json:"present"`
	// Verified says the client's hook shape was checked on a real install
	// (only Claude Code so far); the others are best-effort UNVERIFIED.
	Verified bool `json:"verified"`
	// Note is a platform's extra step, when it has one.
	Note string `json:"note,omitempty"`
}

// groups builds the four hook groups of a client in the client's shape.
// kind is the agent-hook event a group runs, which tells two of our
// groups under one client event (read and write, both PostToolUse) apart.
func groups(client string) []struct {
	event, kind string
	group       map[string]any
} {
	p := platforms[client]
	hook := func(event string, async bool) map[string]any {
		h := map[string]any{"type": "command", "command": Command(event, client), "timeout": p.timeout}
		if client == "claude" {
			if async {
				h["async"] = true
			} else {
				// The turn-start hook is the one the person waits on;
				// Claude Code shows this line while it runs.
				h["statusMessage"] = "cloudfs: preparing context"
			}
		}
		return h
	}
	return []struct {
		event, kind string
		group       map[string]any
	}{
		{p.prompt, "prompt", map[string]any{"hooks": []any{hook("prompt", false)}}},
		{p.tool, "read", map[string]any{"matcher": p.readMatcher, "hooks": []any{hook("read", true)}}},
		{p.tool, "write", map[string]any{"matcher": p.writeMatcher, "hooks": []any{hook("write", true)}}},
		{p.stop, "stop", map[string]any{"hooks": []any{hook("stop", true)}}},
	}
}

// Install registers the hooks for clients (empty means every client
// whose config directory exists) under home. It merges into what the
// file already holds and is idempotent: a group already registered is
// converged to the current shape, nothing else is touched.
func Install(home string, clients []string) ([]Result, error) {
	if len(clients) == 0 {
		clients = Detect(home)
	}
	var out []Result
	for _, c := range clients {
		p, ok := platforms[c]
		if !ok {
			return out, fmt.Errorf("unknown client %q (supported: %s)", c, strings.Join(Clients, ", "))
		}
		path, _ := ConfigPath(home, c)
		merge := mergeInto
		if p.yaml {
			merge = mergeIntoYAML
		}
		changed, err := merge(path, c)
		if err != nil {
			return out, fmt.Errorf("%s: %w", c, err)
		}
		out = append(out, Result{Client: c, Path: path, Changed: changed, Note: p.note})
	}
	return out, nil
}

// Uninstall removes our hooks from each client's config, leaving every
// other hook where it was.
func Uninstall(home string, clients []string) ([]Result, error) {
	if len(clients) == 0 {
		clients = Clients
	}
	var out []Result
	for _, c := range clients {
		path, err := ConfigPath(home, c)
		if err != nil {
			return out, err
		}
		remove := removeFrom
		if platforms[c].yaml {
			remove = removeFromYAML
		}
		changed, err := remove(path)
		if err != nil {
			return out, fmt.Errorf("%s: %w", c, err)
		}
		out = append(out, Result{Client: c, Path: path, Changed: changed})
	}
	return out, nil
}

// Detect lists the clients whose config directory exists under home.
func Detect(home string) []string {
	var found []string
	for _, c := range Clients {
		p := platforms[c]
		if st, err := os.Stat(filepath.Join(home, p.configPath[0])); err == nil && st.IsDir() {
			found = append(found, c)
		}
	}
	return found
}

// Statuses reports every client's registration under home.
func Statuses(home string) []Status {
	var out []Status
	for _, c := range Clients {
		path, _ := ConfigPath(home, c)
		st := Status{Client: c, Path: path, Verified: platforms[c].verified, Note: platforms[c].note}
		if info, err := os.Stat(filepath.Dir(path)); err == nil && info.IsDir() {
			st.Present = true
		}
		if data, err := os.ReadFile(path); err == nil {
			st.Installed = strings.Contains(string(data), marker)
		}
		out = append(out, st)
	}
	return out
}

// mergeInto adds or converges our groups in the JSON file at path.
func mergeInto(path, client string) (bool, error) {
	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if len(strings.TrimSpace(string(data))) > 0 {
			if err := json.Unmarshal(data, &root); err != nil {
				return false, fmt.Errorf("parse %s: %w", path, err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	changed := false
	current := map[string]bool{}
	for _, g := range groups(client) {
		current[g.event] = true
		arr, _ := hooks[g.event].([]any)
		if i := indexOfOurs(arr, g.kind); i >= 0 {
			if !jsonEqual(arr[i], g.group) {
				arr[i] = g.group
				hooks[g.event] = arr
				changed = true
			}
			continue
		}
		hooks[g.event] = append(arr, g.group)
		changed = true
	}
	// A group of ours under an event this shape no longer uses is stale:
	// left there it would keep firing beside the new one.
	for event := range ourEvents {
		if current[event] {
			continue
		}
		arr, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		kept := arr[:0:0]
		for _, g := range arr {
			if containsMarker(g) {
				changed = true
				continue
			}
			kept = append(kept, g)
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if !changed {
		return false, nil
	}
	return true, writeJSON(path, root)
}

// removeFrom strips our groups from the JSON file at path, leaving the
// rest byte-for-byte semantically (the file is re-encoded).
func removeFrom(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	root := map[string]any{}
	if err := json.Unmarshal(data, &root); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return false, nil
	}
	changed := false
	for event := range ourEvents {
		arr, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		var kept []any
		for _, g := range arr {
			if containsMarker(g) {
				changed = true
				continue
			}
			kept = append(kept, g)
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if !changed {
		return false, nil
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	}
	return true, writeJSON(path, root)
}

// indexOfOurs finds our group of the given kind (prompt, read, write,
// stop) in an event's list, -1 when absent; an older shape's group of no
// recognisable kind counts as any kind, so it is converged rather than
// left beside the new one.
func indexOfOurs(arr []any, kind string) int {
	fallback := -1
	for i, g := range arr {
		if !containsMarker(g) {
			continue
		}
		b, _ := json.Marshal(g)
		if strings.Contains(string(b), marker+" "+kind+" ") {
			return i
		}
		known := false
		for _, k := range []string{"prompt", "read", "write", "stop"} {
			known = known || strings.Contains(string(b), marker+" "+k+" ")
		}
		if !known && fallback < 0 {
			fallback = i
		}
	}
	return fallback
}

func containsMarker(v any) bool {
	b, err := json.Marshal(v)
	return err == nil && strings.Contains(string(b), marker)
}

func jsonEqual(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// writeJSON writes root to path atomically, creating the directory, with
// the permissions a settings file has.
func writeJSON(path string, root map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// StartupGroup is installed with the built-in skill integration.
func StartupGroup(client string) map[string]any {
	return map[string]any{"hooks": []any{map[string]any{"type": "command", "command": Command("prompt", client), "timeout": 10}}}
}
