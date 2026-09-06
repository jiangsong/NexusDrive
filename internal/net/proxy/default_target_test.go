package proxy

import (
	"strings"
	"testing"
)

// foreignHosts are endpoints of registered drivers that the built-in rules are
// supposed to route through the proxy.
var foreignHosts = []string{
	"www.googleapis.com",
	"api.dropboxapi.com",
	"graph.microsoft.com",
	"api.box.com",
	"s3.us-east-1.amazonaws.com",
}

func outboundFor(t *testing.T, m *Manager, host string) string {
	t.Helper()
	o, err := m.OutboundFor(host)
	if err != nil {
		t.Fatalf("%s: %v", host, err)
	}
	return o.Name
}

// TestOverseasDrivesWorkWithNoProxyConfigured is the bug this file exists for.
// The built-in rules name an outbound called "proxy", and nothing defines one:
// without configuration the only outbound is "direct". So a user who added a
// Google Drive account and configured no proxy could not use it at all — every
// request failed with `proxy: unknown outbound "proxy"`, an error about our
// internals for a decision they never made.
func TestOverseasDrivesWorkWithNoProxyConfigured(t *testing.T) {
	m, err := NewManager(ManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range foreignHosts {
		if got := outboundFor(t, m, host); got != "direct" {
			t.Fatalf("with no proxy configured %s routes to %q, want direct", host, got)
		}
	}
	// The raw-TCP path (SFTP, SMB) resolves the same way, and resolving is
	// where the failure was. Dialling for real is not this test's business.
	if _, err := m.Resolve(m.Router().Outbound(Target{Host: "www.googleapis.com"})); err != nil {
		t.Fatalf("resolving an overseas drive with no proxy configured: %v", err)
	}
}

// TestBuiltInRulesFindTheProxyWhateverItIsCalled: the second half of the same
// bug. A user who configures a proxy properly but calls it anything other than
// "proxy" — hk, auto, the names people actually use — got the identical
// failure, because the built-in rules named a literal.
func TestBuiltInRulesFindTheProxyWhateverItIsCalled(t *testing.T) {
	for _, tc := range []struct {
		name      string
		outbounds []Outbound
		groups    []Group
		want      string
	}{
		{
			name:      "one outbound under any name",
			outbounds: []Outbound{{Name: "hk", Type: "socks5", Addr: "127.0.0.1:7890"}},
			want:      "hk",
		},
		{
			name:      "a group wins over a bare outbound",
			outbounds: []Outbound{{Name: "hk", Type: "socks5", Addr: "127.0.0.1:7890"}},
			groups:    []Group{{Name: "auto", Type: URLTest, Members: []string{"hk"}}},
			want:      "hk", // resolved through the group
		},
		{
			name: "an outbound actually named proxy is used as written",
			outbounds: []Outbound{
				{Name: "hk", Type: "socks5", Addr: "127.0.0.1:7890"},
				{Name: "proxy", Type: "socks5", Addr: "127.0.0.1:1080"},
			},
			want: "proxy",
		},
		{
			name:      "a direct-typed outbound is not mistaken for a proxy",
			outbounds: []Outbound{{Name: "lan", Type: "direct"}},
			want:      "direct",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewManager(ManagerOptions{Outbounds: tc.outbounds, Groups: tc.groups})
			if err != nil {
				t.Fatal(err)
			}
			for _, host := range foreignHosts {
				if got := outboundFor(t, m, host); got != tc.want {
					t.Fatalf("%s routes to %q, want %q", host, got, tc.want)
				}
			}
			// China and everything else are unaffected.
			if got := outboundFor(t, m, "www.aliyundrive.com"); got != "direct" {
				t.Fatalf("a domestic drive routes to %q, want direct", got)
			}
		})
	}
}

// TestAWrittenRuleNamingAMissingOutboundStillFails guards the reason the
// fallback is narrow. Rewriting only applies to the rules we supply. A rule the
// user wrote is their instruction: if it names an outbound that does not exist,
// the request must fail rather than quietly leave through the wrong door —
// silently going direct is exactly the leak the rule set exists to prevent.
func TestAWrittenRuleNamingAMissingOutboundStillFails(t *testing.T) {
	// A user's rule naming an outbound that does not exist is now refused when
	// the manager is built, rather than accepted and left to fail on the first
	// request that matches it — the same loud-at-startup treatment Reload gives.
	_, err := NewManager(ManagerOptions{Rules: []string{
		"DOMAIN-SUFFIX,googleapis.com,proxy",
		"FINAL,direct",
	}})
	if err == nil {
		t.Fatal("a rule naming an undefined outbound was accepted at build time")
	}
	if !strings.Contains(err.Error(), "unknown outbound") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestEveryOverseasDriverHasABuiltInRule: a driver missing from the list is
// reachable only if the user writes a rule for it by hand, and nothing tells
// them that. S3 was missing, so it went direct while Drive, OneDrive, Dropbox
// and Box beside it were proxied.
func TestEveryOverseasDriverHasABuiltInRule(t *testing.T) {
	m, err := NewManager(ManagerOptions{
		Outbounds: []Outbound{{Name: "hk", Type: "socks5", Addr: "127.0.0.1:7890"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for driver, hosts := range map[string][]string{
		"gdrive":   {"www.googleapis.com", "oauth2.googleapis.com", "lh3.googleusercontent.com"},
		"onedrive": {"graph.microsoft.com", "login.microsoftonline.com", "my.sharepoint.com"},
		"dropbox":  {"api.dropboxapi.com", "content.dropboxapi.com", "uc.dropboxusercontent.com"},
		"box":      {"api.box.com", "upload.box.com", "dl.boxcloud.com"},
		"s3":       {"s3.amazonaws.com", "s3.eu-west-1.amazonaws.com", "bucket.s3.us-east-2.amazonaws.com"},
	} {
		for _, host := range hosts {
			if got := outboundFor(t, m, host); got != "hk" {
				t.Fatalf("%s endpoint %s routes to %q, want the configured proxy", driver, host, got)
			}
		}
	}
	// AWS China is a different suffix and belongs on the China rule.
	if got := outboundFor(t, m, "s3.cn-north-1.amazonaws.com.cn"); got != "direct" {
		t.Fatalf("AWS China routes to %q, want direct", got)
	}
}
