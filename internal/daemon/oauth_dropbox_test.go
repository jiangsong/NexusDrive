package daemon

import (
	"strings"
	"testing"

	"cloudfs/internal/auth"
	"cloudfs/internal/config"
)

// Dropbox was the one drive a person could not add without leaving the app:
// with no profile, `config auth` fell back to asking for a single pasted
// access token, which Dropbox expires in hours. The account then stopped
// working with no sign of why. A profile is what turns that into the same
// browser round-trip every other OAuth backend gets.
func TestDropboxHasABrowserOAuthProfileSoConfigAuthNeedNotAskForAPastedToken(t *testing.T) {
	if !SupportsDaemonAuth("dropbox") {
		t.Fatal("dropbox cannot be authorized by the daemon, so no browser wizard can drive it")
	}
	p, ok := OAuthProfileFor("dropbox")
	if !ok {
		t.Fatal("dropbox has no OAuth profile")
	}
	if !strings.HasPrefix(p.AuthorizeURL, "https://") || !strings.HasPrefix(p.TokenURL, "https://") {
		t.Errorf("dropbox endpoints = %q %q", p.AuthorizeURL, p.TokenURL)
	}
	if !strings.Contains(p.AuthorizeURL, "dropbox.com") || !strings.Contains(p.TokenURL, "dropbox") {
		t.Errorf("dropbox endpoints do not point at Dropbox: %q %q", p.AuthorizeURL, p.TokenURL)
	}
}

// A Dropbox access token lives for hours; a refresh token lives until it is
// revoked. The difference is one authorization parameter, and getting it wrong
// is invisible until the account stops working the same afternoon.
func TestDropboxAsksForOfflineAccessSoTheGrantOutlivesTheAccessToken(t *testing.T) {
	p, ok := OAuthProfileFor("dropbox")
	if !ok {
		t.Fatal("no dropbox profile")
	}
	if got := p.AuthParams.Get("token_access_type"); got != "offline" {
		t.Errorf("token_access_type = %q, want offline", got)
	}
	if !p.FormToken || p.JSONToken {
		t.Errorf("dropbox exchange mode = form:%v json:%v; its token endpoint takes a form POST", p.FormToken, p.JSONToken)
	}
	for _, want := range []string{"files.content.read", "files.content.write", "files.metadata.write", "account_info.read"} {
		if !strings.Contains(p.Scope, want) {
			t.Errorf("scope %q does not ask for %s, which the driver calls", p.Scope, want)
		}
	}
}

// A shipped desktop binary cannot hold a secret. PKCE binds the authorization
// code to a per-attempt verifier instead, which is what lets this profile work
// with nothing but a client id — the value Dropbox itself calls public.
func TestDropboxAuthorizesAsAPublicClientWithPKCE(t *testing.T) {
	p, ok := OAuthProfileFor("dropbox")
	if !ok {
		t.Fatal("no dropbox profile")
	}
	if !p.PKCE {
		t.Fatal("dropbox does not ask for PKCE, so authorizing it would need a client secret nobody has")
	}
}

// The profile is only a description; Apply is what carries it into the options
// the authorization actually runs with. A field added to the table and not to
// Apply is a setting that reads correctly in a test and does nothing in life.
func TestApplyCarriesThePKCEChoiceIntoTheAuthorizationOptions(t *testing.T) {
	p, ok := OAuthProfileFor("dropbox")
	if !ok {
		t.Fatal("no dropbox profile")
	}
	var opt auth.OAuthOptions
	p.Apply(&opt, config.Remote{Type: "dropbox"})
	if !opt.PKCE {
		t.Error("the PKCE choice did not reach the authorization options")
	}
	if opt.AuthParams.Get("token_access_type") != "offline" {
		t.Error("the offline-access parameter did not reach the authorization options")
	}
}

// The profile table is the one place that says which backends a browser can
// drive. Enumerating the answer anywhere else — a second list, a comment — is
// how gdrive and box came to be supported for months while the package comment
// still named only three backends. This walks the table itself.
func TestEveryBackendWithAProfileCanBeDrivenFromABrowser(t *testing.T) {
	for _, typ := range []string{"aliyun", "baidu", "gdrive", "box", "dropbox"} {
		p, ok := OAuthProfileFor(typ)
		if !ok {
			t.Errorf("%s has no OAuth profile", typ)
			continue
		}
		if !SupportsDaemonAuth(typ) {
			t.Errorf("%s has a profile but reports that the daemon cannot authorize it", typ)
		}
		if !strings.HasPrefix(p.AuthorizeURL, "https://") || !strings.HasPrefix(p.TokenURL, "https://") {
			t.Errorf("%s endpoints = %q %q", typ, p.AuthorizeURL, p.TokenURL)
		}
		if p.Scope == "" {
			t.Errorf("%s requests no scope", typ)
		}
	}
}
