package daemon

import (
	"context"
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
	p, closeProvider, err := buildProvider(name, resolved, pm, reg)
	if err != nil {
		pm.Stop()
		return nil, nil, err
	}
	if setter, ok := p.(provider.TokenPersistenceSetter); ok && cfg.SourcePath != "" {
		setter.SetTokenPersister(config.TokenPersister(cfg, name))
	}
	return p, func() error { defer pm.Stop(); return closeProvider() }, nil
}

// CheckAccount performs one root listing, not a recursive scan or upload.
// Rotated tokens are persisted even if a later listing request fails.
func CheckAccount(ctx context.Context, cfg *config.Config, name string) error {
	p, closeAccount, err := OpenAccount(cfg, name)
	if err != nil {
		return err
	}
	defer closeAccount()
	root := "/"
	switch p := p.(type) {
	case interface{ RootID() string }:
		root = p.RootID()
	case interface{ RootFileID() string }:
		root = p.RootFileID()
	case interface{ RootPath() string }:
		root = p.RootPath()
	}
	_, _, err = p.List(ctx, root, "")
	return err
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
