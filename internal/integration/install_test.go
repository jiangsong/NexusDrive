package integration

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"cloudfs/internal/hooks"
)

func snippet(c string) string {
	if c == "codex" {
		return "[mcp_servers.cloudfs]\nurl = \"http://127.0.0.1:8765/\"\n"
	}
	return `{"mcpServers":{"cloudfs":{"type":"http","url":"http://127.0.0.1:8765/"}}}`
}
func TestInstallUpgradeUninstallPreservesUser(t *testing.T) {
	for _, c := range []string{"codex", "claude"} {
		t.Run(c, func(t *testing.T) {
			home := t.TempDir()
			skill, config, _, _ := paths(home, c)
			hp, _ := hooks.ConfigPath(home, c)
			original := []byte("# user comment\nmodel = \"test\"\n")
			if c == "claude" {
				original = []byte(`{"theme":"dark","mcpServers":{"other":{"command":"test"}},"number":9007199254740993}`)
			}
			if err := atomic(config, original); err != nil {
				t.Fatal(err)
			}
			if err := atomic(hp, []byte(`{"other":true,"hooks":{"UserPromptSubmit":[{"hooks":[{"command":"echo user"}]}]}}`)); err != nil {
				t.Fatal(err)
			}
			for _, v := range []string{"1", "1", "2"} {
				if err := Apply(home, c, v, snippet(c), false); err != nil {
					t.Fatal(err)
				}
			}
			st := Inspect(home, c)
			if !st.SkillInstalled || !st.MCPConfigured || !st.HooksInstalled || st.Version != "2" || st.AutoInjection != "unverified" {
				t.Fatalf("%+v", st)
			}
			b, _ := os.ReadFile(skill)
			if !bytes.Equal(b, Skill) {
				t.Fatal("skill drift")
			}
			// User edits outside our groups survive uninstall.
			b, _ = os.ReadFile(hp)
			r, _ := object(b)
			r["newUserSetting"] = true
			_ = atomic(hp, encode(r))
			if err := Apply(home, c, "", "", true); err != nil {
				t.Fatal(err)
			}
			b, _ = os.ReadFile(config)
			if c == "codex" && !bytes.Equal(b, original) {
				t.Fatalf("TOML changed: %s", b)
			}
			if c == "claude" {
				r, _ := object(b)
				want, _ := object(original)
				if !bytes.Equal(encode(r), encode(want)) {
					t.Fatalf("JSON changed: %s", b)
				}
			}
			b, _ = os.ReadFile(hp)
			if !bytes.Contains(b, []byte("echo user")) || !bytes.Contains(b, []byte("newUserSetting")) || bytes.Contains(b, []byte("cloudfs agent-hook")) {
				t.Fatalf("hooks changed: %s", b)
			}
			if _, err := os.Stat(skill); !os.IsNotExist(err) {
				t.Fatal("skill retained")
			}
			if err := Apply(home, c, "", "", true); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestModifiedComponentsRefusedBeforeWrites(t *testing.T) {
	for _, c := range []string{"codex", "claude"} {
		for _, component := range []string{"skill", "mcp", "hooks"} {
			t.Run(c+component, func(t *testing.T) {
				h := t.TempDir()
				if err := Apply(h, c, "1", snippet(c), false); err != nil {
					t.Fatal(err)
				}
				skill, config, mp, _ := paths(h, c)
				hp, _ := hooks.ConfigPath(h, c)
				p := skill
				switch component {
				case "mcp":
					p = config
				case "hooks":
					p = hp
				}
				b, _ := os.ReadFile(p)
				if component == "hooks" {
					b = bytes.ReplaceAll(b, []byte("preparing context"), []byte("changed context"))
					if c == "codex" {
						b = bytes.ReplaceAll(b, []byte("timeout\": 10"), []byte("timeout\": 9"))
					}
				} else if component == "mcp" {
					b = bytes.ReplaceAll(b, []byte("8765"), []byte("9999"))
				} else {
					b = append(b, []byte("edited")...)
				}
				_ = os.WriteFile(p, b, 0600)
				before, _ := os.ReadFile(mp)
				for _, remove := range []bool{false, true} {
					if err := Apply(h, c, "2", snippet(c), remove); err == nil {
						t.Fatal("modified component accepted")
					}
				}
				after, _ := os.ReadFile(mp)
				if !bytes.Equal(before, after) {
					t.Fatal("manifest changed on refusal")
				}
				after, _ = os.ReadFile(p)
				if !bytes.Equal(b, after) {
					t.Fatal("user edits lost")
				}
			})
		}
	}
}
func TestMalformedConfigAndUnmanagedSkillPreserved(t *testing.T) {
	h := t.TempDir()
	skill, config, _, _ := paths(h, "claude")
	_ = atomic(config, []byte(`{"hooks":`))
	if err := Apply(h, "claude", "1", snippet("claude"), false); err == nil {
		t.Fatal("bad JSON accepted")
	}
	if _, err := os.Stat(skill); !os.IsNotExist(err) {
		t.Fatal("partial install")
	}
	_ = os.Remove(config)
	_ = os.MkdirAll(filepath.Dir(skill), 0700)
	_ = os.WriteFile(skill, []byte("mine"), 0600)
	if err := Apply(h, "claude", "1", snippet("claude"), false); err == nil {
		t.Fatal("unmanaged skill overwritten")
	}
}
