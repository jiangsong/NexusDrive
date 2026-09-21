package sftp

import (
	"context"
	"fmt"
	"math"

	psftp "github.com/pkg/sftp"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
)

// Quota implements provider.Quotaer through OpenSSH's statvfs extension.
// Servers that do not advertise the extension return an error, allowing the
// pool to fall back to a configured member capacity.
func (p *Provider) Quota(ctx context.Context) (provider.Quota, error) {
	root, err := p.resolveRoot(ctx)
	if err != nil {
		return provider.Quota{}, err
	}
	var stat *psftp.StatVFS
	if err := p.do(ctx, ratelimit.Meta, func(c *psftp.Client) error {
		var err error
		stat, err = c.StatVFS(root)
		return err
	}); err != nil {
		return provider.Quota{}, mapErr(err)
	}
	unit := stat.Frsize
	if unit == 0 {
		unit = stat.Bsize
	}
	total, err := quotaBytes(unit, stat.Blocks)
	if err != nil {
		return provider.Quota{}, err
	}
	available, err := quotaBytes(unit, stat.Bavail)
	if err != nil {
		return provider.Quota{}, err
	}
	if available > total {
		available = total
	}
	return provider.Quota{Total: total, Used: total - available}, nil
}

func quotaBytes(unit, blocks uint64) (int64, error) {
	if unit != 0 && blocks > uint64(math.MaxInt64)/unit {
		return 0, fmt.Errorf("sftp: filesystem capacity exceeds int64")
	}
	return int64(unit * blocks), nil
}

var _ provider.Quotaer = (*Provider)(nil)
