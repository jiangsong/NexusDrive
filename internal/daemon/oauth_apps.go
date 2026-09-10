package daemon

import (
	"errors"

	"cloudfs/internal/config"
)

// Built-in OAuth applications.
//
// A cloud drive will not let a program hold an account's password, so adding
// one means a browser authorization, and a browser authorization needs an
// application registered with the provider. Requiring every person to go and
// create one — a Google Cloud project, a Dropbox app — before they can add
// their first drive is the single biggest thing standing between "I have four
// accounts" and "I have one folder". A registration shipped with the binary
// removes that step for the people who have no reason to want their own.
//
// What is honest about doing this:
//
//   - A client secret compiled into a binary anyone can download is not a
//     secret. It does not expose anyone's files — tokens stay per-account and
//     per-consent — but it does let a phishing page put this application's
//     name on a real consent screen. Both vendors design for that: Google's
//     "desktop app" client type and Dropbox's PKCE guidance both assume the
//     value is public. Where the profile is PKCE there is no secret at all,
//     which is the strongest argument for preferring it.
//   - The quota is metered per application, so everyone using the built-in
//     draws on one bucket. Rate limiting here is per account, not per
//     application, so nothing in this process defends that bucket. Bringing
//     your own application stays a first-class option, not an expert path.
//   - If a built-in registration is ever revoked, every account authorized
//     under it stops refreshing. Because the resolved client id is written
//     into the account when it is created, those accounts are identifiable
//     from `cloudfs config list` and fail as a plain authorization error.
//
// The table ships empty. Registering the applications is a maintainer's job
// and cannot be done in code: create them with the providers, register the
// callback `http://127.0.0.1:53682/callback`, take a Dropbox app out of
// Development status (it is capped at 50 linked users until then), and for
// Google decide whether the restricted `drive` scope is worth OAuth brand
// verification plus the annual third-party security assessment it requires.
// Until an entry has a client id, every path behaves exactly as it did before.
var builtinOAuthApps = map[string]BuiltinOAuthApp{
	// "dropbox": {ClientID: "..."},                              // PKCE: no secret
	// "gdrive":  {ClientID: "...", ClientSecret: "..."},          // see the note about verification
}

// BuiltinOAuthApp is one registration shipped with the binary.
type BuiltinOAuthApp struct {
	ClientID     string
	ClientSecret string
}

// BuiltinOAuthAppFor returns the shipped registration for a backend. An entry
// with no client id is not a registration: reporting it as one would authorize
// against an application called "".
func BuiltinOAuthAppFor(remoteType string) (BuiltinOAuthApp, bool) {
	app, ok := builtinOAuthApps[remoteType]
	if !ok || app.ClientID == "" {
		return BuiltinOAuthApp{}, false
	}
	return app, true
}

// ResolveOAuthClient decides which application an authorization runs as.
//
// The account's own client id wins outright: whoever registered one did it for
// a reason, and a default that displaced it would move their authorizations
// onto our quota and our consent screen without saying so. Only when the
// account names none does the built-in apply, and then its id and secret are
// taken together — half of one registration paired with half of another is
// meaningless to the authorization server and leaks our secret into an
// exchange it does not belong to.
//
// promptSecret is how the terminal asks for a secret it does not have; the
// daemon passes nil, because a browser cannot be prompted. Neither is called
// for a public client, which has no secret to give.
func ResolveOAuthClient(cfg *config.Config, r config.Remote, profile OAuthProfile, promptSecret func(label string) (string, error)) (string, string, error) {
	id := remoteField(r, "client_id")
	if id == "" {
		app, ok := BuiltinOAuthAppFor(r.Type)
		if !ok {
			return "", "", errors.New("client_id is required; set it in the account configuration first")
		}
		if profile.PKCE {
			// A public client has no secret. Sending one the table happens to
			// hold would put it in an exchange that does not want it, and then
			// into the account's saved credentials.
			return app.ClientID, "", nil
		}
		return app.ClientID, app.ClientSecret, nil
	}

	secret := remoteField(r, "client_secret")
	if config.IsSecretReference(secret) && cfg != nil {
		resolved, err := config.NewSecretStore(cfg).Get(secret)
		if err != nil {
			return "", "", err
		}
		secret = resolved
	}
	if secret == "" && !profile.PKCE && promptSecret != nil {
		asked, err := promptSecret("client_secret")
		if err != nil {
			return "", "", err
		}
		secret = asked
	}
	if secret == "" && !profile.PKCE {
		return "", "", errors.New("client_secret is required for this authorization mode; import it first with `cloudfs config auth --stdin`")
	}
	return id, secret, nil
}

// FillBuiltinClientID writes the shipped application's client id into a new
// account that names none, so the account's identity is complete in the file
// from the moment it is created.
//
// It matters because config.EffectiveAccountBinding hashes the account's
// non-secret fields, and client_id is one of them. Resolving the built-in only
// at authorization time would leave the file blank while the authorization ran
// against a real application — and writing it in later would move the binding
// and fence uploads that were already queued. An account that supplied its own
// client id keeps it; with nothing registered, nothing is written, because an
// empty client_id means "not configured yet", not an application named "".
func FillBuiltinClientID(r *config.Remote) {
	if r == nil || remoteField(*r, "client_id") != "" {
		return
	}
	app, ok := BuiltinOAuthAppFor(r.Type)
	if !ok {
		return
	}
	if r.Extra == nil {
		r.Extra = map[string]any{}
	}
	r.Extra["client_id"] = app.ClientID
}
