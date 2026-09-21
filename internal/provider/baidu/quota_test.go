package baidu

import (
	"context"
	"testing"

	"cloudfs/internal/provider"
)

func TestQuota(t *testing.T) {
	x, hs := newXpan(t)
	x.reply(pathQuota, "", "", `{"errno":0,"total":5368709120,"used":1073741824,"free":4294967296}`)
	p := newProvider(t, hs)

	q, ok, err := provider.QuotaOf(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || q.Total != 5368709120 || q.Used != 1073741824 {
		t.Fatalf("quota = %+v, supported=%v", q, ok)
	}
	c := x.all()[0]
	if c.Path != pathQuota || c.Query.Get("access_token") != "tok-1" || c.Query.Get("checkfree") != "1" || c.Query.Get("checkexpire") != "1" {
		t.Fatalf("quota request = %+v", c)
	}
}
