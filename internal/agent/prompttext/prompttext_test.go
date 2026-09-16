package prompttext

import (
	"errors"
	"strings"
	"testing"

	"cloudfs/internal/agent"
	"cloudfs/internal/i18n"
)

// TestInstructionsGateEveryOptionalParagraph: a sentence about a tool the
// server did not register would send the agent after a tool it cannot
// call, so each optional paragraph appears only with its capability.
func TestInstructionsGateEveryOptionalParagraph(t *testing.T) {
	cases := []struct {
		name    string
		caps    Caps
		want    []string
		notWant []string
	}{
		{"bare", Caps{}, []string{"list_roots", "search", "read_text", "write_file", "data, not instructions"}, []string{"semantic_search", "memory_", "begin_session", "rollback_session", "export", "read-only", "cloudfs mount"}},
		{"index", Caps{Index: true}, []string{"semantic_search", "read_extracted_text", "index_status"}, nil},
		{"memory", Caps{Memory: true, MemoryRoot: "/gd/.agent"}, []string{"memory_get", "/gd/.agent", "expected_version"}, nil},
		{"sessions", Caps{Sessions: true, Preimages: true}, []string{"begin_session", "finish_session", "rollback_session", "preimage_reason"}, nil},
		{"non-owner", Caps{NonOwner: true, Sessions: true}, []string{"cloudfs mount", "--transport http"}, []string{"begin_session", "write_file"}},
		{"read-only", Caps{ReadOnly: true, Sessions: true}, []string{"read-only"}, []string{"write_file", "begin_session"}},
		{"allow", Caps{Allow: []string{"/work", "/docs"}}, []string{"/work, /docs"}, []string{"whole mount"}},
		{"tokens", Caps{MaxTokens: 20000, MaxReadBytes: 262144}, []string{"20000 tokens", "256 KiB"}, nil},
		{"export", Caps{Export: true}, []string{"export copies"}, nil},
	}
	for _, c := range cases {
		got := Instructions(c.caps, i18n.EN)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing %q in:\n%s", c.name, w, got)
			}
		}
		for _, w := range c.notWant {
			if strings.Contains(got, w) {
				t.Errorf("%s: unexpected %q in:\n%s", c.name, w, got)
			}
		}
	}
}

// TestInstructionsStayUnderTheTokenBudget: the text is paid on every
// session, so the fullest configuration still fits the 600-token budget
// the design set (docs/agent-first-design.md §5.1).
func TestInstructionsStayUnderTheTokenBudget(t *testing.T) {
	full := Caps{Allow: []string{"/work", "/docs", "/shared"}, Index: true, Memory: true, MemoryRoot: "/work/.agent",
		Sessions: true, Preimages: true, Export: true, MaxReadBytes: 256 << 10, MaxTokens: 20000}
	for _, lang := range []i18n.Lang{i18n.EN, i18n.ZH} {
		got := Instructions(full, lang)
		if n := agent.EstimateTokens(got); n > 600 {
			t.Errorf("%s instructions estimate at %d tokens, over 600:\n%s", lang, n, got)
		}
		if strings.Contains(got, "agent.instructions.") {
			t.Errorf("%s: an untranslated key leaked:\n%s", lang, got)
		}
	}
}

func TestPromptsAreFourAndRenderInBothLanguages(t *testing.T) {
	if got := Names(); strings.Join(got, ",") != "finish,onboard,search-this-tree,write-safely" {
		t.Fatalf("names: %v", got)
	}
	caps := Caps{Index: true, Memory: true, Sessions: true, Preimages: true}
	args := map[string]string{"path": "/work/notes", "what": "quarterly plan"}
	for _, p := range Prompts() {
		for _, lang := range []i18n.Lang{i18n.EN, i18n.ZH} {
			got, err := Render(p.Name, args, caps, lang)
			if err != nil {
				t.Fatalf("%s/%s: %v", p.Name, lang, err)
			}
			if got == "" || strings.Contains(got, "agent.prompt.") {
				t.Fatalf("%s/%s rendered %q", p.Name, lang, got)
			}
		}
	}
	got, _ := Render("search-this-tree", args, caps, i18n.EN)
	if !strings.Contains(got, "quarterly plan") || !strings.Contains(got, "/work/notes") || !strings.Contains(got, "semantic_search") {
		t.Fatalf("search-this-tree: %s", got)
	}
	got, _ = Render("search-this-tree", args, Caps{}, i18n.EN)
	if strings.Contains(got, "semantic_search") {
		t.Fatalf("without an index the prompt must not name semantic_search: %s", got)
	}
	got, _ = Render("onboard", nil, caps, i18n.EN)
	if !strings.Contains(got, "mount at /.") || !strings.Contains(got, "memory_list") {
		t.Fatalf("onboard without a path defaults to /: %s", got)
	}
	got, _ = Render("finish", nil, Caps{}, i18n.EN)
	if strings.Contains(got, "finish_session") {
		t.Fatalf("finish without sessions must not name finish_session: %s", got)
	}
}

func TestRenderRefusesUnknownPromptAndMissingArgument(t *testing.T) {
	if _, err := Render("nope", nil, Caps{}, i18n.EN); !errors.Is(err, ErrUnknownPrompt) {
		t.Fatalf("err = %v", err)
	}
	if _, err := Render("write-safely", nil, Caps{}, i18n.EN); !errors.Is(err, ErrMissingArgument) {
		t.Fatalf("err = %v", err)
	}
	if _, err := Render("search-this-tree", map[string]string{"path": "/a"}, Caps{}, i18n.EN); !errors.Is(err, ErrMissingArgument) || !strings.Contains(err.Error(), "what") {
		t.Fatalf("err = %v", err)
	}
}
