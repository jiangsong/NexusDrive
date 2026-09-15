package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Pool edits are node surgery on the `pools:` section, like every other
// editor here: comments survive, the result is validated before it is
// written, and the file is locked meanwhile. A pool's settings live under
// pools.<name>; the remote that exposes it carries only `pool: <name>`, so
// none of these edits touches the remote's account binding.

// CreatePool writes a new pool with the given members and exposes it as a
// remote of type pool named exposeAs (the pool's own name when empty).
func CreatePool(configPath, name string, members []PoolMember, replicas, minReplicas int, exposeAs string) error {
	return createPool(configPath, name, members, replicas, minReplicas, exposeAs, nil)
}

// CreatePoolAdvanced creates a pool and writes its placement settings in the
// same atomic configuration edit. The basic creator above remains the stable
// API used by the CLI and older control clients.
func CreatePoolAdvanced(configPath, name string, members []PoolMember, replicas, minReplicas int, exposeAs string, settings Pool) error {
	return createPool(configPath, name, members, replicas, minReplicas, exposeAs, &settings)
}

func createPool(configPath, name string, members []PoolMember, replicas, minReplicas int, exposeAs string, settings *Pool) error {
	if !validPoolName(name) {
		return errors.New("config: pool name must be letters, digits, '-' or '_'")
	}
	if exposeAs == "" {
		exposeAs = name
	}
	if !validPoolName(exposeAs) {
		return errors.New("config: remote name must be letters, digits, '-' or '_'")
	}
	if len(members) == 0 {
		return errors.New("config: a pool needs at least one member")
	}
	return editConfig(configPath, true, func(root *yaml.Node, c *Config) error {
		if _, exists := c.Pools[name]; exists {
			return fmt.Errorf("config: pool %q already exists", name)
		}
		if _, exists := c.Remotes[exposeAs]; exists {
			return fmt.Errorf("config: remote %q already exists", exposeAs)
		}
		for _, m := range members {
			r, ok := c.Remotes[m.Remote]
			if !ok {
				return fmt.Errorf("config: unknown remote %q", m.Remote)
			}
			if r.Type == PoolType {
				return fmt.Errorf("config: %q is a pool; pools do not nest", m.Remote)
			}
		}
		pools := mappingValue(root, "pools")
		if pools == nil || pools.Kind != yaml.MappingNode {
			pools = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setNode(root, "pools", pools)
		}
		pn := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		ms := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, m := range members {
			ms.Content = append(ms.Content, memberNode(m))
		}
		setNode(pn, "members", ms)
		if replicas > 0 {
			setNode(pn, "replicas", intNode(replicas))
		}
		if minReplicas > 0 {
			setNode(pn, "min_replicas", intNode(minReplicas))
		}
		if settings != nil {
			applyPoolSettingsNode(pn, *settings)
		}
		setNode(pools, name, pn)
		remotes := mappingValue(root, "remotes")
		if remotes == nil || remotes.Kind != yaml.MappingNode {
			remotes = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setNode(root, "remotes", remotes)
		}
		rn := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setNode(rn, "type", scalar(PoolType))
		setNode(rn, "pool", scalar(name))
		setNode(remotes, exposeAs, rn)
		return nil
	})
}

// UpdatePoolSettings replaces the editable placement policy in one validated,
// atomic write. Membership identity, roots, weights and capacities are kept;
// only class labels and policy fields are changed.
func UpdatePoolSettings(configPath, name string, settings Pool) error {
	return editConfig(configPath, false, func(root *yaml.Node, c *Config) error {
		current, ok := c.Pools[name]
		if !ok {
			return fmt.Errorf("config: unknown pool %q", name)
		}
		if len(settings.Members) != len(current.Members) {
			return errors.New("config: pool settings cannot add or remove members")
		}
		classes := make(map[string][]string, len(settings.Members))
		for _, member := range settings.Members {
			classes[member.Remote] = member.Class
		}
		for _, member := range current.Members {
			if _, ok := classes[member.Remote]; !ok {
				return fmt.Errorf("config: pool settings missing member %q", member.Remote)
			}
		}
		pn := poolNode(root, name)
		if pn == nil {
			return fmt.Errorf("config: unknown pool %q", name)
		}
		ms := mappingValue(pn, "members")
		for _, mn := range ms.Content {
			remote := mappingValue(mn, "remote")
			if remote == nil {
				continue
			}
			if list := classes[remote.Value]; len(list) > 0 {
				setNode(mn, "class", stringSeqNode(list))
			} else {
				removeNode(mn, "class")
			}
		}
		applyPoolSettingsNode(pn, settings)
		return nil
	})
}

