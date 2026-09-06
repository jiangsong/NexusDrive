// Package proxy routes each outgoing request to an outbound (direct, http or
// socks5) using Clash-style rules, so an API host and its download CDN can
// take different paths (docs/DESIGN.md §4.2).
package proxy

import (
	"fmt"
	"net"
	"strings"
)

// RuleKind is the matcher type.
type RuleKind string

const (
	Domain        RuleKind = "DOMAIN"
	DomainSuffix  RuleKind = "DOMAIN-SUFFIX"
	DomainKeyword RuleKind = "DOMAIN-KEYWORD"
	IPCIDR        RuleKind = "IP-CIDR"
	GeoIP         RuleKind = "GEOIP"
	Final         RuleKind = "FINAL"
)

// Rule is one parsed line such as "DOMAIN-SUFFIX,googleapis.com,proxy".
type Rule struct {
	Kind     RuleKind
	Match    string
	Outbound string
	cidr     *net.IPNet
}

// Parse parses one rule line.
func Parse(line string) (Rule, error) {
	parts := strings.Split(strings.TrimSpace(line), ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	if len(parts) == 2 && strings.EqualFold(parts[0], string(Final)) {
		return Rule{Kind: Final, Outbound: parts[1]}, nil
	}
	if len(parts) != 3 {
		return Rule{}, fmt.Errorf("proxy: malformed rule %q", line)
	}
	r := Rule{Kind: RuleKind(strings.ToUpper(parts[0])), Match: strings.ToLower(parts[1]), Outbound: parts[2]}
	switch r.Kind {
	case Domain, DomainSuffix, DomainKeyword:
		if r.Match == "" {
			return Rule{}, fmt.Errorf("proxy: empty match in %q", line)
		}
	case GeoIP:
		r.Match = strings.ToUpper(r.Match)
	case IPCIDR:
		_, n, err := net.ParseCIDR(r.Match)
		if err != nil {
			return Rule{}, fmt.Errorf("proxy: bad CIDR in %q: %w", line, err)
		}
		r.cidr = n
	case Final:
		return Rule{}, fmt.Errorf("proxy: FINAL takes one argument: %q", line)
	default:
		return Rule{}, fmt.Errorf("proxy: unknown rule kind %q", parts[0])
	}
	return r, nil
}

// Target describes a connection to be routed.
type Target struct {
	Host string // hostname or IP literal, without port
	IP   net.IP // resolved IP if known; nil otherwise
}

// Router evaluates rules in order and returns the first matching outbound.
type Router struct {
	rules []Rule
	// GeoIP returns the ISO country code for ip ("" if unknown). nil disables
	// GEOIP rules.
	GeoIP func(ip net.IP) string
	// DefaultOutbound is used when no rule (including FINAL) matches.
	DefaultOutbound string
}

// NewRouter parses lines into a Router. Rule order is preserved.
func NewRouter(lines []string) (*Router, error) {
	r := &Router{DefaultOutbound: "direct"}
	for _, l := range lines {
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		rule, err := Parse(l)
		if err != nil {
			return nil, err
		}
		r.rules = append(r.rules, rule)
	}
	return r, nil
}

// Rules returns the parsed rules (for `cloudfs proxy test`).
func (r *Router) Rules() []Rule { return r.rules }

// Outbound picks the outbound for t. IP-based rules are skipped when t.IP is
// nil and Host is not an IP literal; the caller may resolve and retry.
func (r *Router) Outbound(t Target) string {
	host := strings.ToLower(strings.TrimSuffix(t.Host, "."))
	ip := t.IP
	if ip == nil {
		ip = net.ParseIP(host)
	}
	for _, rule := range r.rules {
		switch rule.Kind {
		case Domain:
			if host == rule.Match {
				return rule.Outbound
			}
		case DomainSuffix:
			if host == rule.Match || strings.HasSuffix(host, "."+rule.Match) {
				return rule.Outbound
			}
		case DomainKeyword:
			if strings.Contains(host, rule.Match) {
				return rule.Outbound
			}
		case IPCIDR:
			if ip != nil && rule.cidr.Contains(ip) {
				return rule.Outbound
			}
		case GeoIP:
			if ip != nil && r.GeoIP != nil && r.GeoIP(ip) == rule.Match {
				return rule.Outbound
			}
		case Final:
			return rule.Outbound
		}
	}
	return r.DefaultOutbound
}

// Explain returns the matching rule for t, or nil when the default applied.
func (r *Router) Explain(t Target) *Rule {
	host := strings.ToLower(strings.TrimSuffix(t.Host, "."))
	ip := t.IP
	if ip == nil {
		ip = net.ParseIP(host)
	}
	for i := range r.rules {
		rule := &r.rules[i]
		if r.match(rule, host, ip) {
			return rule
		}
	}
	return nil
}

func (r *Router) match(rule *Rule, host string, ip net.IP) bool {
	switch rule.Kind {
	case Domain:
		return host == rule.Match
	case DomainSuffix:
		return host == rule.Match || strings.HasSuffix(host, "."+rule.Match)
	case DomainKeyword:
		return strings.Contains(host, rule.Match)
	case IPCIDR:
		return ip != nil && rule.cidr.Contains(ip)
	case GeoIP:
		return ip != nil && r.GeoIP != nil && r.GeoIP(ip) == rule.Match
	case Final:
		return true
	}
	return false
}

// DefaultProxyTarget is the placeholder the built-in rules name. It is not an
// outbound: defaultRulesFor rewrites it to whatever the configuration actually
// offers before the router ever sees it. A rule a user writes themselves is
// left exactly as written, so naming an outbound that does not exist stays an
// error rather than quietly going direct.
const DefaultProxyTarget = "proxy"

// DefaultRules is the built-in rule set: overseas drives via the configured
// proxy, China and everything else direct. Users override it in config.
//
// Every host here belongs to a registered driver: Drive, OneDrive/SharePoint,
// Dropbox, Box and S3. A driver whose endpoints are not on this list is
// reachable only if the user writes a rule or pins the account to an outbound,
// which is the bug that left S3 going direct while everything beside it was
// proxied.
var DefaultRules = []string{
	"DOMAIN-SUFFIX,googleapis.com,proxy",
	"DOMAIN-SUFFIX,googleusercontent.com,proxy",
	"DOMAIN-SUFFIX,google.com,proxy",
	"DOMAIN-SUFFIX,graph.microsoft.com,proxy",
	"DOMAIN-SUFFIX,microsoftonline.com,proxy",
	"DOMAIN-SUFFIX,sharepoint.com,proxy",
	"DOMAIN-SUFFIX,1drv.com,proxy",
	"DOMAIN-SUFFIX,live.com,proxy",
	"DOMAIN-SUFFIX,dropboxapi.com,proxy",
	"DOMAIN-SUFFIX,dropboxusercontent.com,proxy",
	"DOMAIN-SUFFIX,dropbox.com,proxy",
	"DOMAIN-SUFFIX,box.com,proxy",
	"DOMAIN-SUFFIX,boxcloud.com,proxy",
	// S3. amazonaws.com.cn is a different suffix and stays on the China rule
	// below, which is what an AWS China account wants.
	"DOMAIN-SUFFIX,amazonaws.com,proxy",
	"GEOIP,CN,direct",
	"FINAL,direct",
}
