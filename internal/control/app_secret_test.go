package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/config"
	_ "cloudfs/internal/provider/gdrive"
)

// The OAuth application secret is the one credential-shaped value a browser
// has to be able to supply. A person who registers their own application —
// which Google Drive requires, because its scope is restricted and no
// application ships with the binary — cannot start any authorization without
// it, so refusing it in the page left the whole browser onboarding unable to
// finish and sent them back to a terminal to hand-edit credentials.
//
// It is not the account's credential: it identifies the application, not the
// person, and the token the application obtains still never crosses this
// boundary.

func appSecretServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "cache:\n  dir: " + filepath.Join(dir, "cache") +
		"\nsecrets: {backend: file, dir: " + filepath.Join(dir, "secrets") + "}\n" +
		"remotes:\n" +
		"  gd: {type: gdrive, client_id: app-id.apps.googleusercontent.com}\n" +
		"  nas: {type: webdav, url: 'https://nas.local/dav'}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(&Collector{Config: cfg, Version: "app-secret-test"})
	srv.auth = &AuthStarter{
		Supported: func(string) bool { return true },
		AppSecret: func(typ string) bool { return typ == "gdrive" },
	}
	return srv, path
}

func TestApplicationSecretNeverReachesTheConfigFile(t *testing.T) {
	srv, path := appSecretServer(t)
	rr := accountRequest(t, srv, http.MethodPost, "/accounts/gd/auth/app-secret", AppSecretRequest{ClientSecret: "GOCSPX-typed-in-the-browser"})
	if rr.Code != http.StatusOK {
		t.Fatalf("store app secret = %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "GOCSPX-typed-in-the-browser") {
		t.Fatal("the reply echoed the application secret")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "GOCSPX-typed-in-the-browser") {
		t.Fatal("the application secret was written into the configuration file")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := config.NewSecretStore(cfg).ResolveRemote(cfg.Remotes["gd"])
	if err != nil {
		t.Fatal(err)
	}
	if r.Extra["client_secret"] != "GOCSPX-typed-in-the-browser" {
		t.Fatalf("stored secret does not resolve: %v", r.Extra["client_secret"])
	}
}

// An account that has an application secret and no token is not authorized.
// Reporting it as authorized would tell a resumed setup to skip the one step
// still missing.
func TestApplicationSecretIsNotAnAuthorization(t *testing.T) {
	srv, _ := appSecretServer(t)
	if rr := accountRequest(t, srv, http.MethodPost, "/accounts/gd/auth/app-secret", AppSecretRequest{ClientSecret: "s"}); rr.Code != http.StatusOK {
		t.Fatalf("store = %d: %s", rr.Code, rr.Body.String())
	}
	var d AccountDetail
	decodeAccountJSON(t, accountRequest(t, srv, http.MethodGet, "/accounts/gd", nil), &d)
	if d.HasCredentials {
		t.Fatal("an application secret was reported as an authorization")
	}
	if !d.HasAppSecret {
		t.Fatal("the stored application secret is not reported")
	}
	if !d.AppSecret {
		t.Fatal("the account does not say it needs an application secret")
	}
	var list AccountsResponse
	decodeAccountJSON(t, accountRequest(t, srv, http.MethodGet, "/accounts", nil), &list)
	for _, a := range list.Remotes {
		if a.Name == "gd" && a.HasCredentials {
			t.Fatal("the account list reported an application secret as an authorization")
		}
	}
}

// Only the backends whose authorization actually needs one accept it: the
// endpoint is a hole in the credential boundary, so it is exactly as wide as
// the problem it solves.
func TestApplicationSecretRefusedWhereTheBackendHasNoApplication(t *testing.T) {
	srv, _ := appSecretServer(t)
	if rr := accountRequest(t, srv, http.MethodPost, "/accounts/nas/auth/app-secret", AppSecretRequest{ClientSecret: "s"}); rr.Code != http.StatusBadRequest {
		t.Fatalf("webdav app secret = %d, want 400", rr.Code)
	}
}

func TestApplicationSecretEndpointTakesNothingElse(t *testing.T) {
	srv, path := appSecretServer(t)
	rr := accountRequest(t, srv, http.MethodPost, "/accounts/gd/auth/app-secret", map[string]string{"refresh_token": "stolen"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("refresh token through the app-secret route = %d, want 400", rr.Code)
	}
	if rr := accountRequest(t, srv, http.MethodPost, "/accounts/gd/auth/app-secret", AppSecretRequest{ClientSecret: ""}); rr.Code != http.StatusBadRequest {
		t.Fatalf("empty app secret = %d, want 400", rr.Code)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "stolen") {
		t.Fatal("a rejected field reached the configuration file")
	}
}

// The start failure a person can act on gets its own answer. Everything else
// stays generic, because the daemon's setup errors can name a secrets path or
// proxy internals.
func TestAuthStartAsksForTheApplicationSecretInsteadOfAGenericFailure(t *testing.T) {
	srv, _ := appSecretServer(t)
	missing := &missingSecretError{}
	srv.auth.OAuth = func(ctx context.Context, name string, present func(string)) (string, func(context.Context) error, error) {
		return "", nil, missing
	}
	srv.auth.AppSecretMissing = func(err error) bool { return err == missing }
	rr := accountRequest(t, srv, http.MethodPost, "/accounts/gd/auth/start", AuthStartRequest{})
	if rr.Code != http.StatusPreconditionRequired {
		t.Fatalf("start without an application secret = %d, want 428: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "secrets") {
		t.Fatalf("the daemon's raw setup error reached the page: %s", rr.Body.String())
	}
}

// decodeAccountJSON reads a control reply into out, failing the test on a
// body that is not what the route documents.
func decodeAccountJSON(t *testing.T, rr *httptest.ResponseRecorder, out any) {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("request failed: %d %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
}

type missingSecretError struct{}

func (*missingSecretError) Error() string {
	return "client_secret is required; /home/x/.cloudfs/secrets"
}
