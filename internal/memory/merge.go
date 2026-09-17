package memory

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"cloudfs/internal/vfs"
)

// memory_merge (docs/agent-first-design.md §8.3, T-56): when a drive
// leaves a conflict copy beside a fact — two devices wrote it, the drive
// kept both — the agent gets a merge proposal rather than a coin toss. A
// three-way merge of lines when a common ancestor is known (the caller
// passes it, typically the content the session's own memory_put replaced,
// kept as its preimage), a two-way one when not: lines only one side
// changed are taken, lines both changed differently become a conflict
// block the agent resolves. Nothing is written; the proposal is what a
// following memory_put with the fact's versions may store.

// MergeResult is the proposal.
type MergeResult struct {
	Agent string `json:"agent"`
	Name  string `json:"name"`
	// Base and Theirs name what was merged: the fact and the conflict copy.
	Base   string `json:"base"`
	Theirs string `json:"theirs"`
	// Merged is the proposal, with conflict markers where the two sides
	// disagree and no ancestor settles it.
	Merged string `json:"merged"`
	// Conflicts counts the marked blocks; Clean says there are none.
	Conflicts int  `json:"conflicts"`
	Clean     bool `json:"clean"`
	// ThreeWay says an ancestor was used.
	ThreeWay bool `json:"three_way"`
	// Version and RemoteVersion are the fact's, for the put that adopts
	// the proposal.
	Version       string `json:"version"`
	RemoteVersion string `json:"remote_version,omitempty"`
}

// ErrNoConflict says the fact has no conflict copy to merge.
var ErrNoConflict = errors.New("no conflict copy to merge")

// Merge proposes a merge of a fact with one of its conflict copies (the
// first when conflict is ""), against ancestor when given.
func (s *Store) Merge(ctx context.Context, agent, name, conflict, ancestor string) (MergeResult, error) {
	f, err := s.Get(ctx, agent, name)
	if err != nil {
		return MergeResult{}, err
	}
	if len(f.Conflicts) == 0 {
		return MergeResult{}, fmt.Errorf("%w: %s/%s", ErrNoConflict, agent, name)
	}
	theirs := f.Conflicts[0]
	if conflict != "" {
		theirs = ""
		for _, c := range f.Conflicts {
			if c == conflict || path.Base(c) == conflict {
				theirs = c
			}
		}
		if theirs == "" {
			return MergeResult{}, fmt.Errorf("%w: %s is not a conflict copy of %s/%s", ErrNotFound, conflict, agent, name)
		}
	}
	data, err := s.fs.ReadFileRange(ctx, theirs, 0, 0)
	if err != nil {
		if errors.Is(err, vfs.ErrNotFound) {
			return MergeResult{}, fmt.Errorf("%w: %s", ErrNotFound, theirs)
		}
		return MergeResult{}, err
	}
	_, theirBody, ok := parseFrontmatter(data)
	if !ok {
		theirBody = string(data)
	}
	merged, conflicts := merge3(ancestor, f.Content, theirBody, ancestor != "")
	return MergeResult{
		Agent: agent, Name: name, Base: f.Path, Theirs: theirs,
		Merged: merged, Conflicts: conflicts, Clean: conflicts == 0, ThreeWay: ancestor != "",
		Version: f.Version, RemoteVersion: f.RemoteVersion,
	}, nil
}

// merge3 merges ours and theirs against base line by line. Without a
// base (threeWay false) it is a two-way merge: the common lines are kept
// and every run that differs is a conflict block.
func merge3(base, ours, theirs string, threeWay bool) (string, int) {
	o, t := splitLines(ours), splitLines(theirs)
	if !threeWay {
		hs := diffHunks(o, t)
		return joinHunks(hs), conflictHunks(hs)
	}
	b := splitLines(base)
	// Align both sides to the base: for each base line, what ours and
	// theirs did to it (kept, changed, deleted) and what they inserted.
	oe := editsAgainst(b, o)
	te := editsAgainst(b, t)
	var out []string
	conflicts := 0
	i := 0
	for i <= len(b) {
		oi, ti := oe[i], te[i]
		switch {
		case linesEqual(oi, ti):
			out = append(out, oi...)
		case linesEqual(oi, baseSlot(b, i)):
			out = append(out, ti...)
		case linesEqual(ti, baseSlot(b, i)):
			out = append(out, oi...)
		default:
			out = append(out, conflictBlock(oi, ti)...)
			conflicts++
		}
		i++
	}
	return strings.Join(out, "\n"), conflicts
}

// editsAgainst maps every base position (0..len(base), the last being
// "after the end") to the lines the side has there: the base line itself
// when kept, its replacement when changed, nothing when deleted, plus the
// side's insertions before the next base line. Computed from an LCS
// alignment of base and side.
func editsAgainst(base, side []string) [][]string {
	out := make([][]string, len(base)+1)
	bi, si := 0, 0
	for _, h := range lcsAlign(base, side) {
		// h.bi, h.si: next matched pair; everything before is an edit.
		var ins []string
		for si < h.si {
			ins = append(ins, side[si])
			si++
		}
		if bi < h.bi {
			// base lines bi..h.bi-1 were deleted or replaced by ins.
			out[bi] = append(out[bi], ins...)
			bi++
			for bi < h.bi {
				bi++
			}
		} else {
			out[bi] = append(out[bi], ins...)
		}
		if h.bi < len(base) {
			out[bi] = append(out[bi], base[h.bi])
			bi = h.bi + 1
			si = h.si + 1
		}
	}
	return out
}

type pair struct{ bi, si int }

// lcsAlign lists the matched (base index, side index) pairs of a longest
// common subsequence, ending with a sentinel pair at (len(base),
// len(side)).
func lcsAlign(a, b []string) []pair {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var out []pair
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, pair{i, j})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			i++
		default:
			j++
		}
	}
	return append(out, pair{n, m})
}

func baseSlot(base []string, i int) []string {
	if i < len(base) {
		return []string{base[i]}
	}
	return nil
}

func linesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// hunk is one run of a two-way diff: equal lines, or an ours/theirs pair.
type hunk struct {
	equal        bool
	ours, theirs []string
}

func diffHunks(o, t []string) []hunk {
	var out []hunk
	oi, ti := 0, 0
	for _, p := range lcsAlign(o, t) {
		if oi < p.bi || ti < p.si {
			out = append(out, hunk{ours: o[oi:p.bi], theirs: t[ti:p.si]})
		}
		if p.bi < len(o) {
			out = append(out, hunk{equal: true, ours: []string{o[p.bi]}})
		}
		oi, ti = p.bi+1, p.si+1
	}
	return out
}

func joinHunks(hs []hunk) string {
	var out []string
	for _, h := range hs {
		if h.equal {
			out = append(out, h.ours...)
		} else {
			out = append(out, conflictBlock(h.ours, h.theirs)...)
		}
	}
	return strings.Join(out, "\n")
}

func conflictHunks(hs []hunk) int {
	n := 0
	for _, h := range hs {
		if !h.equal {
			n++
		}
	}
	return n
}

func conflictBlock(ours, theirs []string) []string {
	out := []string{"<<<<<<< this device"}
	out = append(out, ours...)
	out = append(out, "=======")
	out = append(out, theirs...)
	return append(out, ">>>>>>> conflict copy")
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
