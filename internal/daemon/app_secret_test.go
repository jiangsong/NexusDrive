package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"cloudfs/internal/config"
)

// A confidential OAuth client cannot exchange a code without its application
// secret, and the person who registered the application is the only source of
// one. The page needs to know which backends are in that position, and the
// failure has to be recognizable rather than another opaque setup error.

func TestBackendsThatNeedAnApplicationSecretAreNamed(t *testing.T) {
	if !NeedsAppSecret("gdrive") {
		t.Fatal("gdrive registers a confidential client; it needs an application secret")
	}
	if NeedsAppSecret("dropbox") {
		t.Fatal("dropbox authorizes as a public client (PKCE); asking for a secret it never issues is a dead end")
	}
	if NeedsAppSecret("sftp") {
		t.Fatal("a backend with no OAuth profile has no application at all")
	}
}

func TestMissingApplicationSecretIsRecognizable(t *testing.T) {
	r := config.Remote{Type: "gdrive", Extra: map[string]any{"client_id": "app.apps.googleusercontent.com"}}
	profile, _ := OAuthProfileFor("gdrive")
	_, _, err := ResolveOAuthClient(nil, r, profile, nil)
	if !errors.Is(err, ErrClientSecretRequired) {
		t.Fatalf("resolve without a secret = %v, want ErrClientSecretRequired", err)
	}
}

// The control plane learns both facts from here, so the wiring is asserted
// rather than assumed: without it the page cannot tell a missing application
// secret from any other refusal, which is the state that sent people back to
// the terminal.
func TestAuthStarterCarriesTheApplicationSecretFacts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "cache:\n  dir: " + filepath.Join(dir, "cache") + "\nremotes:\n  gd: {type: gdrive, client_id: app}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	starter := AuthStarterFor(func() *config.Config { return cfg })
	if starter == nil || starter.AppSecret == nil || starter.AppSecretMissing == nil {
		t.Fatal("the control plane was not given the application-secret hooks")
	}
	if !starter.AppSecret("gdrive") || starter.AppSecret("dropbox") {
		t.Fatal("AppSecret does not report which backends need one")
	}
	if !starter.AppSecretMissing(fmt.Errorf("wrapped: %w", ErrClientSecretRequired)) {
		t.Fatal("a wrapped ErrClientSecretRequired is not recognized")
	}
	if starter.AppSecretMissing(errors.New("proxy unreachable")) {
		t.Fatal("an unrelated failure was reported as a missing application secret")
	}
}
