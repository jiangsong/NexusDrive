package daemon

import (
	"strings"
	"testing"

	"cloudfs/internal/config"
)

// withBuiltinApp installs a built-in registration for the duration of one test.
// The shipped table is empty until a maintainer registers real applications, so
// every test here supplies its own rather than depending on what is in it.
func withBuiltinApp(t *testing.T, remoteType string, app BuiltinOAuthApp) {
	t.Helper()
	previous, had := builtinOAuthApps[remoteType]
	builtinOAuthApps[remoteType] = app
	t.Cleanup(func() {
		if had {
			builtinOAuthApps[remoteType] = previous
			return
		}
		delete(builtinOAuthApps, remoteType)
	})
}

// withoutBuiltinApp is the mirror: it takes the registration away for the
// duration of one test.
//
// The tests that describe the unregistered case used to skip when the table
// held an entry, which was harmless only while the table was empty — the day a
// maintainer fills it in, those tests stop running and nobody is told. Removing
// the entry instead means they describe the same behaviour for ever: an account
// naming no application, with none registered, must fail exactly as it does
// today.
func withoutBuiltinApp(t *testing.T, remoteType string) {
	t.Helper()
	previous, had := builtinOAuthApps[remoteType]
	delete(builtinOAuthApps, remoteType)
	t.Cleanup(func() {
		if had {
			builtinOAuthApps[remoteType] = previous
		}
	})
}

