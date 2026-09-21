package sftp

import (
	"context"
	"testing"

	"cloudfs/internal/provider"
)

func TestQuota(t *testing.T) {
	p, _ := newTestProvider(t)
	q, ok, err := provider.QuotaOf(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || q.Total <= 0 || q.Used < 0 || q.Used > q.Total {
		t.Fatalf("quota = %+v, supported=%v", q, ok)
	}
}
