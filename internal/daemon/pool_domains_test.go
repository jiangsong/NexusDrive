package daemon

import (
	"testing"

	"cloudfs/internal/config"
)

// TestPoolDomainsFollowTheConfiguredFailureDomain: the pool spreads a
// file's replicas across domains, but only the daemon can say what a
// domain is — two members of one account share a quota and a ban, so
// under the default they are one domain no matter how they are named.
func TestPoolDomainsFollowTheConfiguredFailureDomain(t *testing.T) {
	cfg := &config.Config{Remotes: map[string]config.Remote{
		// ali-a and ali-b are two roots of one account: same type, same
		// account-locating settings, so one binding.
		"ali-a": {Type: "aliyun", Extra: map[string]any{"principal": "user-1"}},
		"ali-b": {Type: "aliyun", Extra: map[string]any{"principal": "user-1"}},
		"ali-c": {Type: "aliyun", Extra: map[string]any{"principal": "user-2"}},
		"gd":    {Type: "gdrive", Extra: map[string]any{"principal": "user-1"}},
	}}
	members := []config.PoolMember{{Remote: "ali-a"}, {Remote: "ali-b"}, {Remote: "ali-c"}, {Remote: "gd"}}

	byAccount, err := poolDomains(config.Pool{Members: members, FailureDomain: config.FailureDomainAccount}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if byAccount["ali-a"] != byAccount["ali-b"] {
		t.Fatalf("two members of one account are one domain, got %q and %q", byAccount["ali-a"], byAccount["ali-b"])
	}
	for _, other := range []string{"ali-c", "gd"} {
		if byAccount[other] == byAccount["ali-a"] {
			t.Fatalf("%s shares a domain with ali-a (%q)", other, byAccount[other])
		}
	}

	byProvider, err := poolDomains(config.Pool{Members: members, FailureDomain: config.FailureDomainProvider}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if byProvider["ali-a"] != "aliyun" || byProvider["ali-c"] != "aliyun" || byProvider["gd"] != "gdrive" {
		t.Fatalf("by provider = %v", byProvider)
	}

	byMember, err := poolDomains(config.Pool{Members: members, FailureDomain: config.FailureDomainMember}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if byMember[m.Remote] != m.Remote {
			t.Fatalf("by member = %v", byMember)
		}
	}

	// A member naming a remote that does not exist is a config error the
	// daemon must report, not a member quietly left out of every domain.
	if _, err := poolDomains(config.Pool{Members: []config.PoolMember{{Remote: "ghost"}}}, cfg); err == nil {
		t.Fatal("an unknown member remote was accepted")
	}
}
