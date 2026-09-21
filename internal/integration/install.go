// Package integration owns the local, user-level CloudFS agent installation.
package integration

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"cloudfs/internal/hooks"
)

//go:embed SKILL.md
var Skill []byte

const begin = "# BEGIN CLOUDFS MANAGED MCP\n"
const end = "# END CLOUDFS MANAGED MCP\n"

type Manifest struct {
	ConfigPath string            `json:"config_path,omitempty"`
	Version    string            `json:"version"`
	Hashes     map[string]string `json:"hashes"`
}
type Status struct {
	Client         string `json:"client"`
	Present        bool   `json:"present"`
	ClientVersion  string `json:"client_version,omitempty"`
	SkillInstalled bool   `json:"skill_installed"`
	MCPConfigured  bool   `json:"mcp_configured"`
	MCPConnection  string `json:"mcp_connection"`
	Memory         string `json:"memory"`
	HooksInstalled bool   `json:"hooks_installed"`
	AutoInjection  string `json:"auto_injection"`
	Version        string `json:"installed_version,omitempty"`
	Problem        string `json:"problem,omitempty"`
}

func paths(home, client string) (skill, config, manifest string, err error) {
	switch client {
	case "codex":
		skill = filepath.Join(home, ".agents", "skills", "cloudfs", "SKILL.md")
		config = filepath.Join(home, ".codex", "config.toml")
	case "claude":
		skill = filepath.Join(home, ".claude", "skills", "cloudfs", "SKILL.md")
		config = filepath.Join(home, ".claude.json")
	default:
		err = fmt.Errorf("unsupported client %q; use codex or claude", client)
	}
	manifest = filepath.Join(home, ".config", "cloudfs", "agents", client+".json")
	return
}
func hash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func read(p string) ([]byte, error) {
	if info, err := os.Lstat(p); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file; preserved", p)
	}

	b, e := os.ReadFile(p)
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	return b, e
}
func object(b []byte) (map[string]any, error) {
	m := map[string]any{}
	if len(b) == 0 {
		return m, nil
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	if m == nil {
		return nil, errors.New("expected JSON object")
	}
	return m, nil
}
func encode(v any) []byte { b, _ := json.MarshalIndent(v, "", "  "); return append(b, '\n') }
func child(m map[string]any, k string) (map[string]any, error) {
	if v, ok := m[k]; ok {
		c, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an object", k)
		}
		return c, nil
	}
	c := map[string]any{}
	m[k] = c
	return c, nil
}
func atomic(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".cloudfs-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}

// Apply preflights every managed component before writing. Unrelated JSON
// values and TOML bytes survive; locally edited managed entries cause a refusal.
func Apply(home, client, version, snippet string, remove bool, configPath ...string) error {
	skill, config, mp, err := paths(home, client)
	if err != nil {
		return err
	}
	mb, err := read(mp)
	if err != nil {
		return err
	}
	if remove && len(mb) == 0 {
		return nil
	}
	old := Manifest{Hashes: map[string]string{}}
	if len(mb) > 0 {
		if err = json.Unmarshal(mb, &old); err != nil {
			return err
		}
	}
	next := Manifest{Version: version, Hashes: map[string]string{}, ConfigPath: old.ConfigPath}
	if len(configPath) > 0 {
		next.ConfigPath = configPath[0]
	}
	check := func(key string, b []byte) error {
		if len(b) == 0 {
			return nil
		}
		if old.Hashes[key] == "" || old.Hashes[key] != hash(b) {
			return fmt.Errorf("%s: unmanaged or locally modified %s; preserved", client, key)
		}
		return nil
	}
	writes := map[string][]byte{}
	sb, err := read(skill)
	if err != nil {
		return err
	}
	if err = check("skill", sb); err != nil {
		return err
	}
	if remove {
		if old.Hashes["skill"] != "" {
			writes[skill] = nil
		}
	} else {
		writes[skill] = Skill
		next.Hashes["skill"] = hash(Skill)
	}
	cb, err := read(config)
	if err != nil {
		return err
	}
	if client == "codex" {
		text := string(cb)
		a, z := strings.Index(text, begin), strings.Index(text, end)
		current := ""
		if a >= 0 && z >= a {
			z += len(end)
			current = text[a:z]
			text = text[:a] + text[z:]
		} else if a >= 0 || z >= 0 {
			return errors.New("incomplete CloudFS MCP markers; preserved")
		}
		// Conservatively refuse alternate/unmanaged CloudFS declarations too.
		if strings.Contains(strings.ToLower(text), "cloudfs") {
			return errors.New("unmanaged CloudFS TOML declaration; preserved")
		}
		if err = check("mcp", []byte(current)); err != nil {
			return err
		}
		if !remove {
			block := begin + snippet + end
			if text != "" && !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			text += block
			next.Hashes["mcp"] = hash([]byte(block))
		}
		writes[config] = []byte(text)
	} else {
		root, e := object(cb)
		if e != nil {
			return e
		}
		servers, e := child(root, "mcpServers")
		if e != nil {
			return e
		}
		if v, ok := servers["cloudfs"]; ok {
			if err = check("mcp", encode(v)); err != nil {
				return err
			}
		}
		if remove {
			delete(servers, "cloudfs")
		} else {
			src, e := object([]byte(snippet))
			if e != nil {
				return e
			}
			sm, e := child(src, "mcpServers")
			if e != nil {
				return e
			}
			v, ok := sm["cloudfs"]
			if !ok {
				return errors.New("missing CloudFS MCP configuration")
			}
			servers["cloudfs"] = v
			next.Hashes["mcp"] = hash(encode(v))
		}
		if len(servers) == 0 {
			delete(root, "mcpServers")
		}
		writes[config] = encode(root)
	}
	hp, _ := hooks.ConfigPath(home, client)
	hb, err := read(hp)
	if err != nil {
		return err
	}
	root, err := object(hb)
	if err != nil {
		return err
	}
	hm, err := child(root, "hooks")
	if err != nil {
		return err
	}
	// Generate the current adapter in an isolated staging home using the same
	// renderer as `hooks install`, then merge individual groups by ownership.
	stage, err := os.MkdirTemp("", "cloudfs-hooks-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if _, err = hooks.Install(stage, []string{client}); err != nil {
		return err
	}
	sp, _ := hooks.ConfigPath(stage, client)
	generated, err := os.ReadFile(sp)
	if err != nil {
		return err
	}
	gr, err := object(generated)
	if err != nil {
		return err
	}
	desired, _ := child(gr, "hooks")
	desired["SessionStart"] = []any{hooks.StartupGroup(client)}
	for event, v := range hm {
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("hooks.%s must be an array", event)
		}
		kept := []any{}
		for _, g := range arr {
			b := encode(g)
			if strings.Contains(string(b), "cloudfs agent-hook") {
				key := "hook:" + event + ":" + hash(b)
				if err = check(key, b); err != nil {
					return err
				}
			} else {
				kept = append(kept, g)
			}
		}
		if len(kept) == 0 {
			delete(hm, event)
		} else {
			hm[event] = kept
		}
	}
	if !remove {
		for event, v := range desired {
			arr, _ := hm[event].([]any)
			for _, g := range v.([]any) {
				b := encode(g)
				next.Hashes["hook:"+event+":"+hash(b)] = hash(b)
				arr = append(arr, g)
			}
			hm[event] = arr
		}
	}
	if len(hm) == 0 {
		delete(root, "hooks")
	}
	writes[hp] = encode(root)
	if !remove {
		writes[mp] = encode(next)
	} else if len(mb) > 0 {
		writes[mp] = nil
	}
	// Manifest is written last, and failures restore every file already changed.
	order := []string{skill, config, hp, mp}
	expected := map[string][]byte{skill: sb, config: cb, hp: hb, mp: mb}
	var done []string
	rollback := func() {
		for i := len(done) - 1; i >= 0; i-- {
			p := done[i]
			current, e := read(p)
			if e != nil || !bytes.Equal(current, writes[p]) {
				continue
			}
			if expected[p] == nil {
				_ = os.Remove(p)
			} else {
				_ = atomic(p, expected[p])
			}
		}
	}
	for _, p := range order {
		b, ok := writes[p]
		if !ok {
			continue
		}
		current, e := read(p)
		if e == nil && !bytes.Equal(current, expected[p]) {
			e = fmt.Errorf("%s changed during installation; preserved", p)
		}
		if e != nil {
			rollback()
			return e
		}
		if bytes.Equal(current, b) {
			continue
		}
		if b == nil {
			e = os.Remove(p)
			if errors.Is(e, os.ErrNotExist) {
				e = nil
			}
		} else {
			e = atomic(p, b)
		}
		if e != nil {
			rollback()
			return e
		}
		done = append(done, p)
	}
	return nil
}

