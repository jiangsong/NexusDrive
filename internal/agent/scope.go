package agent

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// The three refusals a scope can hand back. ErrDenied and ErrReadOnly carry
// the exact text mcpsrv returned before scopes existed, so an agent that
// matches on the message keeps working once mcpsrv delegates to Scope.
var (
	ErrDenied   = errors.New("path is outside the allowed directories")
	ErrReadOnly = errors.New("this cloudfs MCP server is read-only")
	ErrExpired  = errors.New("this cloudfs access has expired")
)

// Normalise turns whatever an agent sent into the one spelling the mount
// uses: rooted, cleaned, no trailing slash. ".." can never climb above "/",
// so a prefix comparison on the result is a real containment test.
func Normalise(p string) string {
	if p == "" {
		return "/"
	}
	return path.Clean("/" + strings.TrimPrefix(p, "/"))
}

// Under reports whether the normalised path p is prefix itself or lives
// somewhere below it. "/" covers everything. Because both sides are
// normalised, "/workshop" is not under "/work".
func Under(p, prefix string) bool {
	return prefix == "/" || p == prefix || strings.HasPrefix(p, prefix+"/")
}

// clean normalises every prefix of a list, keeping nil as nil because for
// Write nil and empty mean different things.
func clean(list []string) []string {
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, p := range list {
		out = append(out, Normalise(p))
	}
	return out
}

// EffectiveRead is the list of normalised prefixes the scope may read. An
// empty Read means the whole mount.
func (s Scope) EffectiveRead() []string {
	if len(s.Read) == 0 {
		return []string{"/"}
	}
	return clean(s.Read)
}

// EffectiveWrite is the list of normalised prefixes the scope may write,
// after ReadOnly (nothing) and Sandbox (only inside that directory) have been
// applied. It is never nil: an empty result means no writes at all.
func (s Scope) EffectiveWrite() []string {
	if s.ReadOnly {
		return []string{}
	}
	w := s.EffectiveRead()
	if s.Write != nil {
		w = clean(s.Write)
	}
	if s.Sandbox != "" {
		w = intersect(w, []string{Normalise(s.Sandbox)})
	}
	return w
}

// intersect keeps, for every pair of prefixes that nest, the deeper one.
// Pairs that share nothing contribute nothing, so the result can be empty.
func intersect(a, b []string) []string {
	out := []string{}
	for _, x := range a {
		for _, y := range b {
			switch {
			case Under(y, x):
				out = append(out, y)
			case Under(x, y):
				out = append(out, x)
			}
		}
	}
	return dedupe(out)
}

// dedupe drops exact repeats while keeping the first occurrence in place.
func dedupe(list []string) []string {
	seen := make(map[string]struct{}, len(list))
	out := list[:0]
	for _, p := range list {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// expired reports whether the scope has stopped being valid at now. A zero
// ExpiresAt never expires; the expiry instant itself is already over.
func (s Scope) expired(now time.Time) bool {
	return !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt)
}

// CheckAt decides whether p may be read (or written) under this scope at the
// given instant and returns the normalised path to use from then on. Expiry
// is checked before anything else so an expired session gets one clear
// answer whatever it asks for.
func (s Scope) CheckAt(p string, write bool, now time.Time) (string, error) {
	if s.expired(now) {
		return "", ErrExpired
	}
	c := Normalise(p)
	if write {
		if s.ReadOnly {
			return "", ErrReadOnly
		}
		for _, w := range s.EffectiveWrite() {
			if Under(c, w) {
				return c, nil
			}
		}
		return "", fmt.Errorf("%w: %s", ErrDenied, c)
	}
	for _, r := range s.EffectiveRead() {
		if Under(c, r) {
			return c, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrDenied, c)
}

// Check is CheckAt against the wall clock.
func (s Scope) Check(p string, write bool) (string, error) {
	return s.CheckAt(p, write, time.Now())
}

// CheckWriteAllowed answers the path-free half of a write check: has the
// scope expired, and is it read-only. Tools that create something whose path
// is decided later (a session workspace, say) ask this before they start.
func (s Scope) CheckWriteAllowed(now time.Time) error {
	if s.expired(now) {
		return ErrExpired
	}
	if s.ReadOnly {
		return ErrReadOnly
	}
	return nil
}

// Narrow composes this scope with a child request and returns a scope that
// allows at most what both allow: prefixes intersect, read-only sticks, and
// the earlier expiry wins. It never widens, so a session may safely narrow
// the scope of the principal that opened it.
func (s Scope) Narrow(c Scope) Scope {
	out := Scope{
		Read:      intersect(s.EffectiveRead(), c.EffectiveRead()),
		Write:     intersect(s.EffectiveWrite(), c.EffectiveWrite()),
		ReadOnly:  s.ReadOnly || c.ReadOnly,
		ExpiresAt: earliest(s.ExpiresAt, c.ExpiresAt),
	}
	if len(out.Read) == 0 {
		// Nothing readable. An empty Read means "whole mount", so an empty
		// intersection cannot be expressed by the prefixes alone; return a
		// scope that is read-only and already expired instead, so every
		// check refuses.
		return denyAll()
	}
	return out
}

// denyAll is the scope Narrow hands back when two scopes share nothing. It
// is expired at the dawn of time, so CheckAt fails for any real clock, and
// read-only with explicit empty lists so nobody misreads it as "whole
// mount".
func denyAll() Scope {
	return Scope{Read: []string{}, Write: []string{}, ReadOnly: true, ExpiresAt: time.Unix(1, 0)}
}

// earliest picks the sooner of two expiries, treating zero as "never".
func earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case b.Before(a):
		return b
	default:
		return a
	}
}

// Covers reports whether o allows nothing this scope does not: every read
// and write prefix of o is under one of s, and o does not outlive s. A scope
// that has already expired allows nothing, so it is covered by anything.
func (s Scope) Covers(o Scope) bool {
	return s.coversAt(o, time.Now())
}

func (s Scope) coversAt(o Scope, now time.Time) bool {
	if o.expired(now) {
		return true
	}
	if !s.ExpiresAt.IsZero() && (o.ExpiresAt.IsZero() || o.ExpiresAt.After(s.ExpiresAt)) {
		return false
	}
	return allUnder(o.EffectiveRead(), s.EffectiveRead()) &&
		allUnder(o.EffectiveWrite(), s.EffectiveWrite())
}

// allUnder reports whether each prefix in want sits under some prefix in
// have. An empty want is trivially covered.
func allUnder(want, have []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if Under(w, h) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
