package pan115

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// tokenSet is one OAuth token pair plus the moment the access token dies.
type tokenSet struct {
	access  string
	refresh string
	expiry  time.Time
}

// tokenStore holds the live token pair. It is small and separate so the
// refresh path can be locked without blocking unrelated driver state.
type tokenStore struct {
	mu sync.Mutex
	tokenSet
}

// refreshResponse is the payload of POST /open/refreshToken.
type refreshResponse struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresIn    flexInt64 `json:"expires_in"`
}

// tokenSkew refreshes a little before the server-stated expiry so a long
// upload does not die mid-part.
const tokenSkew = 5 * time.Minute

// token returns a usable access token, refreshing first when the cached one is
// missing or about to expire.
func (p *Pan115) token(ctx context.Context) (string, error) {
	p.tokens.mu.Lock()
	access := p.tokens.access
	expiry := p.tokens.expiry
	p.tokens.mu.Unlock()
	if err := p.FlushTokens(); err != nil {
		return "", fmt.Errorf("pan115: save credentials: %w", err)
	}
	if access != "" && (expiry.IsZero() || time.Now().Add(tokenSkew).Before(expiry)) {
		return access, nil
	}
	if err := p.refreshToken(ctx); err != nil {
		return "", err
	}
	p.tokens.mu.Lock()
	defer p.tokens.mu.Unlock()
	return p.tokens.access, nil
}

// refreshToken exchanges the refresh token for a new pair.
//
// 115 keeps only TWO valid refresh tokens per account per app: authorising a
// third client silently invalidates the oldest one, and the owner of that
// token then sees a plain "refresh_token invalid" with no other warning. That
// is why the driver stores the rotated refresh token immediately and why
// RefreshToken() exists — the caller is expected to persist the new value so a
// restart does not fall back to a token 115 has already retired.
func (p *Pan115) refreshToken(ctx context.Context) error {
	if err := p.FlushTokens(); err != nil {
		return fmt.Errorf("pan115: save credentials: %w", err)
	}
	p.tokens.mu.Lock()
	defer p.tokens.mu.Unlock()
	if p.tokens.refresh == "" {
		return fmt.Errorf("%w: pan115: no refresh_token configured", provider.ErrAuth)
	}
	form := map[string]string{"refresh_token": p.tokens.refresh}
	// UNVERIFIED: 115's PKCE apps refresh with refresh_token alone. Apps issued
	// a client secret may also have to send client_id/client_secret here; both
	// are forwarded when configured, which is harmless if ignored.
	if p.clientID != "" {
		form["client_id"] = p.clientID
	}
	if p.clientSecret != "" {
		form["client_secret"] = p.clientSecret
	}
	resp, err := p.http.Do(ctx, httpx.Request{
		Method: "POST",
		URL:    p.passportURL + "/open/refreshToken",
		Class:  ratelimit.Meta,
		Form:   form,
	})
	if err != nil {
		return fmt.Errorf("pan115: refresh token: %w", err)
	}
	var out refreshResponse
	if err := decodeEnvelope(resp.Bytes, &out); err != nil {
		return fmt.Errorf("pan115: refresh token: %w", err)
	}
	if out.AccessToken == "" {
		return fmt.Errorf("%w: pan115: refresh returned no access_token", provider.ErrAuth)
	}
	p.tokens.access = out.AccessToken
	if out.RefreshToken != "" {
		p.tokens.refresh = out.RefreshToken
	}
	if out.ExpiresIn > 0 {
		p.tokens.expiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	} else {
		p.tokens.expiry = time.Time{}
	}
	if err := p.SaveTokens(map[string]string{"refresh_token": out.RefreshToken}); err != nil {
		return fmt.Errorf("pan115: save refreshed credentials: %w", err)
	}
	return nil
}

// AccessToken returns the current access token, mainly for diagnostics.
func (p *Pan115) AccessToken() string {
	p.tokens.mu.Lock()
	defer p.tokens.mu.Unlock()
	return p.tokens.access
}

// RefreshToken returns the refresh token currently held. 115 rotates it on
// every refresh and only remembers two per account per app, so the caller must
// persist this value after any operation rather than re-reading the one from
// the config file.
func (p *Pan115) RefreshToken() string {
	p.tokens.mu.Lock()
	defer p.tokens.mu.Unlock()
	return p.tokens.refresh
}

// decodeEnvelope unwraps a 115 response that is not going through call()
// (the token endpoints, which take no Authorization header).
//
// UNVERIFIED: /open/refreshToken is documented as wrapping the new pair in
// `data`, but some deployments answer with the token fields at the top level.
// Both shapes are accepted so a layout change degrades to "still works"
// instead of "silently no token".
func decodeEnvelope(body []byte, out any) error {
	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return err
	}
	if !env.ok() {
		return mapError(env.errCode(), env.errMessage())
	}
	if out == nil {
		return nil
	}
	if len(env.Data) > 0 && string(env.Data) != "null" {
		return json.Unmarshal(env.Data, out)
	}
	return json.Unmarshal(body, out)
}
