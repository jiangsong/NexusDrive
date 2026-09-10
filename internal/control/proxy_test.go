package control

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/net/proxy"
)

func TestProxyConfigRoundTripsAndRefusesCredentials(t *testing.T) {
	srv, _, path := accountsServer(t)
	in := ProxyConfig{
		Outbounds: []ProxyOutbound{{Name: "hk", Type: "socks5", Addr: "127.0.0.1:7890"}},
		Groups:    []ProxyGroup{{Name: "auto", Type: "url-test", Members: []string{"hk"}, Interval: "5m"}},
		Rules:     []string{"DOMAIN-SUFFIX,googleapis.com,auto", "FINAL,direct"},
	}
	rr := accountRequest(t, srv, http.MethodPut, "/proxy/config", in)
	if rr.Code != 200 {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body)
	}
	var out ProxyMutationResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Applied || !out.RestartRequired {
		t.Fatalf("without a live reload the reply must say restart: %+v", out)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Proxy.Outbounds) != 1 || c.Proxy.Groups[0].Interval.String() != "5m0s" || len(c.Proxy.Rules) != 2 {
		t.Fatalf("saved proxy: %+v", c.Proxy)
	}
	rr = accountRequest(t, srv, http.MethodGet, "/proxy/config", nil)
	var view ProxyConfig
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Default != "auto" || view.Groups[0].Interval != "5m0s" {
		t.Fatalf("view: %+v", view)
	}

	// A dangling reference is refused with its name, and the file is untouched.
	before, _ := os.ReadFile(path)
	bad := in
	bad.Rules = []string{"FINAL,nowhere"}
	if rr := accountRequest(t, srv, http.MethodPut, "/proxy/config", bad); rr.Code != 400 || !strings.Contains(rr.Body.String(), "nowhere") {
		t.Fatalf("dangling rule: %d %s", rr.Code, rr.Body)
	}
	withCreds := in
	withCreds.Outbounds = []ProxyOutbound{{Name: "hk", Type: "socks5", Addr: "socks5://user:pw@127.0.0.1:7890"}}
	// The refusal is rendered in the request's language, so this asserts the
	// outcome — refused, naming the outbound, and never echoing the secret —
	// rather than a phrase from one of the two catalogs.
	if rr := accountRequest(t, srv, http.MethodPut, "/proxy/config", withCreds); rr.Code != 400 ||
		!strings.Contains(rr.Body.String(), "hk") || strings.Contains(rr.Body.String(), "user:pw") {
		t.Fatalf("proxy password through the API: %d %s", rr.Code, rr.Body)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("a refused PUT changed the file")
	}

	// Live apply when the daemon offers it; a failing apply is reported as
	// saved-but-not-live, never as applied.
	applied := 0
	srv.collector.ReloadProxy = func(config.Proxy) error { applied++; return nil }
	rr = accountRequest(t, srv, http.MethodPut, "/proxy/config", in)
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if rr.Code != 200 || !out.Applied || out.RestartRequired || applied != 1 {
		t.Fatalf("live apply: %d %+v applied=%d", rr.Code, out, applied)
	}
	srv.collector.ReloadProxy = func(config.Proxy) error { return os.ErrPermission }
	rr = accountRequest(t, srv, http.MethodPut, "/proxy/config", in)
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if rr.Code != 200 || out.Applied || !out.RestartRequired || out.Warning == "" {
		t.Fatalf("failed apply: %d %+v", rr.Code, out)
	}
}

func TestProxyExplainAndCheckUseTheLiveManager(t *testing.T) {
	srv, _, _ := accountsServer(t)
	if rr := accountRequest(t, srv, http.MethodGet, "/proxy/explain?host=www.googleapis.com", nil); rr.Code != 501 {
		t.Fatalf("explain without a manager: %d", rr.Code)
	}
	m, err := proxy.NewManager(proxy.ManagerOptions{
		Outbounds: []proxy.Outbound{{Name: "hk", Type: "socks5", Addr: "127.0.0.1:7890"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.collector.Proxy = m
	rr := accountRequest(t, srv, http.MethodGet, "/proxy/explain?host=www.googleapis.com", nil)
	var out ProxyExplainResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Outbound != "hk" || out.Resolved != "hk" || !strings.HasPrefix(out.Rule, "DOMAIN-SUFFIX,googleapis.com") {
		t.Fatalf("explain: %+v", out)
	}
	rr = accountRequest(t, srv, http.MethodGet, "/proxy/explain?host=www.aliyundrive.com", nil)
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Outbound != "direct" {
		t.Fatalf("a domestic host: %+v", out)
	}
	if rr := accountRequest(t, srv, http.MethodGet, "/proxy/explain?host=", nil); rr.Code != 400 {
		t.Fatalf("empty host: %d", rr.Code)
	}
	if rr := accountRequest(t, srv, http.MethodPost, "/proxy/check", nil); rr.Code != 200 {
		t.Fatalf("check: %d %s", rr.Code, rr.Body)
	}
}
