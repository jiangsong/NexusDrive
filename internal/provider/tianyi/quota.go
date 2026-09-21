package tianyi

import (
	"context"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
)

// Quota implements provider.Quotaer through the signed PC capacity endpoint.
func (t *Tianyi) Quota(ctx context.Context) (provider.Quota, error) {
	var out struct {
		envelope
		Cloud struct {
			Total int64 `json:"totalSize"`
			Used  int64 `json:"usedSize"`
		} `json:"cloudCapacityInfo"`
	}
	if err := t.getJSON(ctx, t.base+"/portal/getUserSizeInfo.action", ratelimit.Meta, &out); err != nil {
		return provider.Quota{}, err
	}
	return provider.Quota{Total: out.Cloud.Total, Used: out.Cloud.Used}, nil
}

var _ provider.Quotaer = (*Tianyi)(nil)
