package trigger

import (
	"cloudfs/internal/config"
	"cloudfs/internal/pathglob"
	"cloudfs/internal/vfs"
)

// matchPaths returns the paths of c that rule fires for: the change's kind
// and origin must be in the rule's lists, and each path must match a glob
// — or, for a subtree change (a directory renamed, removed or replaced by
// the remote), be a directory some path matching a glob could live under,
// since everything below it may have changed with it. A rescan carries no
// paths and is handled by the caller. The result has no duplicates.
func matchPaths(rule config.Trigger, c vfs.Change) []string {
	if c.Rescan || len(c.Paths) == 0 {
		return nil
	}
	if !contains(rule.Events, c.Kind.String()) || !contains(rule.Origins, c.Origin.String()) {
		return nil
	}
	var out []string
	for _, p := range c.Paths {
		if contains(out, p) {
			continue
		}
		if pathglob.MatchAny(rule.Paths, p) || c.Subtree && mayMatchBelowAny(rule.Paths, p) {
			out = append(out, p)
		}
	}
	return out
}

func mayMatchBelowAny(patterns []string, dir string) bool {
	for _, pat := range patterns {
		if pathglob.MayMatchBelow(pat, dir) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
