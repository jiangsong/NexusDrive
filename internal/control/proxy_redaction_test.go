package control

import (
	"net/http"
	"strings"
	"testing"

	"cloudfs/internal/config"
)

// Redaction decided whether an address carried a credential by parsing it as
// a URL. Two ways out of that were open: a password containing a character
// that makes url.Parse fail, and type "direct", for which URL() returns
// (nil, nil) by design. Both made the function return the address verbatim —
// fail-open, on the read path, for a secret. The check is now textual and
// type-independent, so an address either has userinfo or it does not.
func TestRedactionFailsClosed(t *testing.T) {
	for _, c := range []struct {
		typ, addr string
		creds     bool
		redacted  string
	}{
		{"socks5", "socks5://user:pw@127.0.0.1:1080", true, "socks5://127.0.0.1:1080"},
		{"socks5", "socks5://user:p%ss@127.0.0.1:1080", true, "socks5://127.0.0.1:1080"},
		{"socks5", "socks5://user:p ss@127.0.0.1:1080", true, "socks5://127.0.0.1:1080"},
		{"direct", "socks5://user:pw@127.0.0.1:1080", true, "socks5://127.0.0.1:1080"},
		{"socks5", "user:pw@127.0.0.1:1080", true, "127.0.0.1:1080"},
		{"http", "http://proxy.local:3128", false, "http://proxy.local:3128"},
		{"socks5", "127.0.0.1:1080", false, "127.0.0.1:1080"},
		{"direct", "", false, ""},
	} {
		if got := addrHasCredentials(c.addr); got != c.creds {
			t.Errorf("addrHasCredentials(%q) = %v, want %v (type %q)", c.addr, got, c.creds, c.typ)
		}
		if got := redactAddr(c.addr); got != c.redacted {
			t.Errorf("redactAddr(%q) = %q, want %q (type %q)", c.addr, got, c.redacted, c.typ)
		}
		if c.creds && strings.Contains(redactAddr(c.addr), "pw") {
			t.Errorf("redactAddr(%q) leaked the password", c.addr)
		}
	}
}

// The write path decided the same question the same way, so declaring an
// outbound "direct" both smuggled a credential in and, with
// keep_credentials, handed the stored one back out on the next read.
func TestProxyWriteCannotLaunderACredentialThroughTheType(t *testing.T) {
	srv, _, path := accountsServer(t)
	if err := config.SetProxy(path, config.Proxy{
		Outbounds: []config.Outbound{{Name: "hk", Type: "socks5", Addr: "socks5://user:pw@127.0.0.1:7890"}},
		Rules:     []string{"FINAL,hk"},
	}); err != nil {
		t.Fatal(err)
	}
	srv.reloadConfigView()

	// Keeping the credential under a different type must be refused, not
	// quietly applied: the type is what decides whether the outbound proxies
	// at all, so accepting it would also stop every rule from proxying.
	rr := accountRequest(t, srv, http.MethodPut, "/proxy/config", ProxyConfig{
		Outbounds: []ProxyOutbound{{Name: "hk", Type: "direct", KeepCredentials: true}},
		Rules:     []string{"FINAL,hk"},
	})
	if rr.Code != 400 {
		t.Fatalf("type laundering was accepted: %d %s", rr.Code, rr.Body)
	}
	rr = accountRequest(t, srv, http.MethodGet, "/proxy/config", nil)
	if strings.Contains(rr.Body.String(), "user:pw") {
		t.Fatalf("the stored password came back through the control API: %s", rr.Body)
	}

	// Nor may one be written in under a type whose URL() ignores it.
	rr = accountRequest(t, srv, http.MethodPut, "/proxy/config", ProxyConfig{
		Outbounds: []ProxyOutbound{{Name: "x", Type: "direct", Addr: "socks5://attacker:hunter2@evil.example:1080"}},
		Rules:     []string{"FINAL,x"},
	})
	if rr.Code != 400 {
		t.Fatalf("an inbound credential was accepted as type direct: %d %s", rr.Code, rr.Body)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Proxy.Outbounds[0].Addr != "socks5://user:pw@127.0.0.1:7890" {
		t.Fatalf("the stored outbound was rewritten: %q", c.Proxy.Outbounds[0].Addr)
	}
}

// Renaming an outbound walked around the guard entirely: the server looked
// the stored one up by the new name, found nothing, and wrote the redacted
// address it had shown the page. A rename is not a way to lose a password.
func TestRenamingACredentialedOutboundIsRefused(t *testing.T) {
	srv, _, path := accountsServer(t)
	if err := config.SetProxy(path, config.Proxy{
		Outbounds: []config.Outbound{{Name: "corp", Type: "socks5", Addr: "socks5://alice:s3cr3t@10.0.0.1:1080"}},
		Rules:     []string{"FINAL,corp"},
	}); err != nil {
		t.Fatal(err)
	}
	srv.reloadConfigView()

	rr := accountRequest(t, srv, http.MethodPut, "/proxy/config", ProxyConfig{
		Outbounds: []ProxyOutbound{{Name: "corp2", Type: "socks5", Addr: "socks5://10.0.0.1:1080", KeepCredentials: true}},
		Rules:     []string{"FINAL,corp2"},
	})
	if rr.Code != 400 {
		t.Fatalf("a rename that would drop the credential was accepted: %d %s", rr.Code, rr.Body)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Proxy.Outbounds[0].Addr != "socks5://alice:s3cr3t@10.0.0.1:1080" {
		t.Fatalf("the credential did not survive: %q", c.Proxy.Outbounds[0].Addr)
	}
}

// keep_credentials and a new address are contradictory instructions. Taking
// the stored one silently discards an edit the person made on purpose.
func TestKeepCredentialsWithANewAddressIsRefused(t *testing.T) {
	srv, _, path := accountsServer(t)
	if err := config.SetProxy(path, config.Proxy{
		Outbounds: []config.Outbound{{Name: "hk", Type: "socks5", Addr: "socks5://user:pw@127.0.0.1:7890"}},
		Rules:     []string{"FINAL,hk"},
	}); err != nil {
		t.Fatal(err)
	}
	srv.reloadConfigView()
	rr := accountRequest(t, srv, http.MethodPut, "/proxy/config", ProxyConfig{
		Outbounds: []ProxyOutbound{{Name: "hk", Type: "socks5", Addr: "10.0.0.9:1080", KeepCredentials: true}},
		Rules:     []string{"FINAL,hk"},
	})
	if rr.Code != 400 {
		t.Fatalf("a contradictory write was accepted: %d %s", rr.Code, rr.Body)
	}
}

// Two outbounds with one name made the stored-address lookup ambiguous, and
// a keep_credentials write would copy one outbound's credential onto the
// other. The configuration has no use for duplicate names either way.
func TestDuplicateOutboundNamesAreRefused(t *testing.T) {
	srv, _, _ := accountsServer(t)
	rr := accountRequest(t, srv, http.MethodPut, "/proxy/config", ProxyConfig{
		Outbounds: []ProxyOutbound{
			{Name: "hk", Type: "socks5", Addr: "127.0.0.1:1080"},
			{Name: "hk", Type: "socks5", Addr: "127.0.0.2:1080"},
		},
		Rules: []string{"FINAL,hk"},
	})
	if rr.Code != 400 {
		t.Fatalf("duplicate outbound names were accepted: %d %s", rr.Code, rr.Body)
	}
}
