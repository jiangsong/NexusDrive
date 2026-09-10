package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"cloudfs/internal/auth"
	"cloudfs/internal/config"
)

// The authorization flows a running daemon can drive on a caller's behalf,
// lifted out of the CLI so the CLI and the control API share one
// implementation. The point of driving them from the daemon is the credential
// boundary: the daemon opens the authorization URL (or hands the caller a URL
// or a QR string to present), receives the callback itself, exchanges the
// code itself, and saves the credential itself through config.SaveCredentials.
// A secret value never crosses the process boundary to whoever started the
// flow — which is what lets the browser drive an OAuth login without the token
// ever passing through it.
//
// Only the flows where the daemon can obtain the credential on the user's
// behalf are here: every backend with an entry in OAuthProfileFor, plus
// pan115's device/QR exchange. Naming them in prose instead went stale twice —
// gdrive, box and dropbox were each added to the table while this comment
// still listed three backends — so the table below is the answer, and
// SupportsDaemonAuth derives from it rather than repeating it. The providers
// whose credential is a password, a cookie or an externally issued token have
// no entry: there is nothing for the daemon to fetch, and those stay
// `cloudfs config auth --stdin`.

// OAuthPresentation is what a caller must show the user to complete an OAuth
// login: the URL to open, and the callback the app must be registered with.
type OAuthPresentation struct {
	AuthorizeURL string
	RedirectURI  string
}

// SupportsDaemonAuth reports whether StartOAuthFlow or StartDevice115Flow can
// drive this remote's authorization. Everything else is terminal-only import.
func SupportsDaemonAuth(remoteType string) bool {
	if remoteType == "pan115" {
		return true
	}
	_, ok := OAuthProfileFor(remoteType)
	return ok
}

// OAuthProfile is everything about a backend's authorization server that does
// not depend on the account: the two endpoints, the scope to ask for, the
// shape of the token exchange, and any extra authorization parameters. It is
// the one table both the CLI and the control API read, so a browser login and
// a terminal login cannot drift apart.
type OAuthProfile struct {
	AuthorizeURL string
	TokenURL     string
	Scope        string
	// JSONToken and FormToken select the exchange the server accepts; a
	// profile setting neither uses the query exchange.
	JSONToken bool
	FormToken bool
	// PKCE authorizes as a public client (RFC 7636): the code is bound to a
	// per-attempt verifier instead of a static client secret, so no secret has
	// to exist for the exchange to be safe against interception. It is what
	// lets a shipped desktop binary hold a client registration at all — a
	// secret compiled into a binary anyone can download is not a secret.
	PKCE       bool
	AuthParams url.Values
}

// OAuthProfileFor returns the profile for a remote type, if it has one. A
// backend whose credential is a password, a cookie or a device code has none.
func OAuthProfileFor(remoteType string) (OAuthProfile, bool) {
	switch remoteType {
	case "aliyun":
		return OAuthProfile{
			AuthorizeURL: "https://openapi.alipan.com/oauth/authorize",
			TokenURL:     "https://openapi.alipan.com/oauth/access_token",
			Scope:        "user:base,file:all:read,file:all:write",
			JSONToken:    true,
			AuthParams:   url.Values{"style": {"folder"}},
		}, true
	case "baidu":
		return OAuthProfile{
			AuthorizeURL: "https://openapi.baidu.com/oauth/2.0/authorize",
			TokenURL:     "https://openapi.baidu.com/oauth/2.0/token",
			Scope:        "basic netdisk",
		}, true
	case "gdrive":
		return OAuthProfile{
			AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
			TokenURL:     "https://oauth2.googleapis.com/token",
			Scope:        "https://www.googleapis.com/auth/drive",
			FormToken:    true,
			// Google returns a refresh token only for an offline grant, and
			// only re-issues one when consent is asked for again. An account
			// authorized without both stops working after an hour.
			AuthParams: url.Values{"access_type": {"offline"}, "prompt": {"consent"}},
		}, true
	case "dropbox":
		return OAuthProfile{
			AuthorizeURL: "https://www.dropbox.com/oauth2/authorize",
			TokenURL:     "https://api.dropboxapi.com/oauth2/token",
			// The scopes the driver actually calls, and no more: metadata and
			// content both ways, plus the account read that reports the space.
			// A scope the app was never granted in the App Console fails the
			// whole authorization, so this list has to match reality rather
			// than ask for everything.
			Scope:     "account_info.read files.metadata.read files.metadata.write files.content.read files.content.write",
			FormToken: true,
			// Dropbox issues no client secret to a public client, and none is
			// needed: the verifier proves the exchange comes from whoever
			// started the authorization.
			PKCE: true,
			// Without an offline grant the exchange returns an access token
			// that expires in hours. Every Dropbox account added before this
			// profile existed died that way, because the fallback asked for
			// exactly that token by hand.
			AuthParams: url.Values{"token_access_type": {"offline"}},
		}, true
	case "box":
		return OAuthProfile{
			AuthorizeURL: "https://account.box.com/api/oauth2/authorize",
			TokenURL:     "https://api.box.com/oauth2/token",
			Scope:        "root_readwrite",
			FormToken:    true,
		}, true
	}
	return OAuthProfile{}, false
}

