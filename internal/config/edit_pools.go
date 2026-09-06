package config

import (
	"errors"
	"fmt"
	"strings"

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

// AddPoolMember appends a member to an existing pool.
func AddPoolMember(configPath, pool string, m PoolMember) error {
	return editConfig(configPath, false, func(root *yaml.Node, c *Config) error {
		if _, ok := c.Pools[pool]; !ok {
			return fmt.Errorf("config: unknown pool %q", pool)
		}
		r, ok := c.Remotes[m.Remote]
		if !ok {
			return fmt.Errorf("config: unknown remote %q", m.Remote)
		}
		if r.Type == PoolType {
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
	})
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
		"hold_max_bytes", "hold_max_age", "repair_concurrency", "scrub_interval", "scrub_sample":
	default:
		return fmt.Errorf("config: %q is not a pool setting", key)
	}
	if strings.TrimSpace(value) == "" || containsControl(value) {
		return errors.New("config: value must be a single-line value without control characters")
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
