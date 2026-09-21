package box

import (
	"context"
	"net/http"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
)

// Quota implements provider.Quotaer from the authenticated Box user's space
// allocation. Both values are bytes.
func (p *Provider) Quota(ctx context.Context) (provider.Quota, error) {
	var out struct {
		Total int64 `json:"space_amount"`
		Used  int64 `json:"space_used"`
	}
	if err := p.apiJSON(ctx, http.MethodGet, p.apiBase+"/users/me?fields=space_amount%2Cspace_used", nil, &out, ratelimit.Meta, true); err != nil {
		return provider.Quota{}, err
	}
	return provider.Quota{Total: out.Total, Used: out.Used}, nil
}

var _ provider.Quotaer = (*Provider)(nil)