// Apply writes the profile into the options a caller is assembling, then lets
// the account's own configuration override the endpoints and the scope. A
// self-hosted or region-specific authorization server is configured per
// account; the exchange shape is a property of the protocol, not of the host.
func (p OAuthProfile) Apply(o *auth.OAuthOptions, r config.Remote) {
	o.AuthorizeURL, o.TokenURL, o.Scope = p.AuthorizeURL, p.TokenURL, p.Scope
	o.JSONToken, o.FormToken, o.AuthParams = p.JSONToken, p.FormToken, p.AuthParams
	o.PKCE = p.PKCE
	override := func(key string, dst *string) {
		if v := remoteField(r, key); v != "" {
			*dst = v
		}
	}
	override("oauth_authorize_url", &o.AuthorizeURL)
	override("oauth_token_url", &o.TokenURL)
	override("oauth_scope", &o.Scope)
}

func remoteField(r config.Remote, key string) string {
	v, _ := r.Extra[key].(string)
	return v
}

// oauthOptions assembles the OAuth parameters for a remote, resolving the
// client secret from storage. present receives the authorization URL once the
// callback socket is listening and must return without waiting.
func oauthOptions(cfg *config.Config, name string, r config.Remote, redirectURI string, present func(context.Context, string) error) (auth.OAuthOptions, func(), error) {
	// Whether a secret is required at all is a property of the profile, so it
	// is read first. A public client (PKCE) has none: the per-attempt verifier
	// is what proves the exchange, and demanding a secret would make the flow
	// unusable for a backend that never issues one.
	profile, ok := OAuthProfileFor(r.Type)
	if !ok {
		return auth.OAuthOptions{}, nil, fmt.Errorf("account: %q does not use daemon-driven OAuth", r.Type)
	}
	// A browser cannot be prompted, so this path passes no prompt: an account
	// that needs a secret it does not have is a refusal here, not a question.
	clientID, secret, err := ResolveOAuthClient(cfg, r, profile, nil)
	if err != nil {
		return auth.OAuthOptions{}, nil, err
	}
	client, closeHTTP, err := AuthorizationHTTP(cfg, name)
	if err != nil {
		return auth.OAuthOptions{}, nil, err
	}
	if redirectURI == "" {
		redirectURI = remoteField(r, "oauth_redirect_uri")
	}
	if redirectURI == "" {
		redirectURI = "http://127.0.0.1:53682/callback"
	}
	o := auth.OAuthOptions{Client: client, ClientID: clientID, ClientSecret: secret, RedirectURI: redirectURI, RequireRefresh: true, OpenURL: present}
	profile.Apply(&o, r)
	return o, closeHTTP, nil
}

