package baidu

import (
	"context"
	"net/url"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
)

const pathQuota = "/api/quota"

// Quota implements provider.Quotaer through Baidu's account quota endpoint.
func (p *Provider) Quota(ctx context.Context) (provider.Quota, error) {
	q := url.Values{}
	q.Set("checkfree", "1")
	q.Set("checkexpire", "1")
	var out struct {
		baseResp
		Total int64 `json:"total"`
		Used  int64 `json:"used"`
	}
	if err := p.get(ctx, "quota", p.base, pathQuota, q, ratelimit.Meta, &out); err != nil {
		return provider.Quota{}, err
	}
	return provider.Quota{Total: out.Total, Used: out.Used}, nil
}

var _ provider.Quotaer = (*Provider)(nil)
