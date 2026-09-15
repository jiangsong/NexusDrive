package index

import (
	"sort"
	"strings"

	"cloudfs/internal/config"
	"cloudfs/internal/pathglob"
)

// DefaultMaxFileSize bounds a rule that names no max_file_size, the same
// 20 MiB config.Index.Validate fills in for configured rules; run-time
// rules (Source "ui" or "tool") and built-in ones reach the matcher
// without that pass.
const DefaultMaxFileSize int64 = 20 << 20

// Matcher decides which files the rules cover. It is immutable once built:
// the indexer rebuilds one whenever the rule table changes.
//
// Precedence, in order: a global exclude pattern (matched against the whole
// virtual path) refuses the file whatever any rule says; then the deepest
// rule whose path is the file itself or one of its ancestors is the only
// rule consulted; an exact file rule accepts the file if it is not larger
// than its max_file_size; a directory rule additionally requires one of its
// include patterns to match and none of its exclude patterns, both applied
// to the path relative to the rule's directory.
type Matcher struct {
	rules  []Rule // deepest first
	global []string
}

// NewMatcher builds a Matcher over rules (config and run-time alike) and
// the global exclude patterns, normally config.Index.Exclude. Rules with an
// empty Include list get config.DefaultIndexInclude, a MaxFileSize of zero
// or less becomes DefaultMaxFileSize.
func NewMatcher(rules []Rule, globalExclude []string) *Matcher {
	m := &Matcher{
		rules:  make([]Rule, 0, len(rules)),
		global: append([]string(nil), globalExclude...),
	}
	for _, r := range rules {
		r.Path = cleanRulePath(r.Path)
		if len(r.Include) == 0 {
			r.Include = config.DefaultIndexInclude
		}
		if r.MaxFileSize <= 0 {
			r.MaxFileSize = DefaultMaxFileSize
		}
		m.rules = append(m.rules, r)
	}
	// Deepest path first so the first covering rule wins; ties (a repeated
	// path) keep their input order, so a later rule does not shadow an
	// earlier one unpredictably.
	sort.SliceStable(m.rules, func(i, j int) bool {
		return depth(m.rules[i].Path) > depth(m.rules[j].Path)
	})
	return m
}

// cleanRulePath makes "/", "/work" and "/work/" compare the same.
func cleanRulePath(p string) string {
	p = "/" + strings.Trim(p, "/")
	return p
}

func depth(p string) int {
	if p == "/" {
		return 0
	}
	return strings.Count(p, "/")
}

// covers reports whether rule path root is p itself or an ancestor of p.
func covers(root, p string) bool {
	if root == "/" {
		return strings.HasPrefix(p, "/")
	}
	return p == root || strings.HasPrefix(p, root+"/")
}

// relativeTo returns p relative to root without a leading "/", the form
// rule include and exclude patterns are written against.
func relativeTo(root, p string) string {
	if root != "/" {
		p = strings.TrimPrefix(p, root)
	}
	return strings.TrimPrefix(p, "/")
}

// Excluded reports whether p (a file or a directory) hits a global exclude
// pattern. The indexer asks it about directories too, so a walk can skip
// ".git" or "node_modules" instead of visiting every file below.
func (m *Matcher) Excluded(p string) bool {
	return pathglob.MatchAny(m.global, strings.TrimPrefix(p, "/"))
}

// Match reports the rule covering the file at p with the given size, and
// whether it is to be indexed. A rule whose Path equals p covers exactly
// that file regardless of Include. When no rule covers p, or the covering
// rule refuses it, ok is false and the zero Rule is returned.
func (m *Matcher) Match(p string, size int64) (Rule, bool) {
	if m.Excluded(p) {
		return Rule{}, false
	}
	for _, r := range m.rules {
		if !covers(r.Path, p) {
			continue
		}
		if size > r.MaxFileSize {
			return Rule{}, false
		}
		if r.Path == p {
			return r, true
		}
		rel := relativeTo(r.Path, p)
		if !pathglob.MatchAny(r.Include, rel) || pathglob.MatchAny(r.Exclude, rel) {
			return Rule{}, false
		}
		return r, true
	}
	return Rule{}, false
}

// Roots returns the outermost rule paths in path order: the subtrees a
// reconcile pass has to walk. A rule nested inside another rule's subtree
// is reached by walking the outer one, and Match still picks the deeper
// rule for the files below it.
func (m *Matcher) Roots() []string {
	paths := make([]string, 0, len(m.rules))
	for _, r := range m.rules {
		paths = append(paths, r.Path)
	}
	sort.Strings(paths)
	var out []string
	for _, p := range paths {
		if len(out) > 0 && covers(out[len(out)-1], p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// RulesFromConfig converts the configured rules to store rules with Source
// "config", the form Store.SyncConfigRules and NewMatcher take.
func RulesFromConfig(c []config.IndexRule) []Rule {
	out := make([]Rule, 0, len(c))
	for _, r := range c {
		out = append(out, Rule{
			Path:        r.Path,
			Include:     append([]string(nil), r.Include...),
			Exclude:     append([]string(nil), r.Exclude...),
			MaxFileSize: int64(r.MaxFileSize),
			Source:      SourceConfig,
		})
	}
	return out
}