func applyPoolSettingsNode(pn *yaml.Node, p Pool) {
	setNode(pn, "replicas", intNode(p.Replicas))
	setNode(pn, "min_replicas", intNode(p.MinReplicas))
	setNode(pn, "repair_concurrency", intNode(p.RepairConcurrency))
	setNode(pn, "failure_domain", scalar(p.FailureDomain))
	setNode(pn, "write_mode", scalar(p.WriteMode))
	setNode(pn, "min_replicas_timeout", scalar(p.MinReplicasTimeout.String()))
	setNode(pn, "out_after", scalar(p.OutAfter.String()))
	rules := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, rule := range p.Rules {
		rules.Content = append(rules.Content, ruleNode(rule))
	}
	setNode(pn, "rules", rules)
	rebalance := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setNode(rebalance, "target_skew", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: fmt.Sprint(p.Rebalance.TargetSkew)})
	if p.Rebalance.AutoBackfill != nil {
		setNode(rebalance, "auto_backfill", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(*p.Rebalance.AutoBackfill)})
	}
	setNode(rebalance, "max_rate", scalar(fmt.Sprint(int64(p.Rebalance.MaxRate))))
	setNode(rebalance, "pause_between", scalar(p.Rebalance.PauseBetween.String()))
	setNode(pn, "rebalance", rebalance)
}

// AddPoolMember appends a member to an existing pool.
func AddPoolMember(configPath, pool string, m PoolMember) error {
	return editConfig(configPath, false, func(root *yaml.Node, c *Config) error {
		r, ok := c.Remotes[m.Remote]
		if !ok {
			return fmt.Errorf("config: unknown remote %q", m.Remote)
		}
		return appendPoolMember(root, c, pool, m, r.Type)
	})
}

// appendPoolMember is the membership edit without a transaction of its own, so
// a caller that must publish it together with another change can put both in
// one. AddRemote is why that matters: creating an account and joining it to a
// pool used to be two writes, and a failure between them left an account that
// belonged to nothing.
//
// The member's type is a parameter because the remote being joined may not be
// in c at all — when AddRemote calls this, c is the document as it was parsed
// before the remote node was added.
func appendPoolMember(root *yaml.Node, c *Config, pool string, m PoolMember, memberType string) error {
	if _, ok := c.Pools[pool]; !ok {
		return fmt.Errorf("config: unknown pool %q", pool)
	}
	if memberType == PoolType {
		return fmt.Errorf("config: %q is a pool; pools do not nest", m.Remote)
	}
	for _, existing := range c.Pools[pool].Members {
		if existing.Remote == m.Remote {
			return fmt.Errorf("config: %q is already a member of pool %q", m.Remote, pool)
		}
	}
	pn := poolNode(root, pool)
	if pn == nil {
		return fmt.Errorf("config: unknown pool %q", pool)
	}
	ms := mappingValue(pn, "members")
	if ms == nil || ms.Kind != yaml.SequenceNode {
		ms = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setNode(pn, "members", ms)
	}
	ms.Content = append(ms.Content, memberNode(m))
	return nil
}

