// Package secrets spots content that looks like a credential before it is
// made public (docs/agent-first-design.md §8.2, borrowed from BearDrive's
// share gate). It is a handful of well-known shapes, not a scanner that
// finds everything: it shortens the odds, and the share tool says so.
// Only the first MiB of a file is looked at, and only when the file is
// already cached — the scan must not be the reason a file is downloaded.
package secrets

import (
	"regexp"
	"strings"
)

// Finding is one match: the rule and the 1-based line it is on. The
// matched text itself is never returned; a hint must not leak what the
// scan found.
type Finding struct {
	Rule string `json:"rule"`
	Line int    `json:"line"`
}

// ScanLimit is how much of a file the scan reads.
const ScanLimit = 1 << 20

type rule struct {
	name string
	re   *regexp.Regexp
}

// The rules: an AWS access key id, a PEM private key header, a GitHub
// token, a Slack token, a GitLab token, an OpenAI-style key, and the
// generic "password=…" / "secret=…" assignment with a non-trivial value.
var rules = []rule{
	{"aws-access-key", regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`)},
	{"private-key", regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----`)},
	{"github-token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[abpr]-[A-Za-z0-9-]{10,}\b`)},
	{"gitlab-token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`)},
	{"openai-key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)},
	{"password-assignment", regexp.MustCompile(`(?i)\b(?:password|passwd|secret|api[_-]?key|access[_-]?token)\s*[=:]\s*['"]?[^\s'"]{8,}`)},
}

// Scan returns every rule that matches in data, first occurrence each,
// in the order the rules are listed. Binary data (a NUL byte) is not
// scanned: the rules are for text.
func Scan(data []byte) []Finding {
	if len(data) > ScanLimit {
		data = data[:ScanLimit]
	}
	if len(data) == 0 || strings.IndexByte(string(data), 0) >= 0 {
		return nil
	}
	text := string(data)
	var out []Finding
	for _, r := range rules {
		loc := r.re.FindStringIndex(text)
		if loc == nil {
			continue
		}
		out = append(out, Finding{Rule: r.name, Line: strings.Count(text[:loc[0]], "\n") + 1})
	}
	return out
}

// Rules lists the rule names, for documentation and the settings screen.
func Rules() []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.name)
	}
	return out
}
