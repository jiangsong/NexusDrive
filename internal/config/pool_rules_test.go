package config

import (
	"strings"
	"testing"
	"time"
)

// basePoolYAML is a minimal two-member pool a test can append placement
// keys to.
const basePoolYAML = `
remotes:
  a: {type: fake}
  b: {type: fake}
  home: {type: pool, pool: home}
pools:
  home:
    members:
      - {remote: a, class: [fast, cheap]}
      - {remote: b, class: [local-nas]}
`

func parsePool(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	return Parse([]byte(basePoolYAML + extra))
}

func TestPoolPlacementDefaults(t *testing.T) {
	c, err := parsePool(t, "")
	if err != nil {
		t.Fatal(err)
	}
	p := c.Pools["home"]
	if p.FailureDomain != FailureDomainAccount {
		t.Fatalf("failure_domain default = %q", p.FailureDomain)
	}
	if p.WriteMode != WriteModeRelaxed {
		t.Fatalf("write_mode default = %q", p.WriteMode)
	}
	if p.MinReplicasTimeout != 2*time.Minute {
		t.Fatalf("min_replicas_timeout default = %s", p.MinReplicasTimeout)
	}
	if p.Rebalance.TargetSkew != 0.10 {
		t.Fatalf("rebalance.target_skew default = %v", p.Rebalance.TargetSkew)
	}
	if p.Rebalance.AutoBackfill == nil || !*p.Rebalance.AutoBackfill {
		t.Fatalf("rebalance.auto_backfill default = %v", p.Rebalance.AutoBackfill)
	}
	if p.Rebalance.MaxRate != 30<<20 {
		t.Fatalf("rebalance.max_rate default = %s", p.Rebalance.MaxRate)
	}
	if p.Rebalance.PauseBetween != 500*time.Millisecond {
		t.Fatalf("rebalance.pause_between default = %s", p.Rebalance.PauseBetween)
	}
	if len(p.Members[0].Class) != 2 || p.Members[0].Class[0] != "fast" {
		t.Fatalf("member class = %v", p.Members[0].Class)
	}
}

func TestPoolRuleValidation(t *testing.T) {
	valid := `    rules:
      - {prefix: /photos, replicas: 2, prefer: [local-nas], avoid: [cheap]}
      - {prefix: /video, require: [fast]}
`
	if _, err := parsePool(t, valid); err != nil {
		t.Fatalf("valid rules rejected: %v", err)
	}
	cases := map[string]string{
		"relative prefix": `    rules:
      - {prefix: photos, replicas: 1}
`,
		"non-canonical prefix": `    rules:
      - {prefix: /photos/../x, replicas: 1}
`,
		"duplicate prefix": `    rules:
      - {prefix: /photos, replicas: 1}
      - {prefix: /photos, replicas: 1}
`,
		"replicas exceeds members": `    rules:
      - {prefix: /photos, replicas: 3}
`,
		"negative replicas": `    rules:
      - {prefix: /photos, replicas: -1}
`,
		"unknown prefer class": `    rules:
      - {prefix: /photos, prefer: [nope]}
`,
		"unknown avoid class": `    rules:
      - {prefix: /photos, avoid: [nope]}
`,
		"unknown require class": `    rules:
      - {prefix: /photos, require: [nope]}
`,
	}
	for name, extra := range cases {
		if _, err := parsePool(t, extra); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestPoolRuleZeroReplicasInheritsPoolReplicas(t *testing.T) {
	c, err := parsePool(t, `    replicas: 2
    rules:
      - {prefix: /photos, prefer: [fast]}
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pools["home"].Rules[0].Replicas != 0 {
		t.Fatalf("expected replicas to stay 0 (inherit), got %d", c.Pools["home"].Rules[0].Replicas)
	}
}

func TestPoolFailureDomainEnum(t *testing.T) {
	for _, v := range []string{"account", "provider", "member"} {
		if _, err := parsePool(t, "    failure_domain: "+v+"\n"); err != nil {
			t.Errorf("failure_domain %q rejected: %v", v, err)
		}
	}
	if _, err := parsePool(t, "    failure_domain: datacenter\n"); err == nil {
		t.Fatal("expected an error for an unknown failure_domain")
	}
}

func TestPoolWriteModeEnum(t *testing.T) {
	for _, v := range []string{"relaxed", "strict"} {
		if _, err := parsePool(t, "    write_mode: "+v+"\n"); err != nil {
			t.Errorf("write_mode %q rejected: %v", v, err)
		}
	}
	if _, err := parsePool(t, "    write_mode: yolo\n"); err == nil {
		t.Fatal("expected an error for an unknown write_mode")
	}
}

func TestPoolRebalanceValidation(t *testing.T) {
	// target_skew: 0 is indistinguishable from "unset" (like scrub_sample)
	// and defaults to 0.10 rather than being rejected.
	if _, err := parsePool(t, "    rebalance: {target_skew: 1}\n"); err == nil {
		t.Fatal("target_skew of 1 should be rejected (out of (0,1))")
	}
	if _, err := parsePool(t, "    rebalance: {target_skew: -0.1}\n"); err == nil {
		t.Fatal("negative target_skew should be rejected")
	}
	c, err := parsePool(t, "    rebalance: {auto_backfill: false}\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Pools["home"].Rebalance.AutoBackfill == nil || *c.Pools["home"].Rebalance.AutoBackfill {
		t.Fatal("explicit auto_backfill: false must be preserved, not defaulted back to true")
	}
}

func TestPoolPlacementErrorsMentionPool(t *testing.T) {
	_, err := parsePool(t, "    write_mode: yolo\n")
	if err == nil || !strings.Contains(err.Error(), "home") {
		t.Fatalf("error should name the pool: %v", err)
	}
}
