package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cloudfs/internal/config"
)

// The read path redacts an outbound's userinfo, and the write path refuses an
// address that carries one. Neither covers the third way a password reaches the
// page: an error about the address it could not use. net/url does not redact —
// a *url.Error prints the exact string it failed to parse — and the string the
// daemon is asked to apply is the STORED one, password included, whenever
// keep_credentials is set. A password holding a percent sign or a space is
// rejected by url.Parse, which is the very case the address splitter was
// written for, so this is not a hypothetical input.
func TestAFailureToApplyTheProxyDoesNotEchoTheStoredPassword(t *testing.T) {
	srv, _, path := accountsServer(t)
	const stored = "socks5://user:p%ss@127.0.0.1:7890"
	if err := config.SetProxy(path, config.Proxy{
		Outbounds: []config.Outbound{{Name: "hk", Type: "socks5", Addr: stored}},
		Rules:     []string{"FINAL,hk"},
	}); err != nil {
		t.Fatal(err)
	}
	srv.reloadConfigView()
	srv.collector.ReloadProxy = func(p config.Proxy) error {
		// What buildRouting does with an address url.Parse rejects.
		return errors.New(`parse "` + stored + `": invalid URL escape "%ss"`)
	}

	rr := accountRequest(t, srv, http.MethodGet, "/proxy/config", nil)
	var view ProxyConfig
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	view.Outbounds[0].KeepCredentials = true
	rr = accountRequest(t, srv, http.MethodPut, "/proxy/config", ProxyConfig{
		Outbounds: view.Outbounds, Groups: view.Groups, Rules: view.Rules,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body)
	}
	if strings.Contains(rr.Body.String(), "p%ss") || strings.Contains(rr.Body.String(), "user:") {
		t.Fatalf("the response carries the stored proxy password: %s", rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "127.0.0.1:7890") {
		t.Fatalf("the warning no longer says which outbound failed: %s", rr.Body)
	}
}

// The same error text reaches the page through http.Error when the write
// itself is refused, and that sink needs the same treatment.
func TestAProxyWriteRefusalDoesNotEchoAPasswordEither(t *testing.T) {
	if got := scrubProxyCredentials(`parse "socks5://user:p%ss@127.0.0.1:7890": invalid URL escape "%ss"`); strings.Contains(got, "user") || strings.Contains(got, "p%ss") {
		t.Fatalf("scrubbed text still carries the credential: %q", got)
	} else if !strings.Contains(got, "127.0.0.1:7890") {
		t.Fatalf("scrubbing removed the host as well: %q", got)
	}
	if got := scrubProxyCredentials("dial tcp 127.0.0.1:7890: connect: connection refused"); got != "dial tcp 127.0.0.1:7890: connect: connection refused" {
		t.Fatalf("an error with no credential in it was rewritten: %q", got)
	}
}
