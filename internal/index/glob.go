package index

import (
	"path"
	"strings"
)

// MatchGlob reports whether the virtual path p matches pattern. Both are
// split on "/" (a leading "/" is ignored on either side) and compared one
// segment at a time: a "**" segment matches zero or more whole segments,
// every other segment is a path.Match pattern applied to exactly one
// segment, so "*" never crosses a "/". A pattern that does not start with
// "**" is anchored at the first segment: "docs/*.txt" matches "/docs/a.txt"
// and not "/x/docs/a.txt". Matching is case-sensitive, like the virtual
// tree itself. A malformed segment matches nothing; config.Index.Validate
// rejects those before they get here.
//
// This is the whole glob dialect the index uses; it deliberately avoids a
// doublestar dependency.
func MatchGlob(pattern, p string) bool {
	return matchSegments(splitGlob(pattern), splitGlob(p))
}

// splitGlob cuts s on "/" and drops empty segments, so "/a//b/" and "a/b"
// are the same path.
func splitGlob(s string) []string {
	parts := strings.Split(s, "/")
	out := parts[:0]
	for _, seg := range parts {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

func matchSegments(pat, parts []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Collapse runs of "**" and let the run swallow 0..n segments.
			for len(pat) > 0 && pat[0] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 0 {
				return true
			}
			for i := 0; i <= len(parts); i++ {
				if matchSegments(pat, parts[i:]) {
					return true
				}
			}
			return false
		}
		if len(parts) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], parts[0]); err != nil || !ok {
			return false
		}
		pat, parts = pat[1:], parts[1:]
	}
	return len(parts) == 0
}

// matchAny reports whether p matches at least one of patterns.
func matchAny(patterns []string, p string) bool {
	for _, pat := range patterns {
		if MatchGlob(pat, p) {
			return true
		}
	}
	return false
}