// RemovePoolMember drops a member from a pool's configuration. It does not
// move data: drain the member first.
func RemovePoolMember(configPath, pool, remote string) error {
	return editConfig(configPath, false, func(root *yaml.Node, c *Config) error {
		p, ok := c.Pools[pool]
		if !ok {
			return fmt.Errorf("config: unknown pool %q", pool)
		}
		if len(p.Members) == 1 && p.Members[0].Remote == remote {
			return fmt.Errorf("config: %q is the last member of pool %q; remove the pool instead", remote, pool)
		}
		pn := poolNode(root, pool)
		ms := mappingValue(pn, "members")
		if pn == nil || ms == nil || ms.Kind != yaml.SequenceNode {
			return fmt.Errorf("config: pool %q has no members", pool)
		}
		for i, mn := range ms.Content {
			if rv := mappingValue(mn, "remote"); rv != nil && rv.Value == remote {
				ms.Content = append(ms.Content[:i], ms.Content[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("config: %q is not a member of pool %q", remote, pool)
	})
}

// SetPoolField sets one scalar setting of a pool (replicas, min_replicas,
// out_after…), validated by the same rules as a hand-written file.
func SetPoolField(configPath, pool, key, value string) error {
	switch key {
	case "replicas", "min_replicas", "out_after", "probe_interval", "gc_grace", "op_ttl", "trim_grace",
		"hold_max_bytes", "hold_max_age", "repair_concurrency", "scrub_interval", "scrub_sample",
		"write_mode", "failure_domain", "min_replicas_timeout":
	default:
		return fmt.Errorf("config: %q is not a pool setting", key)
	}
	if strings.TrimSpace(value) == "" || containsControl(value) {
		return errors.New("config: value must be a single-line value without control characters")
	}
	switch key {
	case "write_mode":
		if value != WriteModeRelaxed && value != WriteModeStrict {
			return fmt.Errorf("config: write_mode must be %s or %s", WriteModeRelaxed, WriteModeStrict)
		}
	case "failure_domain":
		if value != FailureDomainAccount && value != FailureDomainProvider && value != FailureDomainMember {
			return fmt.Errorf("config: failure_domain must be %s, %s or %s", FailureDomainAccount, FailureDomainProvider, FailureDomainMember)
		}
	case "min_replicas_timeout":
		if _, err := time.ParseDuration(value); err != nil {
			return fmt.Errorf("config: min_replicas_timeout: %w", err)
		}
	}
	return editConfig(configPath, false, func(root *yaml.Node, c *Config) error {
		pn := poolNode(root, pool)
		if pn == nil {
			return fmt.Errorf("config: unknown pool %q", pool)
		}
		setNode(pn, key, &yaml.Node{Kind: yaml.ScalarNode, Value: value})
		return nil
	})
}

// AddPoolRule appends a placement rule to an existing pool. Full validation
// (canonical unique prefix, replicas within range, classes declared by some
// member) happens through the same Parse the write commits, like every
// other pool editor.
func AddPoolRule(configPath, pool string, r PoolRule) error {
	return editConfig(configPath, false, func(root *yaml.Node, c *Config) error {
		if _, ok := c.Pools[pool]; !ok {
			return fmt.Errorf("config: unknown pool %q", pool)
		}
		pn := poolNode(root, pool)
		if pn == nil {
			return fmt.Errorf("config: unknown pool %q", pool)
		}
		rs := mappingValue(pn, "rules")
		if rs == nil || rs.Kind != yaml.SequenceNode {
			rs = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			setNode(pn, "rules", rs)
		}
		for _, rn := range rs.Content {
			if pv := mappingValue(rn, "prefix"); pv != nil && pv.Value == r.Prefix {
				return fmt.Errorf("config: pool %q already has a rule for prefix %q", pool, r.Prefix)
			}
		}
		rs.Content = append(rs.Content, ruleNode(r))
		return nil
	})
}

// RemovePoolRule drops the rule for one prefix from a pool's configuration.
func RemovePoolRule(configPath, pool, prefix string) error {
	return editConfig(configPath, false, func(root *yaml.Node, c *Config) error {
		if _, ok := c.Pools[pool]; !ok {
			return fmt.Errorf("config: unknown pool %q", pool)
		}
		pn := poolNode(root, pool)
		rs := mappingValue(pn, "rules")
		if pn == nil || rs == nil || rs.Kind != yaml.SequenceNode {
			return fmt.Errorf("config: pool %q has no rule for prefix %q", pool, prefix)
		}
		for i, rn := range rs.Content {
			if pv := mappingValue(rn, "prefix"); pv != nil && pv.Value == prefix {
				rs.Content = append(rs.Content[:i], rs.Content[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("config: pool %q has no rule for prefix %q", pool, prefix)
	})
}

// SetPoolMemberField sets one field of one pool member. Today the only key
// is "class", a comma-separated list of member classes; an empty value
// clears it rather than writing an empty sequence.
func SetPoolMemberField(configPath, pool, remote, key, value string) error {
	if key != "class" {
		return fmt.Errorf("config: %q is not a pool member setting", key)
	}
	return editConfig(configPath, false, func(root *yaml.Node, c *Config) error {
		p, ok := c.Pools[pool]
		if !ok {
			return fmt.Errorf("config: unknown pool %q", pool)
		}
		member := false
		for _, m := range p.Members {
			if m.Remote == remote {
				member = true
				break
			}
		}
		if !member {
			return fmt.Errorf("config: %q is not a member of pool %q", remote, pool)
		}
		pn := poolNode(root, pool)
		ms := mappingValue(pn, "members")
		if pn == nil || ms == nil || ms.Kind != yaml.SequenceNode {
			return fmt.Errorf("config: pool %q has no members", pool)
		}
		for _, mn := range ms.Content {
			rv := mappingValue(mn, "remote")
			if rv == nil || rv.Value != remote {
				continue
			}
			var classes []string
			for _, cl := range strings.Split(value, ",") {
				if cl = strings.TrimSpace(cl); cl != "" {
					classes = append(classes, cl)
				}
			}
			if len(classes) == 0 {
				removeNode(mn, "class")
			} else {
				setNode(mn, "class", stringSeqNode(classes))
			}
			return nil
		}
		return fmt.Errorf("config: %q is not a member of pool %q", remote, pool)
	})
}

func poolNode(root *yaml.Node, pool string) *yaml.Node {
	pools := mappingValue(root, "pools")
	if pools == nil || pools.Kind != yaml.MappingNode {
		return nil
	}
	pn := mappingValue(pools, pool)
	if pn == nil || pn.Kind != yaml.MappingNode {
		return nil
	}
	return pn
}

func memberNode(m PoolMember) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle}
	setNode(n, "remote", scalar(m.Remote))
	if m.Root != "" {
		setNode(n, "root", scalar(m.Root))
	}
	if m.Weight > 0 && m.Weight != 1 {
		setNode(n, "weight", &yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprint(m.Weight)})
	}
	if m.Capacity > 0 {
		setNode(n, "capacity", &yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprint(int64(m.Capacity))})
	}
	if m.Adopt != nil && !*m.Adopt {
		setNode(n, "adopt", &yaml.Node{Kind: yaml.ScalarNode, Value: "false"})
	}
	if len(m.Class) > 0 {
		setNode(n, "class", stringSeqNode(m.Class))
	}
	return n
}

// ruleNode builds the YAML node for one placement rule, in the same flow
// style as memberNode.
func ruleNode(r PoolRule) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle}
	setNode(n, "prefix", scalar(r.Prefix))
	if r.Replicas != 0 {
		setNode(n, "replicas", intNode(r.Replicas))
	}
	if len(r.Prefer) > 0 {
		setNode(n, "prefer", stringSeqNode(r.Prefer))
	}
	if len(r.Avoid) > 0 {
		setNode(n, "avoid", stringSeqNode(r.Avoid))
	}
	if len(r.Require) > 0 {
		setNode(n, "require", stringSeqNode(r.Require))
	}
	return n
}

// stringSeqNode builds a flow-style YAML sequence of strings, used for
// class lists and rule prefer/avoid/require.
func stringSeqNode(vals []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, v := range vals {
		n.Content = append(n.Content, scalar(v))
	}
	return n
}

func intNode(v int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(v)}
}

func validPoolName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
