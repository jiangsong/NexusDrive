package pan123

import (
	"context"
	"net/http"
	"testing"

	"cloudfs/internal/provider"
)

func TestQuota(t *testing.T) {
	a, hs := newAPI(t)
	a.on(pathUserInfo, func(c call) (int, string) {
		if c.Method != http.MethodGet || c.Header.Get("Platform") != Platform {
			t.Fatalf("quota request = %+v", c)
		}
		return http.StatusOK, ok(`{"spaceUsed":1073741824,"spacePermanent":5368709120,"spaceTemp":2147483648}`)
	})
	p := newProvider(t, hs)

	q, supported, err := provider.QuotaOf(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !supported || q.Total != 7<<30 || q.Used != 1<<30 {
		t.Fatalf("quota = %+v, supported=%v", q, supported)
	}
}