// StartOAuthFlow begins an OAuth login for a configured remote. present is
// called once, with the authorization URL, after the callback listener is up;
// it must not block on authorization. wait blocks until the exchange completes
// or ctx ends, then persists the credential and returns the presentation the
// caller already showed. Nothing about the token is returned.
func StartOAuthFlow(ctx context.Context, cfg *config.Config, name string, redirectURI string, present func(ctx context.Context, url string) error) (OAuthPresentation, func(context.Context) error, error) {
	r, ok := cfg.Remotes[name]
	if !ok {
		return OAuthPresentation{}, nil, fmt.Errorf("account: unknown remote %q", name)
	}
	if !SupportsDaemonAuth(r.Type) || r.Type == "pan115" {
		return OAuthPresentation{}, nil, fmt.Errorf("account: %q is not an OAuth remote", r.Type)
	}
	shown := make(chan string, 1)
	o, closeHTTP, err := oauthOptions(cfg, name, r, redirectURI, func(ctx context.Context, address string) error {
		select {
		case shown <- address:
		default:
		}
		return present(ctx, address)
	})
	if err != nil {
		return OAuthPresentation{}, nil, err
	}
	result := make(chan error, 1)
	go func() {
		token, err := auth.Authorize(ctx, o)
		if err != nil {
			result <- err
			return
		}
		// The daemon saves the credential; it is never handed back to the
		// caller that started the flow.
		// A public client has no secret to store, and saveCredentials refuses
		// an empty value: sending the key unconditionally would fail the save
		// after the authorization already succeeded, blaming a field the
		// person never supplied.
		saved := map[string]string{"refresh_token": token.RefreshToken}
		if o.ClientSecret != "" {
			saved["client_secret"] = o.ClientSecret
		}
		_, saveErr := config.SaveCredentialsForRemote(cfg.SourcePath, name, saved, r)
		result <- saveErr
	}()
	// Authorize sends the URL to present before it blocks; wait for it so the
	// caller has something to show even though authorization is still pending.
	var url string
	select {
	case url = <-shown:
	case err := <-result:
		closeHTTP()
		if err != nil {
			return OAuthPresentation{}, nil, err
		}
		return OAuthPresentation{}, nil, errors.New("account: authorization finished before presenting a URL")
	case <-ctx.Done():
		closeHTTP()
		return OAuthPresentation{}, nil, ctx.Err()
	}
	wait := func(waitCtx context.Context) error {
		defer closeHTTP()
		select {
		case err := <-result:
			return err
		case <-waitCtx.Done():
			return waitCtx.Err()
		}
	}
	return OAuthPresentation{AuthorizeURL: url, RedirectURI: o.RedirectURI}, wait, nil
}

// StartDevice115Flow begins 115's device/QR login. present receives the QR
// content string (not an image) once, so the caller renders the code itself.
// scanned, if non-nil, fires when 115 reports the code was scanned. wait
// blocks until confirmation, saves the credential and returns.
func StartDevice115Flow(ctx context.Context, cfg *config.Config, name string, present func(ctx context.Context, qrContent string) error, scanned func()) (func(context.Context) error, error) {
	r, ok := cfg.Remotes[name]
	if !ok {
		return nil, fmt.Errorf("account: unknown remote %q", name)
	}
	if r.Type != "pan115" {
		return nil, fmt.Errorf("account: %q does not use the 115 device flow", r.Type)
	}
	client, closeHTTP, err := AuthorizationHTTP(cfg, name)
	if err != nil {
		return nil, err
	}
	opt := auth.Device115Options{
		Client: client, ClientID: remoteField(r, "client_id"),
		DeviceURL: remoteField(r, "oauth_device_url"), PollURL: remoteField(r, "oauth_poll_url"), TokenURL: remoteField(r, "oauth_token_url"),
		Show: func(ctx context.Context, content string) error { return present(ctx, content) },
	}
	if scanned != nil {
		opt.Scanned = scanned
	}
	result := make(chan error, 1)
	go func() {
		token, err := auth.Authorize115(ctx, opt)
		if err != nil {
			result <- err
			return
		}
		_, saveErr := config.SaveCredentialsForRemote(cfg.SourcePath, name, map[string]string{"refresh_token": token.RefreshToken}, r)
		result <- saveErr
	}()
	wait := func(waitCtx context.Context) error {
		defer closeHTTP()
		select {
		case err := <-result:
			return err
		case <-waitCtx.Done():
			return waitCtx.Err()
		}
	}
	return wait, nil
}
