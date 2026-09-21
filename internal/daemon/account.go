package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// OpenAccount builds one provider without a VFS, cache, journal or mount.
// Authorization checks can therefore run before the first mount is configured.
func OpenAccount(cfg *config.Config, name string) (provider.Provider, func() error, error) {
	rc, ok := cfg.Remotes[name]
	if !ok {
		return nil, nil, fmt.Errorf("account: unknown remote %q", name)
	}
	resolved, err := config.NewSecretStore(cfg).ResolveRemote(rc)
	if err != nil {
		return nil, nil, err
	}
	pm, err := buildProxy(cfg)
	if err != nil {
		return nil, nil, err
	}
	var p provider.Provider
	reg := buildLimiters(cfg, func(string) (provider.Caps, bool) {
		if p == nil {
			return provider.Caps{}, false
		}
		return p.Capabilities(), true
	})
	p, effectiveConns, closeProvider, err := buildProvider(name, resolved, pm, reg)
	if err != nil {
		pm.Stop()
		return nil, nil, err
	}
	if setter, ok := p.(provider.TokenPersistenceSetter); ok && cfg.SourcePath != "" {
		setter.SetTokenPersister(config.TokenPersister(cfg, name))
	}
	// Same ordering and rule as daemon.Open: the caps override must use the
	// same effectiveConns buildProvider already applied to the transport,
	// and it goes on after the TokenPersistenceSetter check since its
	// wrapper does not forward that interface.
	p = provider.WithMaxConns(p, effectiveConns)
	return p, func() error { defer pm.Stop(); return closeProvider() }, nil
}

// CheckAccount performs one root listing, not a recursive scan or upload. A
// backend may opt in to creating its configured root when that listing says it
// is absent; the new root is listed again before the check succeeds. Rotated
// tokens are persisted even if a later listing request fails.
func CheckAccount(ctx context.Context, cfg *config.Config, name string) error {
	p, closeAccount, err := OpenAccount(cfg, name)
	if err != nil {
		return err
	}
	defer closeAccount()
	return checkAccountProvider(ctx, p)
}

// rootEnsurer is deliberately opt-in: an opaque root id or a path owned by a
// server must never be treated as a directory CloudFS is free to create.
type rootEnsurer interface {
	EnsureRoot(context.Context) error
}

func checkAccountProvider(ctx context.Context, p provider.Provider) error {
	root := providerRoot(p)
	_, _, err := p.List(ctx, root, "")
	if err == nil || !errors.Is(err, provider.ErrNotFound) {
		return err
	}
	ensurer, ok := provider.Unwrap(p).(rootEnsurer)
	if !ok {
		return err
	}
	if err := ensurer.EnsureRoot(ctx); err != nil {
		return err
	}
	_, _, err = p.List(ctx, root, "")
	return err
}

// AccountQuota reports one account's own space. The second return says
// whether the backend could answer at all: gdrive publishes a figure, dropbox
// now does too, box does not, and "cannot say" is a different fact from "zero
// bytes free". A pool needs the distinction — a member whose space is unknown
// is placed after every member whose space is known — and a person adding a
// drive deserves to be told which of the two they just added.
func AccountQuota(ctx context.Context, cfg *config.Config, name string) (provider.Quota, bool, error) {
	p, closeAccount, err := OpenAccount(cfg, name)
	if err != nil {
		return provider.Quota{}, false, err
	}
	defer closeAccount()
	q, supported, err := provider.QuotaOf(ctx, p)
	if !supported || err != nil {
		// Not being able to say is not an error worth surfacing: the account
		// works, it just does not publish a figure.
		return provider.Quota{}, false, nil
	}
	return q, q.Total > 0, nil
}

// providerRoot is the id or path a provider lists its root from; both the
// account check and the mount builder resolve it the same way, so a remote
// that checks out is one that mounts.
func providerRoot(p provider.Provider) string { return provider.RootOf(p) }

// SanitizeAccountError reduces a provider error to its kind. Provider errors
// can embed signed URLs, cookies and OAuth query strings, so nothing that
// reports an account check — the CLI, the control API — prints the original.
func SanitizeAccountError(err error) error {
	if err == nil {
		return nil
	}
	kind := "connection/provider error"
	switch {
	case errors.Is(err, provider.ErrAuth):
		kind = "authentication rejected"
	case errors.Is(err, provider.ErrRateLimited):
		kind = "rate limited"
	case errors.Is(err, provider.ErrRiskControl):
		kind = "risk control; pause account activity"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return fmt.Errorf("account check failed (%s); verify credentials, proxy and root settings", kind)
}

// AuthorizationHTTP applies existing proxy rules and metadata rate limits.
// Token/code exchanges are single-use, so neither HTTP redirects nor retries
// may replay them. OAuth callers redact transport bodies before displaying errors.
func AuthorizationHTTP(cfg *config.Config, name string) (*httpx.Client, func(), error) {
	rc, ok := cfg.Remotes[name]
	if !ok {
		return nil, nil, fmt.Errorf("account: unknown remote %q", name)
	}
	pm, err := buildProxy(cfg)
	if err != nil {
		return nil, nil, err
	}
	h := pm.Client(rc.Proxy, 30*time.Second)
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	reg := buildLimiters(cfg, nil)
	client := httpx.New(httpx.Options{HTTP: h, Remote: name, Limiters: reg, Policy: retry.Policy{MaxAttempts: 1}})
	return client, func() { h.CloseIdleConnections(); pm.Stop() }, nil
}
