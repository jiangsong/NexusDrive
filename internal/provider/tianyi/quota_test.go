package tianyi

import (
	"context"
	"net/http"
	"testing"

	"cloudfs/internal/provider"
)

func TestQuota(t *testing.T) {
	f := newFake189(t)
	f.setOverride("/portal/getUserSizeInfo.action", func(w http.ResponseWriter, r *http.Request) {
		if !f.signatureOK(r) {
			t.Fatal("capacity request was not signed")
		}
		writeJSON(w, `{"res_code":0,"cloudCapacityInfo":{"totalSize":1099511627776,"usedSize":274877906944,"freeSize":824633720832}}`)
	})
	p := newDriver(t, f, Options{})

	q, ok, err := provider.QuotaOf(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || q.Total != 1<<40 || q.Used != 1<<38 {
		t.Fatalf("quota = %+v, supported=%v", q, ok)
	}
}
