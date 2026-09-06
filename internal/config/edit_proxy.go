package config

import (
	"gopkg.in/yaml.v3"
)

// SetProxy replaces the whole proxy section — outbounds, groups and rules
// together. Rules name groups and groups name outbounds, so a partial edit
// can be valid in each part and still not what the person meant (deleting an
// outbound out from under a live rule). The candidate is validated as a whole
// configuration before a byte is written, so the error names the actual
// cross-reference rather than a parse failure after the fact.
func SetProxy(configPath string, p Proxy) error {
	return editConfig(configPath, false, func(root *yaml.Node, c *Config) error {
		candidate := *c
		candidate.Proxy = p
		if err := candidate.Validate(); err != nil {
			return err
		}
		if len(p.Outbounds) == 0 && len(p.Groups) == 0 && len(p.Rules) == 0 {
			removeNode(root, "proxy")
			return nil
		}
		var n yaml.Node
		if err := n.Encode(p); err != nil {
			return err
		}
		setNode(root, "proxy", &n)
		return nil
	})
}
