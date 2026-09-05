package proxy

import (
	"net"
	"testing"
)

func TestParse(t *testing.T) {
	good := []string{
		"DOMAIN-SUFFIX,googleapis.com,proxy",
		"domain,openapi.alipan.com,direct",
		"DOMAIN-KEYWORD,baidu,direct",
		"IP-CIDR,10.0.0.0/8,direct",
		"GEOIP,cn,direct",
		"FINAL,direct",
	}
	for _, g := range good {
		if _, err := Parse(g); err != nil {
			t.Errorf("Parse(%q) = %v", g, err)
		}
	}
	bad := []string{"", "DOMAIN-SUFFIX,x", "IP-CIDR,notacidr,direct", "MAGIC,x,y", "FINAL,a,b"}
	for _, b := range bad {
		if _, err := Parse(b); err == nil {
			t.Errorf("Parse(%q) should fail", b)
		}
	}
}

func TestRouter(t *testing.T) {
	r, err := NewRouter([]string{
		"# comment",
		"DOMAIN,openapi.alipan.com,cn-exit",
		"DOMAIN-SUFFIX,googleapis.com,proxy",
		"DOMAIN-KEYWORD,dropbox,proxy",
		"IP-CIDR,192.168.0.0/16,direct",
		"GEOIP,CN,direct",
		"FINAL,fallback",
	})
	if err != nil {
		t.Fatal(err)
	}
	r.GeoIP = func(ip net.IP) string {
		if ip.Equal(net.ParseIP("1.2.3.4")) {
			return "CN"
		}
		return "US"
	}
	cases := []struct {
		tgt  Target
		want string
	}{
		{Target{Host: "openapi.alipan.com"}, "cn-exit"},
		{Target{Host: "www.googleapis.com"}, "proxy"},
		{Target{Host: "googleapis.com"}, "proxy"},
		{Target{Host: "notgoogleapis.com"}, "fallback"},
		{Target{Host: "content.dropboxapi.com"}, "proxy"},
		{Target{Host: "192.168.1.9"}, "direct"},
		{Target{Host: "cdn.example.com", IP: net.ParseIP("1.2.3.4")}, "direct"},
		{Target{Host: "cdn.example.com", IP: net.ParseIP("8.8.8.8")}, "fallback"},
		{Target{Host: "cdn.example.com"}, "fallback"},
		{Target{Host: "WWW.GOOGLEAPIS.COM."}, "proxy"},
	}
	for _, c := range cases {
		if got := r.Outbound(c.tgt); got != c.want {
			t.Errorf("Outbound(%+v) = %q, want %q", c.tgt, got, c.want)
		}
	}
	if e := r.Explain(Target{Host: "x.googleapis.com"}); e == nil || e.Kind != DomainSuffix {
		t.Fatalf("Explain = %+v", e)
	}
}

func TestDefaultRulesParse(t *testing.T) {
	r, err := NewRouter(DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	if r.Outbound(Target{Host: "www.googleapis.com"}) != "proxy" {
		t.Fatal("googleapis should go through proxy by default")
	}
	if r.Outbound(Target{Host: "openapi.alipan.com"}) != "direct" {
		t.Fatal("unlisted hosts should go direct by default")
	}
}
