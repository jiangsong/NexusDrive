package onedrive

import (
	"context"
	"net/http"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
)

// Quota implements provider.Quotaer from the drive resource's quota facet.
func (p *Provider) Quota(ctx context.Context) (provider.Quota, error) {
	var out struct {
		Quota struct {
			Total int64 `json:"total"`
			Used  int64 `json:"used"`
		} `json:"quota"`
	}
	if err := p.graphJSON(ctx, http.MethodGet, p.graphBase+p.drivePath+"?$select=quota", nil, &out, ratelimit.Meta, true); err != nil {
		return provider.Quota{}, err
	}
	return provider.Quota{Total: out.Quota.Total, Used: out.Quota.Used}, nil
}

var _ provider.Quotaer = (*Provider)(nil)
