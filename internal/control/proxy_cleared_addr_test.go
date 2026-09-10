package control

import (
	"net/http"
	"strings"
	"testing"

	"cloudfs/internal/config"
)

// Clearing the address field of an outbound that carries a credential must
// not destroy the stored password. proxyConfigFromView alone does produce
// Addr:"" for this shape — no case covers hadCredential with an empty
// address — but the write does not reach the file: config.Validate refuses
// an outbound with no address, so the stored credential survives. This pins
// that second line of defence, because the first one does not hold it.
func TestClearingTheAddressOfACredentialedOutboundIsRefused(t *testing.T) {
	srv, _, path := accountsServer(t)
	const stored = "socks5://bob:s3cret@10.0.0.9:1080"
	if err := config.SetProxy(path, config.Proxy{
		Outbounds: []config.Outbound{{Name: "office", Type: "socks5", Addr: stored}},
		Rules:     []string{"FINAL,office"},
	}); err != nil {
		t.Fatal(err)
	}
	srv.reloadConfigView()

	back := ProxyConfig{
		Outbounds: []ProxyOutbound{{Name: "office", Type: "socks5", Addr: ""}},
		Rules:     []string{"FINAL,office"},
	}
	rr := accountRequest(t, srv, http.MethodPut, "/proxy/config", back)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("an emptied address was accepted: %d %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "office") {
		t.Errorf("the refusal does not name the outbound: %s", rr.Body)
	}
	// Known gap, not a data loss: this refusal comes from config.Validate and
	// is raw English, not a catalog entry, so a zh reader sees it untranslated.
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Proxy.Outbounds[0].Addr != stored {
		t.Fatalf("the stored credential was destroyed: %q", c.Proxy.Outbounds[0].Addr)
	}
}
