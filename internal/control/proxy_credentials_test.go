package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cloudfs/internal/config"
)

// GET /proxy/config redacts the userinfo out of an outbound address, which is
// what a person reading the page should see. It also means the page cannot
// send that address back: doing so would rewrite the file with the password
// stripped, silently breaking the outbound the next time the daemon starts.
// The view therefore marks which outbounds carry a credential, and a write
// that would round-trip the redacted form is either told to keep the stored
// address or refused — never quietly accepted.
func TestProxyConfigDoesNotLoseCredentialsOnARoundTrip(t *testing.T) {
	srv, _, path := accountsServer(t)
	if err := config.SetProxy(path, config.Proxy{
		Outbounds: []config.Outbound{{Name: "hk", Type: "socks5", Addr: "socks5://user:pw@127.0.0.1:7890"}},
		Rules:     []string{"FINAL,hk"},
	}); err != nil {
		t.Fatal(err)
	}
	srv.reloadConfigView()

	rr := accountRequest(t, srv, http.MethodGet, "/proxy/config", nil)
	var view ProxyConfig
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Outbounds) != 1 || strings.Contains(rr.Body.String(), "user:pw") {
		t.Fatalf("the view leaked or lost the outbound: %s", rr.Body)
	}
	if !view.Outbounds[0].HasCredentials {
		t.Fatal("the view does not say the outbound carries a credential, so the page cannot know not to overwrite it")
	}

	// Sending the redacted address straight back is the accident this guards.
	back := ProxyConfig{Outbounds: view.Outbounds, Groups: view.Groups, Rules: view.Rules}
	rr = accountRequest(t, srv, http.MethodPut, "/proxy/config", back)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "keep_credentials") {
		t.Fatalf("the redacted address was accepted as a new one: %d %s", rr.Code, rr.Body)
	}

	// With the flag the stored address survives untouched.
	back.Outbounds[0].KeepCredentials = true
	rr = accountRequest(t, srv, http.MethodPut, "/proxy/config", back)
	if rr.Code != 200 {
		t.Fatalf("PUT with keep_credentials: %d %s", rr.Code, rr.Body)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Proxy.Outbounds[0].Addr != "socks5://user:pw@127.0.0.1:7890" {
		t.Fatalf("the stored credential did not survive the write: %q", c.Proxy.Outbounds[0].Addr)
	}

	// A genuinely new address still replaces it, credential and all.
	next := ProxyConfig{
		Outbounds: []ProxyOutbound{{Name: "hk", Type: "socks5", Addr: "127.0.0.1:1080"}},
		Rules:     []string{"FINAL,hk"},
	}
	if rr = accountRequest(t, srv, http.MethodPut, "/proxy/config", next); rr.Code != 200 {
		t.Fatalf("replacing the address: %d %s", rr.Code, rr.Body)
	}
	if c, err = config.Load(path); err != nil {
		t.Fatal(err)
	}
	if c.Proxy.Outbounds[0].Addr != "127.0.0.1:1080" {
		t.Fatalf("the new address did not land: %q", c.Proxy.Outbounds[0].Addr)
	}
}
