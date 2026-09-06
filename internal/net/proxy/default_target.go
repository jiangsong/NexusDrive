package proxy

import "strings"

// defaultRulesFor renders the built-in rule set against the configuration in
// hand, replacing DefaultProxyTarget with something that actually exists.
//
// The built-in rules say "send Drive, OneDrive, Dropbox, Box and S3 through
// the proxy", but nothing defines an outbound called "proxy" — only "direct"
// exists without configuration. So the rules named a target that was never
// there, and every request to those drives failed with
//
//	proxy: unknown outbound "proxy"
//
// not on some edge case but in the two most ordinary configurations there are:
// no proxy section at all, and a proxy section whose outbound happens to be
// called something else (hk, auto, ...). A user could add a Google Drive
// account, configure nothing, and find the account unusable with an error
// about proxy internals.
//
// The target is resolved in the order someone would mean it:
//
//  1. an outbound or group literally named "proxy" — an explicit answer;
//  2. the first group, which is what a group is for;
//  3. the first outbound that is not direct;
//  4. direct, and only when the configuration has nothing proxy-like in it at
//     all. That case is not a silent bypass: the user did not ask for a proxy,
//     and these rules are ours, not theirs. A rule the user wrote naming a
//     missing outbound never reaches this function and still fails loudly —
//     traffic they told us to route must never quietly leave unrouted.
func defaultRulesFor(opt ManagerOptions) []string {
	target := defaultProxyTarget(opt)
	if target == DefaultProxyTarget {
		return DefaultRules
	}
	out := make([]string, len(DefaultRules))
	for i, rule := range DefaultRules {
		out[i] = rewriteRuleTarget(rule, DefaultProxyTarget, target)
	}
	return out
}

func defaultProxyTarget(opt ManagerOptions) string {
	for _, g := range opt.Groups {
		if g.Name == DefaultProxyTarget {
			return DefaultProxyTarget
		}
	}
	for _, o := range opt.Outbounds {
		if o.Name == DefaultProxyTarget {
			return DefaultProxyTarget
		}
	}
	if len(opt.Groups) > 0 {
		return opt.Groups[0].Name
	}
	for _, o := range opt.Outbounds {
		if o.Type != "" && o.Type != "direct" {
			return o.Name
		}
	}
	return "direct"
}

// rewriteRuleTarget replaces a rule's trailing outbound name. The target is
// the last comma-separated field of every rule form the parser accepts.
func rewriteRuleTarget(rule, from, to string) string {
	i := strings.LastIndex(rule, ",")
	if i < 0 || strings.TrimSpace(rule[i+1:]) != from {
		return rule
	}
	return rule[:i+1] + to
}
