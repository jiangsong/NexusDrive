package pan115

import (
	"context"
	"net/http"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// Quota implements provider.Quotaer from the Open platform's current-user
// record. 115 wraps each size in an object whose size value may be a JSON
// string or number, hence flexInt64.
func (p *Pan115) Quota(ctx context.Context) (provider.Quota, error) {
	type sizeValue struct {
		Size flexInt64 `json:"size"`
	}
	var out struct {
		Space struct {
			Total sizeValue `json:"all_total"`
			Used  sizeValue `json:"all_use"`
		} `json:"rt_space_info"`
	}
	if _, err := p.call(ctx, httpx.Request{
		Method: http.MethodGet,
		URL:    p.baseURL + "/open/user/info",
		Class:  ratelimit.Meta,
	}, &out); err != nil {
		return provider.Quota{}, err
	}
	return provider.Quota{Total: int64(out.Space.Total.Size), Used: int64(out.Space.Used.Size)}, nil
}

var _ provider.Quotaer = (*Pan115)(nil)
