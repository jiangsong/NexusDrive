package onedrive

import (
	"context"
	"net/http/httptest"
	"testing"

	"cloudfs/internal/provider"
)

func TestQuota(t *testing.T) {
	fake := newFakeGraph()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	p := newODProvider(t, fake, srv)

	q, ok, err := provider.QuotaOf(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || q.Total != 5<<30 || q.Used != 2<<30 {
		t.Fatalf("quota = %+v, supported=%v", q, ok)
	}
}
