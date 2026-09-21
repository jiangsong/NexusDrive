package pan115

import (
	"context"
	"net/http"
	"testing"

	"cloudfs/internal/provider"
)

func TestQuota(t *testing.T) {
	p, rec, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/open/user/info" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, okEnvelope(map[string]any{
			"rt_space_info": map[string]any{
				"all_total": map[string]any{"size": "1099511627776", "size_format": "1TB"},
				"all_use":   map[string]any{"size": 274877906944, "size_format": "256GB"},
			},
		}))
	})

	q, ok, err := provider.QuotaOf(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || q.Total != 1<<40 || q.Used != 1<<38 {
		t.Fatalf("quota = %+v, supported=%v", q, ok)
	}
	got := rec.last(t, "/open/user/info")
	if got.Method != http.MethodGet || got.Header.Get("Authorization") != "Bearer access-1" {
		t.Fatalf("quota request = %+v", got)
	}
}
