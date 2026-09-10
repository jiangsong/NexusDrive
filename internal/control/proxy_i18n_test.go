package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloudfs/internal/config"
)

// Refusing a write because the address came back redacted is a sentence a
// person reads and acts on, so it belongs in the catalog like every other
// control-API refusal. An English literal here would be the one refusal on
// this screen that ignores the reader's language.
func TestTheRedactedAddressRefusalIsLocalized(t *testing.T) {
	srv, _, path := accountsServer(t)
	if err := config.SetProxy(path, config.Proxy{
		Outbounds: []config.Outbound{{Name: "hk", Type: "socks5", Addr: "socks5://user:pw@127.0.0.1:7890"}},
		Rules:     []string{"FINAL,hk"},
	}); err != nil {
		t.Fatal(err)
	}
	srv.reloadConfigView()

	body := ProxyConfig{
		Outbounds: []ProxyOutbound{{Name: "hk", Type: "socks5", Addr: "socks5://127.0.0.1:7890"}},
		Rules:     []string{"FINAL,hk"},
	}
	zh := proxyPutIn(t, srv, body, "")
	en := proxyPutIn(t, srv, body, "en")
	if zh.Code != 400 || en.Code != 400 {
		t.Fatalf("expected both to be refused: %d %d", zh.Code, en.Code)
	}
	if zh.Body.String() == en.Body.String() {
		t.Fatalf("the refusal reads the same in both languages: %s", zh.Body)
	}
	if !strings.Contains(en.Body.String(), "keep_credentials") || !strings.Contains(zh.Body.String(), "keep_credentials") {
		t.Fatalf("the refusal stopped naming the field that fixes it:\nzh=%s\nen=%s", zh.Body, en.Body)
	}
	for _, rr := range []*httptest.ResponseRecorder{zh, en} {
		if !strings.Contains(rr.Body.String(), "hk") {
			t.Errorf("the refusal does not say which outbound: %s", rr.Body)
		}
	}
}

// proxyPutIn writes the proxy section in one language. An empty lang leaves
// the header off, which is the default the daemon falls back to.
func proxyPutIn(t *testing.T, srv *Server, body ProxyConfig, lang string) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/proxy/config", bytes.NewReader(encoded))
	req.Host = "127.0.0.1:9101"
	req.Header.Set("X-CloudFS-Control", "1")
	req.Header.Set("Content-Type", "application/json")
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

// A proxy section that saved but could not be applied live is the outcome a
// person most needs to understand — the file and the running daemon now
// disagree — and the page shows this text verbatim. Before the screen could
// write, nothing surfaced it; now that it can, the sentence needs the same
// treatment as every other one the page prints.
func TestTheSavedButNotAppliedWarningIsLocalized(t *testing.T) {
	srv, _, _ := accountsServer(t)
	srv.collector.ReloadProxy = func(config.Proxy) error { return errStubReload }
	body := ProxyConfig{
		Outbounds: []ProxyOutbound{{Name: "hk", Type: "socks5", Addr: "127.0.0.1:7890"}},
		Rules:     []string{"FINAL,hk"},
	}
	zh := proxyPutIn(t, srv, body, "")
	en := proxyPutIn(t, srv, body, "en")
	if zh.Code != 200 || en.Code != 200 {
		t.Fatalf("the save itself must succeed: %d %d", zh.Code, en.Code)
	}
	var zhOut, enOut ProxyMutationResponse
	if err := json.Unmarshal(zh.Body.Bytes(), &zhOut); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(en.Body.Bytes(), &enOut); err != nil {
		t.Fatal(err)
	}
	if zhOut.Applied || !zhOut.RestartRequired || zhOut.Warning == "" {
		t.Fatalf("a failed reload must not read as applied: %+v", zhOut)
	}
	if zhOut.Warning == enOut.Warning {
		t.Fatalf("the warning reads the same in both languages: %q", zhOut.Warning)
	}
	for _, w := range []string{zhOut.Warning, enOut.Warning} {
		if !strings.Contains(w, errStubReload.Error()) {
			t.Errorf("the warning drops the reason the daemon gave: %q", w)
		}
	}
}

var errStubReload = errors.New("outbound hk is unreachable")
