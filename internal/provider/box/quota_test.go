package box

import (
	"context"
	"testing"

	"cloudfs/internal/provider"
)

func TestQuota(t *testing.T) {
	p := newTestProvider(t, newFakeBox())
	q, ok, err := provider.QuotaOf(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || q.Total != 10<<30 || q.Used != 3<<30 {
		t.Fatalf("quota = %+v, supported=%v", q, ok)
	}
}
