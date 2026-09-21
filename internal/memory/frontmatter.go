package memory

import (
	"bytes"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Meta is the YAML frontmatter of a fact file. The field order is the
// order the block is written in.
type Meta struct {
	Name          string     `yaml:"name" json:"name"`
	Description   string     `yaml:"description" json:"description,omitempty"`
	Type          string     `yaml:"type" json:"type,omitempty"`
	Scope         string     `yaml:"scope,omitempty" json:"scope,omitempty"`
	SourcePaths   []string   `yaml:"source_paths,omitempty" json:"source_paths,omitempty"`
	SourceSession string     `yaml:"source_session,omitempty" json:"source_session,omitempty"`
	Replaces      []string   `yaml:"replaces,omitempty" json:"replaces,omitempty"`
	ExpiresAt     *time.Time `yaml:"expires_at,omitempty" json:"expires_at,omitempty"`
	UpdatedAt     time.Time  `yaml:"updated_at" json:"updated_at"`
}

// frontmatterHead bounds how much of a file is scanned for the closing
// fence: a listing parses every fact's head, so the cost per entry has to
// stay bounded whatever the body holds.
const frontmatterHead = 4 << 10

const fence = "---\n"

// frontmatterFits reports whether the closing fence is inside the bounded
// prefix that every reader, including List, is allowed to inspect.
func frontmatterFits(file []byte) bool {
	end := min(len(file), frontmatterHead)
	if end <= len(fence) || !bytes.HasPrefix(file, []byte(fence)) {
		return false
	}
	return bytes.Contains(file[len(fence):end], []byte("\n---\n"))
}

// render writes a fact file: the frontmatter block, then the body verbatim.
func render(m Meta, body string) string {
	var b strings.Builder
	b.WriteString(fence)
	b.WriteString("name: ")
	b.WriteString(yamlScalar(m.Name))
	b.WriteString("\ndescription: ")
	b.WriteString(yamlScalar(m.Description))
	b.WriteString("\ntype: ")
	b.WriteString(yamlScalar(m.Type))
	if m.Scope != "" {
		b.WriteString("\nscope: ")
		b.WriteString(yamlScalar(m.Scope))
	}
	if len(m.SourcePaths) > 0 {
		b.WriteString("\nsource_paths:")
		for _, p := range m.SourcePaths {
			b.WriteString("\n  - ")
			b.WriteString(yamlScalar(p))
		}
	}
	if m.SourceSession != "" {
		b.WriteString("\nsource_session: ")
		b.WriteString(yamlScalar(m.SourceSession))
	}
	if len(m.Replaces) > 0 {
		b.WriteString("\nreplaces:")
		for _, name := range m.Replaces {
			b.WriteString("\n  - ")
			b.WriteString(yamlScalar(name))
		}
	}
	if m.ExpiresAt != nil {
		b.WriteString("\nexpires_at: ")
		b.WriteString(m.ExpiresAt.UTC().Format(time.RFC3339))
	}
	b.WriteString("\nupdated_at: ")
	b.WriteString(m.UpdatedAt.UTC().Format(time.RFC3339))
	b.WriteString("\n")
	b.WriteString(fence)
	b.WriteString(body)
	return b.String()
}

// yamlScalar encodes one string as a single-line YAML scalar: plain when
// the encoder would keep it plain, quoted otherwise.
func yamlScalar(s string) string {
	out, err := yaml.Marshal(s)
	if err != nil {
		return `""`
	}
	return strings.TrimSuffix(string(out), "\n")
}

// parseFrontmatter splits a fact file into its frontmatter and body. ok is
// false, and the whole file is the body, when the file does not start with
// a fence, the closing fence is not within the first 4 KiB, or the block is
// not valid YAML. Unknown keys are ignored.
func parseFrontmatter(file []byte) (m Meta, body string, ok bool) {
	if !bytes.HasPrefix(file, []byte(fence)) {
		return Meta{}, string(file), false
	}
	head := file
	if len(head) > frontmatterHead {
		head = head[:frontmatterHead]
	}
	rest := head[len(fence):]
	end := -1
	if bytes.HasPrefix(rest, []byte("---\n")) || bytes.Equal(rest, []byte("---")) {
		end = 0
	} else if i := bytes.Index(rest, []byte("\n---\n")); i >= 0 {
		end = i + 1
	} else if bytes.HasSuffix(rest, []byte("\n---")) && len(head) == len(file) {
		end = len(rest) - 3
	}
	if end < 0 {
		return Meta{}, string(file), false
	}
	block := rest[:end]
	if err := yaml.Unmarshal(block, &m); err != nil {
		return Meta{}, string(file), false
	}
	after := len(fence) + end + 3
	if after < len(file) && file[after] == '\n' {
		after++
	}
	if after > len(file) {
		after = len(file)
	}
	return m, string(file[after:]), true
}
