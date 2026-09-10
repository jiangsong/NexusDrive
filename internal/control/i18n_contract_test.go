package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloudfs/internal/i18n"
)

// The refusal a person reads is rendered from the catalog in their language,
// and nothing branches on its text: the gate answers the request itself, so
// there is no sentinel to preserve and no format string carrying a %w that a
// translation could drop.
func TestTheConfirmationRefusalIsLocalized(t *testing.T) {
	seen := map[string]bool{}
	for _, lang := range []i18n.Lang{i18n.ZH, i18n.EN} {
		rr := httptest.NewRecorder()
		if confirmed(rr, langRequest(lang), false, "confirm.remove_account") {
			t.Fatalf("%s: an unconfirmed call was allowed through", lang)
		}
		body := strings.TrimSpace(rr.Body.String())
		if body == "" || seen[body] {
			t.Fatalf("%s: refusal did not follow the language: %q", lang, body)
		}
		seen[body] = true
	}
}

// A missing key renders as the key itself, so a gap is visible rather than
// blank. Running it through Sprintf with the arguments turned that promise
// into doctor.free.low%!(EXTRA string=...), which is neither the key nor a
// sentence.
func TestAMissingKeyRendersAsTheBareKey(t *testing.T) {
	if got := i18n.T(i18n.EN, "no.such.key.here", "arg", 3); got != "no.such.key.here" {
		t.Fatalf("a missing key with arguments rendered as %q", got)
	}
}

// The refusals that reject a bad group interval, timeout or check URL are
// shown verbatim in the page's toast, so they belong in the catalog with
// every other refusal on that screen.
func TestGroupValidationRefusalsAreLocalized(t *testing.T) {
	srv, _, _ := accountsServer(t)
	body := ProxyConfig{
		Outbounds: []ProxyOutbound{{Name: "hk", Type: "socks5", Addr: "127.0.0.1:1080"}},
		Groups:    []ProxyGroup{{Name: "auto", Type: "url-test", Members: []string{"hk"}, Interval: "nonsense"}},
		Rules:     []string{"FINAL,auto"},
	}
	zh := proxyPutIn(t, srv, body, "")
	en := proxyPutIn(t, srv, body, "en")
	if zh.Code != 400 || en.Code != 400 {
		t.Fatalf("an invalid interval was accepted: %d %d", zh.Code, en.Code)
	}
	if zh.Body.String() == en.Body.String() {
		t.Fatalf("the refusal reads the same in both languages: %s", zh.Body)
	}
}

// Failing to write the configuration file is this machine's problem, not the
// request's: a full disk or a read-only mount is not a bad request.
func TestAFailedConfigWriteIsAServerError(t *testing.T) {
	srv, _, path := accountsServer(t)
	if err := makeUnwritable(path); err != nil {
		t.Skipf("cannot make the config unwritable here: %v", err)
	}
	defer restoreWritable(path)
	rr := accountRequest(t, srv, http.MethodPut, "/proxy/config", ProxyConfig{
		Outbounds: []ProxyOutbound{{Name: "hk", Type: "socks5", Addr: "127.0.0.1:1080"}},
		Rules:     []string{"FINAL,hk"},
	})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("a failed write answered %d, want 500: %s", rr.Code, rr.Body)
	}
}