// Someone who registered their own application did so for a reason — their own
// quota, their own consent screen, their own audit trail. A built-in default
// exists for the person who has none, and must never quietly displace a choice
// that was already made.
func TestAnAccountsOwnClientIDWinsOverTheBuiltInApplication(t *testing.T) {
	withBuiltinApp(t, "dropbox", BuiltinOAuthApp{ClientID: "builtin-key"})
	profile, _ := OAuthProfileFor("dropbox")
	r := config.Remote{Type: "dropbox", Extra: map[string]any{"client_id": "my-own-key"}}

	id, _, err := ResolveOAuthClient(nil, r, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id != "my-own-key" {
		t.Fatalf("client id = %q, want the account's own", id)
	}
}

// The whole point of a built-in registration: an account that names no
// application still authorizes, instead of failing with a demand that the
// person go and create one first.
func TestTheBuiltInApplicationIsUsedOnlyWhenTheAccountNamesNoClientID(t *testing.T) {
	withBuiltinApp(t, "dropbox", BuiltinOAuthApp{ClientID: "builtin-key"})
	profile, _ := OAuthProfileFor("dropbox")

	id, _, err := ResolveOAuthClient(nil, config.Remote{Type: "dropbox"}, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id != "builtin-key" {
		t.Fatalf("client id = %q, want the built-in application", id)
	}
}

// A client id and its secret are one registration. Pairing our secret with
// somebody else's id is meaningless to the authorization server and leaks our
// secret into an exchange for an application it does not belong to.
func TestABuiltInClientSecretIsNeverPairedWithTheUsersOwnClientID(t *testing.T) {
	withBuiltinApp(t, "gdrive", BuiltinOAuthApp{ClientID: "builtin-id", ClientSecret: "builtin-secret"})
	profile, _ := OAuthProfileFor("gdrive")
	r := config.Remote{Type: "gdrive", Extra: map[string]any{"client_id": "my-own-id"}}

	_, secret, err := ResolveOAuthClient(nil, r, profile, func(string) (string, error) { return "my-own-secret", nil })
	if err != nil {
		t.Fatal(err)
	}
	if secret == "builtin-secret" {
		t.Fatal("the built-in secret was paired with an application it does not belong to")
	}
	if secret != "my-own-secret" {
		t.Fatalf("client secret = %q, want the one supplied for the account's own application", secret)
	}
}

// The table ships empty: the code lands and is tested before any application
// exists, and filling it in is a maintainer's one-line edit. Until then every
// path must behave exactly as it did before, including the error text that
// tells someone what to do.
func TestAnUnregisteredBuiltInApplicationLeavesTodaysErrorInPlace(t *testing.T) {
	withoutBuiltinApp(t, "gdrive")
	profile, _ := OAuthProfileFor("gdrive")
	_, _, err := ResolveOAuthClient(nil, config.Remote{Type: "gdrive"}, profile, nil)
	if err == nil {
		t.Fatal("an account with no client id and no built-in application was accepted")
	}
	if !strings.Contains(err.Error(), "client_id is required") {
		t.Fatalf("error = %v, want the existing client_id refusal", err)
	}
}

// A registration with no client id is not a registration. An entry left blank
// in the table must read as absent rather than as an application called "".
func TestABlankTableEntryIsNotAnApplication(t *testing.T) {
	withBuiltinApp(t, "dropbox", BuiltinOAuthApp{})
	if _, ok := BuiltinOAuthAppFor("dropbox"); ok {
		t.Fatal("a blank entry was reported as a registered application")
	}
}

// A public client has no secret at all, so resolving one must not go looking
// for it — and must never prompt for a value the app console never issued.
func TestAPublicClientResolvesWithoutASecretAndWithoutPrompting(t *testing.T) {
	withBuiltinApp(t, "dropbox", BuiltinOAuthApp{ClientID: "builtin-key"})
	profile, _ := OAuthProfileFor("dropbox")

	id, secret, err := ResolveOAuthClient(nil, config.Remote{Type: "dropbox"}, profile,
		func(label string) (string, error) {
			t.Errorf("a public client was asked for %q", label)
			return "", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if id != "builtin-key" || secret != "" {
		t.Fatalf("resolved id=%q secret=%q, want the built-in id and no secret", id, secret)
	}
}

// The effective account binding hashes the account's non-secret fields, and
// client_id is one of them. Resolving a built-in application at authorization
// time and leaving the file blank would mean the account's identity depended on
// a value that is not in it — and filling it in later would move the binding,
// fencing uploads that were already in flight. Writing it when the account is
// created keeps the binding stable from birth.
func TestANewAccountRecordsTheBuiltInClientIDSoItsBindingIsStable(t *testing.T) {
	withBuiltinApp(t, "dropbox", BuiltinOAuthApp{ClientID: "builtin-key"})
	r := config.Remote{Type: "dropbox"}
	FillBuiltinClientID(&r)
	if got, _ := r.Extra["client_id"].(string); got != "builtin-key" {
		t.Fatalf("client_id = %q, want the built-in application written into the account", got)
	}
}

// Someone who supplied their own application must keep it, in the file as well
// as at authorization time.
func TestANewAccountKeepsAClientIDTheUserSupplied(t *testing.T) {
	withBuiltinApp(t, "dropbox", BuiltinOAuthApp{ClientID: "builtin-key"})
	r := config.Remote{Type: "dropbox", Extra: map[string]any{"client_id": "my-own-key"}}
	FillBuiltinClientID(&r)
	if got, _ := r.Extra["client_id"].(string); got != "my-own-key" {
		t.Fatalf("client_id = %q, want the one the account supplied", got)
	}
}

// With no registration to fall back on, nothing is written: an account with a
// blank client_id reads as "not configured yet", not as an application named
// the empty string.
func TestAnAccountGainsNoClientIDWhenNothingIsRegistered(t *testing.T) {
	withoutBuiltinApp(t, "gdrive")
	r := config.Remote{Type: "gdrive"}
	FillBuiltinClientID(&r)
	if _, ok := r.Extra["client_id"]; ok {
		t.Fatalf("a client_id was written with no application to name: %v", r.Extra)
	}
}

// A PKCE profile is a public client: the app console never issued a secret for
// it, and sending one is at best meaningless. The account-supplied path already
// knows this — it neither prompts for nor requires a secret under PKCE — but
// the built-in branch returned whatever the table held. The table ships empty,
// so nothing is wrong today; the day a maintainer fills the dropbox entry in
// with a secret, that secret would go out in a public-client token exchange and
// then be written into the account's credentials.
func TestABuiltInSecretIsNeverSentForAPublicClient(t *testing.T) {
	withBuiltinApp(t, "dropbox", BuiltinOAuthApp{ClientID: "builtin-key", ClientSecret: "should-never-be-sent"})
	profile, _ := OAuthProfileFor("dropbox")
	if !profile.PKCE {
		t.Fatal("the dropbox profile is no longer a public client; this test describes the PKCE case")
	}

	id, secret, err := ResolveOAuthClient(nil, config.Remote{Type: "dropbox"}, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id != "builtin-key" {
		t.Fatalf("client id = %q, want the built-in application", id)
	}
	if secret != "" {
		t.Fatalf("client secret = %q; a public client exchange must carry none", secret)
	}
}
