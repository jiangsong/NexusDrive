package config

import (
	"fmt"
	"path"
	"strings"
	"time"
)

// PoolRule overrides replicas and placement bias for one path prefix within
// a pool. The longest matching prefix wins; a path matching no rule falls
// back to the pool's own Replicas and no bias. See docs/pool-v2.md §6.1.
type PoolRule struct {
	Prefix string `yaml:"prefix"`
	// Replicas is the target replica count under this prefix. Zero means
	// "inherit the pool's own Replicas".
	Replicas int `yaml:"replicas"`
	// Prefer, Avoid and Require name member classes (PoolMember.Class).
	// Prefer biases candidate ordering; Avoid is a soft demotion, never an
	// exclusion; Require excludes members lacking the class.
	Prefer  []string `yaml:"prefer"`
	Avoid   []string `yaml:"avoid"`
	Require []string `yaml:"require"`
}

// PoolRebalance configures automatic backfill and skew correction across a
// pool's members. None of it is consumed by placement yet; it is validated
// and stored for the rebalancer to read once it exists.
type PoolRebalance struct {
	// TargetSkew is the maximum acceptable difference between the fullest
	// and emptiest in-service member's fill ratio before a rebalance plan
	// is proposed. Must be in (0, 1).
	TargetSkew float64 `yaml:"target_skew"`
	// AutoBackfill starts a rebalance plan when a freshly added member is
	// still empty while others are not. Nil means true.
	AutoBackfill *bool `yaml:"auto_backfill"`
	// MaxRate caps the byte rate a rebalance moves data at.
	MaxRate Size `yaml:"max_rate"`
	// PauseBetween is how long a rebalance waits between moves.
	PauseBetween time.Duration `yaml:"pause_between"`
}

// Values of Pool.FailureDomain: what "spread across" means when placing
// replicas of one file.
const (
	FailureDomainAccount  = "account"
	FailureDomainProvider = "provider"
	FailureDomainMember   = "member"
)

// Values of Pool.WriteMode.
const (
	WriteModeRelaxed = "relaxed"
	WriteModeStrict  = "strict"
)

// validatePoolPlacement validates and fills defaults for the v2 placement
// fields of one pool: Rules, FailureDomain, WriteMode, MinReplicasTimeout
// and Rebalance. It runs after member validation, so len(p.Members) and
// each member's Class list are already known-good.
func validatePoolPlacement(name string, p *Pool) error {
	classes := map[string]bool{}
	for _, m := range p.Members {
		for _, cl := range m.Class {
			classes[cl] = true
		}
	}
	seenPrefix := map[string]bool{}
	for i, r := range p.Rules {
		if !strings.HasPrefix(r.Prefix, "/") || path.Clean(r.Prefix) != r.Prefix || strings.ContainsAny(r.Prefix, "\x00\\") {
			return fmt.Errorf("config: pool %q rule %d has a non-canonical prefix %q (must be an absolute, cleaned path)", name, i, r.Prefix)
		}
		if seenPrefix[r.Prefix] {
			return fmt.Errorf("config: pool %q has two rules for prefix %q", name, r.Prefix)
		}
		seenPrefix[r.Prefix] = true
		if r.Replicas != 0 && (r.Replicas < 1 || r.Replicas > len(p.Members)) {
			return fmt.Errorf("config: pool %q rule %q needs 0 (inherit) or 1..%d replicas, got %d", name, r.Prefix, len(p.Members), r.Replicas)
		}
		for _, group := range [][]string{r.Prefer, r.Avoid, r.Require} {
			for _, cl := range group {
				if !classes[cl] {
					return fmt.Errorf("config: pool %q rule %q references class %q, which no member declares", name, r.Prefix, cl)
				}
			}
		}
	}
	switch p.FailureDomain {
	case "":
		p.FailureDomain = FailureDomainAccount
	case FailureDomainAccount, FailureDomainProvider, FailureDomainMember:
	default:
		return fmt.Errorf("config: pool %q failure_domain must be %s, %s or %s (got %q)", name, FailureDomainAccount, FailureDomainProvider, FailureDomainMember, p.FailureDomain)
	}
	switch p.WriteMode {
	case "":
		p.WriteMode = WriteModeRelaxed
	case WriteModeRelaxed, WriteModeStrict:
	default:
		return fmt.Errorf("config: pool %q write_mode must be %s or %s (got %q)", name, WriteModeRelaxed, WriteModeStrict, p.WriteMode)
	}
	if p.MinReplicasTimeout == 0 {
		p.MinReplicasTimeout = 2 * time.Minute
	}
	if p.MinReplicasTimeout < 0 {
		return fmt.Errorf("config: pool %q min_replicas_timeout must not be negative", name)
	}
	if p.Rebalance.TargetSkew == 0 {
		p.Rebalance.TargetSkew = 0.10
	}
	if p.Rebalance.TargetSkew <= 0 || p.Rebalance.TargetSkew >= 1 {
		return fmt.Errorf("config: pool %q rebalance.target_skew must be within (0, 1), got %v", name, p.Rebalance.TargetSkew)
	}
	if p.Rebalance.AutoBackfill == nil {
		t := true
		p.Rebalance.AutoBackfill = &t
	}
	if p.Rebalance.MaxRate == 0 {
		p.Rebalance.MaxRate = 30 << 20
	}
	if p.Rebalance.MaxRate < 0 {
		return fmt.Errorf("config: pool %q rebalance.max_rate must not be negative", name)
	}
	if p.Rebalance.PauseBetween == 0 {
		p.Rebalance.PauseBetween = 500 * time.Millisecond
	}
	if p.Rebalance.PauseBetween < 0 {
		return fmt.Errorf("config: pool %q rebalance.pause_between must not be negative", name)
	}
	return nil
}
