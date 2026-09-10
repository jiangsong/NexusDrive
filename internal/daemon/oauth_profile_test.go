package daemon

import (
	"strings"
	"testing"
)

// Google Drive and Box are OAuth backends whose credential the daemon can
// fetch on the user's behalf. Without a profile the only way in was
// `config auth --stdin` with a refresh token the user had to mint elsewhere.
func TestGoogleDriveAndBoxHaveABrowserOAuthProfile(t *testing.T) {
	for _, typ := range []string{"aliyun", "baidu", "gdrive", "box"} {
		if !SupportsDaemonAuth(typ) {
			t.Errorf("%s cannot be authorized by the daemon, so no browser wizard can drive it", typ)
		}
		p, ok := OAuthProfileFor(typ)
		if !ok {
			t.Fatalf("%s has no OAuth profile", typ)
		}
		if !strings.HasPrefix(p.AuthorizeURL, "https://") || !strings.HasPrefix(p.TokenURL, "https://") {
			t.Errorf("%s endpoints = %q %q", typ, p.AuthorizeURL, p.TokenURL)
		}
		if p.Scope == "" {
			t.Errorf("%s requests no scope", typ)
		}
	}
	if _, ok := OAuthProfileFor("pan115"); ok {
		t.Error("115 authorizes by device code, not by redirect")
	}
	if _, ok := OAuthProfileFor("smb"); ok {
		t.Error("SMB authenticates with a password; there is no authorization server")
	}
}

// Google issues a refresh token only when the authorization asks for offline
// access and forces the consent screen. Without both, the exchange returns an
// access token that expires in an hour and the account stops working.
func TestGoogleDriveAsksForOfflineAccess(t *testing.T) {
	p, ok := OAuthProfileFor("gdrive")
	if !ok {
		t.Fatal("no gdrive profile")
	}
	if p.AuthParams.Get("access_type") != "offline" {
		t.Errorf("access_type = %q, want offline", p.AuthParams.Get("access_type"))
	}
	if p.AuthParams.Get("prompt") != "consent" {
		t.Errorf("prompt = %q, want consent", p.AuthParams.Get("prompt"))
	}
	if !p.FormToken {
		t.Error("Google's token endpoint takes a form-encoded POST")
	}
	if !strings.Contains(p.Scope, "drive") {
		t.Errorf("scope = %q, want the Drive scope", p.Scope)
	}
}

// Box's token endpoint is also a form POST, and it returns a refresh token
// without extra authorization parameters.
func TestBoxUsesTheFormTokenExchange(t *testing.T) {
	p, ok := OAuthProfileFor("box")
	if !ok {
		t.Fatal("no box profile")
	}
	if !p.FormToken || p.JSONToken {
		t.Errorf("box exchange mode = form:%v json:%v", p.FormToken, p.JSONToken)
	}
	if !strings.Contains(p.TokenURL, "box.com") || !strings.Contains(p.AuthorizeURL, "box.com") {
		t.Errorf("box endpoints = %q %q", p.AuthorizeURL, p.TokenURL)
	}
}
