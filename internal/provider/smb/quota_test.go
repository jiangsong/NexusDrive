package smb

import (
	"context"
	"testing"

	"cloudfs/internal/provider"
)

type fakeFsInfo struct {
	blockSize, fragmentSize, total, free, available uint64
}

func (f fakeFsInfo) BlockSize() uint64           { return f.blockSize }
func (f fakeFsInfo) FragmentSize() uint64        { return f.fragmentSize }
func (f fakeFsInfo) TotalBlockCount() uint64     { return f.total }
func (f fakeFsInfo) FreeBlockCount() uint64      { return f.free }
func (f fakeFsInfo) AvailableBlockCount() uint64 { return f.available }

func TestQuota(t *testing.T) {
	share := newMemShare()
	share.fsInfo = fakeFsInfo{blockSize: 512, fragmentSize: 8, total: 1_000_000, free: 300_000, available: 250_000}
	p := newTestProvider(t, share)

	q, ok, err := provider.QuotaOf(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || q.Total != 4_096_000_000 || q.Used != 3_072_000_000 {
		t.Fatalf("quota = %+v, supported=%v", q, ok)
	}
}
