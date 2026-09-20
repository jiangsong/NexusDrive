package control

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/i18n"
)

// TestCacheConfigSavesAndAppliesTheBudget: the console's budget edit lands
// in the file — so a restart keeps it — and in the running cache — so a
// disk that was refusing writes for want of headroom admits them at once.
// The status document reports the live figures from then on.
func TestCacheConfigSavesAndAppliesTheBudget(t *testing.T) {
	srv, cfg, path := accountsServer(t)
	free := int64(4 << 30)
	ca, err := cache.New(cache.Options{
		Dir: filepath.Join(filepath.Dir(path), "cache"), BlockSize: 4096, MaxBytes: 50 << 30, MinFree: 5 << 30,
		FreeSpace: func(string) (int64, error) { return free, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close() })
	srv.collector.Cache = ca
	srv.collector.FreeSpace = func(string) (int64, error) { return free, nil }
	_ = cfg

	rr := accountRequest(t, srv, http.MethodGet, "/cache/config", nil)
	var view CacheBudget
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("GET: %d %s", rr.Code, rr.Body)
	}
	if view.MaxBytes != 50<<30 || view.MinFree != 5<<30 || view.FreeBytes != free {
		t.Fatalf("view = %+v", view)
	}
	// 4 GiB free under a 5 GiB headroom: a write has no room.
	if _, err := ca.ReserveDisk(ca.Dir(), 1<<20); err == nil {
		t.Fatal("a write was admitted below the headroom")
	}

	rr = accountRequest(t, srv, http.MethodPut, "/cache/config", CacheBudget{MaxBytes: 20 << 30, MinFree: 1 << 30})
	if rr.Code != 200 {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Applied || view.MaxBytes != 20<<30 || view.MinFree != 1<<30 {
		t.Fatalf("after PUT: %+v", view)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Cache.MaxSize != 20<<30 || saved.Cache.MinFree != 1<<30 {
		t.Fatalf("saved = %+v", saved.Cache)
	}
	if maxBytes, minFree := ca.Budget(); maxBytes != 20<<30 || minFree != 1<<30 {
		t.Fatalf("live budget = %d %d", maxBytes, minFree)
	}
	release, err := ca.ReserveDisk(ca.Dir(), 1<<20)
	if err != nil {
		t.Fatalf("write still refused after lowering the headroom: %v", err)
	}
	release()
	st := srv.collector.Collect(t.Context(), i18n.Lang("en"))
	if st.Cache.MaxBytes != 20<<30 || st.Cache.MinFree != 1<<30 {
		t.Fatalf("status budget = %+v", st.Cache)
	}

	if rr := accountRequest(t, srv, http.MethodPut, "/cache/config", CacheBudget{MaxBytes: -1}); rr.Code != 400 {
		t.Fatalf("negative budget: %d %s", rr.Code, rr.Body)
	}
	if rr := accountRequest(t, srv, http.MethodPost, "/cache/config", nil); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rr.Code)
	}
}
