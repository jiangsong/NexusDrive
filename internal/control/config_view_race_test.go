package control

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
)

// Every editing route republishes the shared configuration view while other
// requests read it. The view is what decides whether a credential is
// preserved, which mount a layout belongs to and what the page is shown, so a
// reader must never see one section from before an edit and another from
// after it.
//
// The detector is the point of this test and it only speaks under -race, which
// the repository's baseline run uses. It still asserts on every response, so a
// plain run is not silently reduced to "did not panic": under load every read
// has to keep answering 200 and every write has to keep being accepted, which
// is what a torn or half-published view would break first.
func TestConcurrentConfigReadsAndEditsDoNotRace(t *testing.T) {
	srv, _, _ := accountsServer(t)
	var wg sync.WaitGroup
	var failures atomic.Int64
	check := func(what string, code, want int) {
		if code != want {
			failures.Add(1)
			t.Errorf("%s answered %d while the configuration was being edited, want %d", what, code, want)
		}
	}
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for n := 0; n < 20 && failures.Load() == 0; n++ {
				check("GET /proxy/config", accountRequest(t, srv, http.MethodGet, "/proxy/config", nil).Code, http.StatusOK)
				check("GET /mounts", accountRequest(t, srv, http.MethodGet, "/mounts", nil).Code, http.StatusOK)
				check("GET /accounts", accountRequest(t, srv, http.MethodGet, "/accounts", nil).Code, http.StatusOK)
			}
		}()
		go func() {
			defer wg.Done()
			for n := 0; n < 20 && failures.Load() == 0; n++ {
				check("PUT /proxy/config", accountRequest(t, srv, http.MethodPut, "/proxy/config", ProxyConfig{
					Outbounds: []ProxyOutbound{{Name: "hk", Type: "socks5", Addr: "127.0.0.1:1080"}},
					Rules:     []string{"FINAL,hk"},
				}).Code, http.StatusOK)
			}
		}()
	}
	wg.Wait()

	// And the view that survives the storm is the one the last write left, not
	// a snapshot from before it.
	cfg := srv.collector.ConfigView()
	if cfg == nil || len(cfg.Proxy.Outbounds) != 1 || cfg.Proxy.Outbounds[0].Name != "hk" {
		t.Fatalf("the published view does not hold the outbound every write installed: %+v", cfg.Proxy.Outbounds)
	}
}