func Inspect(home, client string) Status {
	st := Status{Client: client, AutoInjection: "unverified"}
	skill, config, mp, err := paths(home, client)
	if err != nil {
		st.Problem = err.Error()
		return st
	}
	b, err := os.ReadFile(mp)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			st.Problem = err.Error()
		}
		return st
	}
	var m Manifest
	if err = json.Unmarshal(b, &m); err != nil {
		st.Problem = err.Error()
		return st
	}
	st.Version = m.Version
	b, _ = read(skill)
	st.SkillInstalled = len(b) > 0 && hash(b) == m.Hashes["skill"]
	b, _ = read(config)
	if client == "codex" {
		a, z := strings.Index(string(b), begin), strings.Index(string(b), end)
		if a >= 0 && z >= a {
			st.MCPConfigured = hash(b[a:z+len(end)]) == m.Hashes["mcp"]
		}
	} else if r, e := object(b); e == nil {
		if c, ok := r["mcpServers"].(map[string]any); ok {
			st.MCPConfigured = hash(encode(c["cloudfs"])) == m.Hashes["mcp"]
		}
	}
	hp, _ := hooks.ConfigPath(home, client)
	b, _ = read(hp)
	if r, e := object(b); e == nil {
		hm, _ := r["hooks"].(map[string]any)
		found := map[string]bool{}
		for ev, v := range hm {
			arr, _ := v.([]any)
			for _, g := range arr {
				key := "hook:" + ev + ":" + hash(encode(g))
				found[key] = true
			}
		}
		st.HooksInstalled = true
		count := 0
		for k := range m.Hashes {
			if strings.HasPrefix(k, "hook:") {
				count++
				st.HooksInstalled = st.HooksInstalled && found[k]
			}
		}
		st.HooksInstalled = st.HooksInstalled && count > 0
	}
	if !st.SkillInstalled || !st.MCPConfigured || !st.HooksInstalled {
		st.Problem = "installation missing or modified; run agent install after reviewing local edits"
	}
	return st
}

// OwnerConfig returns the installed owner configuration, never a mounted file.
func OwnerConfig(home, client string) string {
	_, _, p, err := paths(home, client)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var m Manifest
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	return m.ConfigPath
}
