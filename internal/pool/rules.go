package pool

import (
	"strings"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
)

// ruleFor returns the rule that covers pth: the one with the longest
// matching prefix, or nil when the pool's own settings are the rule. See
// docs/pool-v2.md §6.1.
func (p *Pool) ruleFor(pth string) *config.PoolRule {
	var best *config.PoolRule
	for i := range p.settings.Rules {
		r := &p.settings.Rules[i]
		if !prefixCovers(r.Prefix, pth) {
			continue
		}
		if best == nil || len(r.Prefix) > len(best.Prefix) {
			best = r
		}
	}
	return best
}

// prefixCovers reports whether a rule prefix covers a path. It matches on
// whole path components, so /photos covers /photos and /photos/a but not
// /photoshop.
func prefixCovers(prefix, pth string) bool {
	if prefix == "/" {
		return true
	}
	if !strings.HasPrefix(pth, prefix) {
		return false
	}
	return len(pth) == len(prefix) || pth[len(prefix)] == '/'
}

// wantReplicas is how many replicas pth is configured to have, before the
// members in service cap it. Use it where the question is "what did the
// operator ask for" (should this write take a hold, is this surplus);
// targetFor answers "how many can we hold right now".
func (p *Pool) wantReplicas(pth string) int {
	want := p.settings.Replicas
	if r := p.ruleFor(pth); r != nil && r.Replicas != 0 {
		want = r.Replicas
	}
	if want < 1 {
		want = 1
	}
	return want
}

// anyMultiReplica reports whether any rule — the pool's own settings
// included — asks for more than one replica. It gates the work that only
// a replicated pool needs (the repair scan, the repair queue entry a write
// leaves behind) without looking at a path.
func (p *Pool) anyMultiReplica() bool {
	if p.settings.Replicas > 1 {
		return true
	}
	for _, r := range p.settings.Rules {
		if r.Replicas > 1 {
			return true
		}
	}
	return false
}

// eligibleMembers counts the members that can take a replica right now.
func (p *Pool) eligibleMembers() int {
	n := 0
	for _, m := range p.members {
		switch m.health.Snapshot().State {
		case provider.HealthOut, provider.HealthDisabled, provider.HealthDraining:
		default:
			n++
		}
	}
	return n
}

// targetFor is how many replicas pth can have right now: what its rule
// asks for, capped by the members that can take one. capped says the cap
// bit, so callers can report "3 wanted, 2 possible" rather than treating a
// member outage as a satisfied target.
func (p *Pool) targetFor(pth string) (target int, capped bool) {
	want := p.wantReplicas(pth)
	if eligible := p.eligibleMembers(); eligible < want {
		return eligible, true
	}
	return want, false
}

// replicaTarget is targetFor under the pool's own settings, for the
// callers that speak about the pool rather than about one path.
func (p *Pool) replicaTarget() (target int, capped bool) {
	want := p.settings.Replicas
	if want < 1 {
		want = 1
	}
	if eligible := p.eligibleMembers(); eligible < want {
		return eligible, true
	}
	return want, false
}
