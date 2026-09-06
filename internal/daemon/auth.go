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
// behalf are here: aliyun and baidu (OAuth) and pan115 (device/QR). The
// providers whose credential is a password, a cookie or an externally issued
// token are not — there is nothing for the daemon to fetch, and those stay
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
	switch remoteType {
	case "aliyun", "baidu", "pan115":
		return true
	}
	return false
}

func remoteField(r config.Remote, key string) string {
	v, _ := r.Extra[key].(string)
	return v
}

// oauthOptions assembles the OAuth parameters for a remote, resolving the
// client secret from storage. present receives the authorization URL once the
// callback socket is listening and must return without waiting.
func oauthOptions(cfg *config.Config, name string, r config.Remote, redirectURI string, present func(context.Context, string) error) (auth.OAuthOptions, func(), error) {
	if remoteField(r, "client_id") == "" {
		return auth.OAuthOptions{}, nil, errors.New("client_id is required; set it in the account configuration first")
	}
	secret := remoteField(r, "client_secret")
	if config.IsSecretReference(secret) {
		resolved, err := config.NewSecretStore(cfg).Get(secret)
		if err != nil {
			return auth.OAuthOptions{}, nil, err
		}
		secret = resolved
	}
	if secret == "" {
		return auth.OAuthOptions{}, nil, errors.New("client_secret is required for this authorization mode; import it first with `cloudfs config auth --stdin`")
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
	o := auth.OAuthOptions{Client: client, ClientID: remoteField(r, "client_id"), ClientSecret: secret, RedirectURI: redirectURI, RequireRefresh: true, OpenURL: present}
	switch r.Type {
	case "aliyun":
		o.AuthorizeURL = "https://openapi.alipan.com/oauth/authorize"
		o.TokenURL = "https://openapi.alipan.com/oauth/access_token"
		o.Scope = "user:base,file:all:read,file:all:write"
		o.JSONToken = true
		o.AuthParams = url.Values{"style": {"folder"}}
	case "baidu":
		o.AuthorizeURL = "https://openapi.baidu.com/oauth/2.0/authorize"
		o.TokenURL = "https://openapi.baidu.com/oauth/2.0/token"
		o.Scope = "basic netdisk"
	default:
		closeHTTP()
		return auth.OAuthOptions{}, nil, fmt.Errorf("account: %q does not use daemon-driven OAuth", r.Type)
	}
	for key, dst := range map[string]*string{"oauth_authorize_url": &o.AuthorizeURL, "oauth_token_url": &o.TokenURL, "oauth_scope": &o.Scope} {
		if v := remoteField(r, key); v != "" {
			*dst = v
		}
	}
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
		_, saveErr := config.SaveCredentialsForRemote(cfg.SourcePath, name, map[string]string{
			"refresh_token": token.RefreshToken, "client_secret": o.ClientSecret,
		}, r)
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
