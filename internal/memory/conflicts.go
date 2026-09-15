package memory

import (
	"regexp"
	"strings"
)

// Conflict copies. When two devices write one fact before either upload
// lands, the drive keeps the other side's file and this side's content is
// uploaded beside it under a name the provider chooses: "style (conflict
// 2026-09-15).md", "style.md.sb-1a2b", "style (1).md"... The store does not
// guess the scheme. A conflict copy of <name> is any sibling in facts/
// whose name starts with <name> and is not itself a well-formed fact file
// (<valid name>.md): "style-guide.md" is another fact, "style (1).md" is
// not.

// namePattern is the fact (and agent) name grammar: lower-case, digits and
// dashes, 1 to 64 characters, no leading dash. It leaves no room for "..",
// "/" or a space, so a name is always one safe path component.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidName reports whether s is a well-formed fact or agent name.
func ValidName(s string) bool { return namePattern.MatchString(s) }

// factName returns the fact name of a well-formed fact file name
// ("style.md" → "style") and ok=false for anything else.
func factName(file string) (string, bool) {
	name, ok := strings.CutSuffix(file, ".md")
	if !ok || !ValidName(name) {
		return "", false
	}
	return name, true
}

// conflictsOf picks, from the file names of one facts/ directory, the
// conflict copies of name, in the order given.
func conflictsOf(names []string, name string) []string {
	var out []string
	for _, n := range names {
		if !strings.HasPrefix(n, name) || n == name+".md" {
			continue
		}
		if _, isFact := factName(n); isFact {
			continue
		}
		out = append(out, n)
	}
	return out
}

// countConflicts counts the entries of a facts/ directory that are not
// well-formed fact files: every one of them is a conflict copy of some
// fact, or a stray the agent should look at.
func countConflicts(names []string) int {
	n := 0
	for _, name := range names {
		if _, isFact := factName(name); !isFact {
			n++
		}
	}
	return n
}
