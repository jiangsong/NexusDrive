package pan123

import (
	"context"
	"net/http"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
)

const pathUserInfo = "/api/v1/user/info"

// Quota implements provider.Quotaer from the Open API user record. Temporary
// space is usable until it expires, so it is part of the current total.
func (p *Provider) Quota(ctx context.Context) (provider.Quota, error) {
	var out struct {
		Used      int64 `json:"spaceUsed"`
		Permanent int64 `json:"spacePermanent"`
		Temporary int64 `json:"spaceTemp"`
	}
	if err := p.call(ctx, "user/info", http.MethodGet, pathUserInfo, ratelimit.Meta, nil, nil, &out); err != nil {
		return provider.Quota{}, err
	}
	return provider.Quota{Total: out.Permanent + out.Temporary, Used: out.Used}, nil
}

var _ provider.Quotaer = (*Provider)(nil)
