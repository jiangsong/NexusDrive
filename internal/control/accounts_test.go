package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/config"
	_ "cloudfs/internal/provider/smb"
	_ "cloudfs/internal/provider/webdav"
)

func accountsServer(t *testing.T) (*Server, *config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "cache:\n  dir: " + filepath.Join(dir, "cache") + "\nremotes:\n  existing: {type: webdav, url: 'https://nas.local/dav'}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(&Collector{Config: cfg, Version: "accounts-test"}), cfg, path
}

func accountRequest(t *testing.T, srv *Server, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Host = "127.0.0.1:9101"
	if method != http.MethodGet {
		req.Header.Set("X-CloudFS-Control", "1")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestAccountsListsWhatEachDriverNeeds(t *testing.T) {
	srv, _, path := accountsServer(t)
	rr := accountRequest(t, srv, http.MethodGet, "/accounts", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /accounts = %d %s", rr.Code, rr.Body)
	}
	var out AccountsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Configurable || out.ConfigPath != path {
		t.Fatalf("configurable=%v path=%q", out.Configurable, out.ConfigPath)
	}
	if len(out.Remotes) != 1 || out.Remotes[0].Name != "existing" || out.Remotes[0].Type != "webdav" {
		t.Fatalf("remotes = %+v", out.Remotes)
	}
	var smb *AccountType
	for i := range out.Types {
		if out.Types[i].Type == "smb" {
			smb = &out.Types[i]
		}
	}
	if smb == nil {
		t.Fatalf("smb is not offered: %+v", out.Types)
	}
	required := map[string]bool{}
	for _, f := range smb.Fields {
		if f.Required {
			required[f.Name] = true
		}
	}
	for _, name := range []string{"host", "share", "user"} {
		if !required[name] {
			t.Fatalf("%s is not reported as required: %+v", name, smb.Fields)
		}
	}
	if smb.Credentials == "" {
		t.Fatal("the page is not told what the credential step will ask for")
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache control = %q; this reply names configured remotes", rr.Header().Get("Cache-Control"))
	}
}

func TestAccountsAddsARemoteAndPointsAtTheCredentialStep(t *testing.T) {
	srv, _, path := accountsServer(t)
	rr := accountRequest(t, srv, http.MethodPost, "/accounts", AddAccountRequest{
		Name: "nas2", Type: "smb",
		Fields: map[string]string{"host": "192.168.0.30", "share": "media", "user": "work", "root": "/movies"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /accounts = %d %s", rr.Code, rr.Body)
	}
	var out AddAccountResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.NextCommand, "config auth nas2") {
		t.Fatalf("next command = %q", out.NextCommand)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := reloaded.Remotes["nas2"]
	if !ok {
		t.Fatal("the remote was not written to the configuration")
	}
	if r.Type != "smb" {
		t.Fatalf("type = %q", r.Type)
	}
	for key, want := range map[string]string{"host": "192.168.0.30", "share": "media", "user": "work", "root": "/movies"} {
		if got, _ := r.Extra[key].(string); got != want {
			t.Fatalf("%s = %v, want %q", key, r.Extra[key], want)
		}
	}
	// And the existing remote is untouched.
	if _, ok := reloaded.Remotes["existing"]; !ok {
		t.Fatal("adding a remote removed another one")
	}
}

// TestAccountsRefusesCredentials is the boundary this endpoint exists to hold.
// A cloud drive's password or refresh token is the most sensitive value in the
// system; it does not travel through a browser form to save one command.
func TestAccountsRefusesCredentials(t *testing.T) {
	srv, _, path := accountsServer(t)
	for _, key := range []string{"password", "refresh_token", "access_token", "client_secret", "cookie", "ntlm_hash", "secret_access_key"} {
		rr := accountRequest(t, srv, http.MethodPost, "/accounts", AddAccountRequest{
			Name: "leaky", Type: "smb",
			Fields: map[string]string{"host": "h", "share": "s", "user": "u", key: "super-secret"},
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s was accepted: %d %s", key, rr.Code, rr.Body)
		}
		if !strings.Contains(rr.Body.String(), "config auth") {
			t.Fatalf("%s: the refusal does not say where credentials go: %s", key, rr.Body)
		}
		if strings.Contains(rr.Body.String(), "super-secret") {
			t.Fatalf("%s: the rejected value was echoed back", key)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret") || strings.Contains(string(raw), "leaky") {
		t.Fatalf("a rejected request still wrote to the configuration:\n%s", raw)
	}
}

func TestAccountsValidatesNamesTypesAndSettings(t *testing.T) {
	srv, _, path := accountsServer(t)
	cases := []struct {
		name string
		body AddAccountRequest
	}{
		{"empty name", AddAccountRequest{Type: "smb", Fields: map[string]string{"host": "h", "share": "s", "user": "u"}}},
		{"path in name", AddAccountRequest{Name: "../etc", Type: "smb", Fields: map[string]string{"host": "h", "share": "s", "user": "u"}}},
		{"unknown type", AddAccountRequest{Name: "x", Type: "nosuch", Fields: map[string]string{}}},
		{"missing required", AddAccountRequest{Name: "x", Type: "smb", Fields: map[string]string{"host": "h"}}},
		{"structural key", AddAccountRequest{Name: "x", Type: "smb", Fields: map[string]string{"host": "h", "share": "s", "user": "u", "type": "webdav"}}},
		{"private key", AddAccountRequest{Name: "x", Type: "smb", Fields: map[string]string{"host": "h", "share": "s", "user": "u", "_http_client": "x"}}},
		{"newline in value", AddAccountRequest{Name: "x", Type: "smb", Fields: map[string]string{"host": "h\nevil: true", "share": "s", "user": "u"}}},
		{"prefix without mount", AddAccountRequest{Name: "x", Type: "smb", Prefix: "/x", Fields: map[string]string{"host": "h", "share": "s", "user": "u"}}},
	}
	for _, tc := range cases {
		rr := accountRequest(t, srv, http.MethodPost, "/accounts", tc.body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s was accepted: %d %s", tc.name, rr.Code, rr.Body)
		}
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "evil") {
		t.Fatalf("a rejected value reached the configuration:\n%s", raw)
	}
}

func TestAccountsRejectsCrossSiteAndUnmarkedRequests(t *testing.T) {
	srv, _, _ := accountsServer(t)
	body, _ := json.Marshal(AddAccountRequest{Name: "x", Type: "smb",
		Fields: map[string]string{"host": "h", "share": "s", "user": "u"}})

	// A cross-site form submission carries no custom header.
	req := httptest.NewRequest(http.MethodPost, "/accounts", bytes.NewReader(body))
	req.Host = "127.0.0.1:9101"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("a request without X-CloudFS-Control = %d", rr.Code)
	}

	// A cross-origin fetch is refused even with the header.
	req = httptest.NewRequest(http.MethodPost, "/accounts", bytes.NewReader(body))
	req.Host = "127.0.0.1:9101"
	req.Header.Set("X-CloudFS-Control", "1")
	req.Header.Set("Origin", "http://evil.example")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("a cross-origin request = %d", rr.Code)
	}

	// So is a DNS-rebound host.
	req = httptest.NewRequest(http.MethodPost, "/accounts", bytes.NewReader(body))
	req.Host = "attacker.example"
	req.Header.Set("X-CloudFS-Control", "1")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("a rebound host = %d", rr.Code)
	}
}

func TestAccountsWithoutAConfigFileSaysSo(t *testing.T) {
	srv := NewServer(&Collector{Version: "no-config"})
	rr := accountRequest(t, srv, http.MethodGet, "/accounts", nil)
	var out AccountsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Configurable {
		t.Fatal("a daemon with no configuration file reported itself configurable")
	}
	if len(out.Types) == 0 {
		t.Fatal("the backend list should still be available")
	}
	rr = accountRequest(t, srv, http.MethodPost, "/accounts", AddAccountRequest{Name: "x", Type: "smb"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("adding without a configuration file = %d %s", rr.Code, rr.Body)
	}
}

// TestTheAccountFormNeverAsksForACredential: the page must not grow a
// password box. The server would refuse it, but a form that asks for one has
// already taught the user to type it into a browser.

// writeConfigLine appends a remote of the given type to the server's config
// file and reloads the server's view, for tests that need one that is not
// webdav.
func writeConfigLine(t *testing.T, cfg *config.Config, name, rtype string) {
	t.Helper()
	f, err := os.OpenFile(cfg.SourcePath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("  " + name + ": {type: " + rtype + ", client_id: app, client_secret: s}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	fresh, err := config.Load(cfg.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	*cfg = *fresh
}
