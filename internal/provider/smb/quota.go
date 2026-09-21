package smb

import (
	"context"
	"fmt"
	"math"

	smb2 "github.com/hirochachacha/go-smb2"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
)

type spaceFileSystem interface {
	Statfs(name string) (smb2.FileFsInfo, error)
}

// Quota implements provider.Quotaer from SMB FileFsFullSizeInformation.
func (p *Provider) Quota(ctx context.Context) (provider.Quota, error) {
	var info smb2.FileFsInfo
	err := p.share.use(ctx, ratelimit.Meta, func(fs fileSystem) error {
		space, ok := fs.(spaceFileSystem)
		if !ok {
			return provider.ErrUnsupported
		}
		var err error
		info, err = space.Statfs(p.root)
		return err
	})
	if err != nil {
		return provider.Quota{}, mapErr(err)
	}
	unit, err := smbQuotaBytes(info.BlockSize(), info.FragmentSize())
	if err != nil {
		return provider.Quota{}, err
	}
	total, err := smbQuotaBytes(unit, info.TotalBlockCount())
	if err != nil {
		return provider.Quota{}, err
	}
	available, err := smbQuotaBytes(unit, info.AvailableBlockCount())
	if err != nil {
		return provider.Quota{}, err
	}
	if available > total {
		available = total
	}
	return provider.Quota{Total: int64(total), Used: int64(total - available)}, nil
}

func smbQuotaBytes(a, b uint64) (uint64, error) {
	if a != 0 && b > math.MaxUint64/a {
		return 0, fmt.Errorf("smb: filesystem capacity overflows uint64")
	}
	v := a * b
	if v > math.MaxInt64 {
		return 0, fmt.Errorf("smb: filesystem capacity exceeds int64")
	}
	return v, nil
}

var _ provider.Quotaer = (*Provider)(nil)
