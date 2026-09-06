package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// forwardingProxy is an HTTP proxy that counts what passes through it.
func forwardingProxy(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		outReq, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		resp, err := http.DefaultTransport.RoundTrip(outReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// TestReloadAppliesToTheNextRequest is the claim behind "a proxy change lands
// without a restart": the client every provider already holds asks the
// manager per request, so replacing the manager's routing is enough.
func TestReloadAppliesToTheNextRequest(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer origin.Close()
	first, viaFirst := forwardingProxy(t)
	second, viaSecond := forwardingProxy(t)

	m, err := NewManager(ManagerOptions{
		Outbounds: []Outbound{{Name: "a", Type: "http", Addr: first.URL}},
		Rules:     []string{"DOMAIN-KEYWORD,127.0.0.1,a", "FINAL,direct"},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := m.Client("", 5*time.Second)
	get := func() {
		t.Helper()
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	get()
	if viaFirst.Load() != 1 || viaSecond.Load() != 0 {
		t.Fatalf("before reload: first=%d second=%d", viaFirst.Load(), viaSecond.Load())
	}

	// The configuration now names a different outbound. The same client, no
	// rebuild anywhere, goes there on its next request.
	err = m.Reload(ManagerOptions{
		Outbounds: []Outbound{{Name: "b", Type: "http", Addr: second.URL}},
		Rules:     []string{"DOMAIN-KEYWORD,127.0.0.1,b", "FINAL,direct"},
	})
	if err != nil {
		t.Fatal(err)
	}
	get()
	if viaFirst.Load() != 1 || viaSecond.Load() != 1 {
		t.Fatalf("after reload: first=%d second=%d", viaFirst.Load(), viaSecond.Load())
	}
	if _, err := m.Resolve("a"); err == nil {
		t.Fatal("an outbound the reload removed still resolves")
	}

	// A reload that names an outbound it does not define is refused, and
	// the configuration in force stays exactly what it was.
	if err := m.Reload(ManagerOptions{Rules: []string{"FINAL,nowhere"}, Outbounds: []Outbound{{Name: "x", Type: "http", Addr: first.URL}}}); err == nil {
		t.Fatal("a reload with a dangling rule target was accepted")
	}
	get()
	if viaFirst.Load() != 1 || viaSecond.Load() != 2 {
		t.Fatalf("routing after a refused reload: first=%d second=%d", viaFirst.Load(), viaSecond.Load())
	}
}

// TestReloadFollowsGroupsAndKeepsHealthOfSurvivors: a reload while health
// checking runs starts probing new groups, stops probing removed ones, and
// keeps what it knows about outbounds that are still there.
func TestReloadFollowsGroupsAndKeepsHealthOfSurvivors(t *testing.T) {
	var mu sync.Mutex
	probed := map[string]int{}
	check := func(_ context.Context, o Outbound, _ string, _ time.Duration) (time.Duration, error) {
		mu.Lock()
		defer mu.Unlock()
		probed[o.Name]++
		return time.Millisecond, nil
	}
	m, err := NewManager(ManagerOptions{
		Outbounds: []Outbound{{Name: "hk", Type: "http", Addr: "hk:1"}, {Name: "sg", Type: "http", Addr: "sg:1"}},
		Groups:    []Group{{Name: "auto", Type: URLTest, Members: []string{"hk"}, Interval: 10 * time.Millisecond}},
		Check:     check,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartHealthChecks(ctx)
	defer m.Stop()
	waitFor := func(name string, atLeast int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := probed[name]
			mu.Unlock()
			if n >= atLeast {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("%s was not probed %d times", name, atLeast)
	}
	waitFor("hk", 2)
	if o, err := m.Resolve("auto"); err != nil || o.Name != "hk" {
		t.Fatalf("auto resolves to %+v %v", o, err)
	}

	// Swap the group to a new member set. The old member stops being probed,
	// the new one starts, and the group resolves to something it contains.
	err = m.Reload(ManagerOptions{
		Outbounds: []Outbound{{Name: "hk", Type: "http", Addr: "hk:1"}, {Name: "sg", Type: "http", Addr: "sg:1"}},
		Groups:    []Group{{Name: "auto", Type: URLTest, Members: []string{"sg"}, Interval: 10 * time.Millisecond}, {Name: "backup", Type: Fallback, Members: []string{"hk"}, Interval: 10 * time.Millisecond}},
		Check:     check,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor("sg", 2)
	if o, err := m.Resolve("auto"); err != nil || o.Name != "sg" {
		t.Fatalf("after reload auto resolves to %+v %v; a selection outside the new member set survived", o, err)
	}
	if o, err := m.Resolve("backup"); err != nil || o.Name != "hk" {
		t.Fatalf("a group added by reload does not resolve: %+v %v", o, err)
	}
	mu.Lock()
	hkBefore := probed["hk"]
	mu.Unlock()
	// hk is now only in backup; it keeps being probed there. Remove backup
	// and hk must stop.
	err = m.Reload(ManagerOptions{
		Outbounds: []Outbound{{Name: "sg", Type: "http", Addr: "sg:1"}},
		Groups:    []Group{{Name: "auto", Type: URLTest, Members: []string{"sg"}, Interval: 10 * time.Millisecond}},
		Check:     check,
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	hkAfter := probed["hk"]
	mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	hkLater := probed["hk"]
	mu.Unlock()
	if hkLater != hkAfter {
		t.Fatalf("a removed outbound is still being probed: %d -> %d -> %d", hkBefore, hkAfter, hkLater)
	}
	for _, h := range m.Health() {
		if h.Name == "hk" {
			t.Fatal("health of a removed outbound survived the reload")
		}
	}
}

// TestReloadRacesRequestsCleanly: Reload against requests, resolutions and
// probes, under -race. Any request sees one configuration or the other.
func TestReloadRacesRequestsCleanly(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer origin.Close()
	a, _ := forwardingProxy(t)
	b, _ := forwardingProxy(t)
	optA := ManagerOptions{Outbounds: []Outbound{{Name: "a", Type: "http", Addr: a.URL}}, Groups: []Group{{Name: "g", Type: Fallback, Members: []string{"a"}, Interval: time.Millisecond}}, Rules: []string{"DOMAIN-KEYWORD,127.0.0.1,g", "FINAL,direct"},
		Check: func(context.Context, Outbound, string, time.Duration) (time.Duration, error) { return 0, nil }}
	optB := optA
	optB.Outbounds = []Outbound{{Name: "b", Type: "http", Addr: b.URL}}
	optB.Groups = []Group{{Name: "g", Type: Fallback, Members: []string{"b"}, Interval: time.Millisecond}}
	m, err := NewManager(optA)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartHealthChecks(ctx)
	defer m.Stop()
	client := m.Client("", 5*time.Second)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if resp, err := client.Get(origin.URL); err == nil {
					resp.Body.Close()
				}
				_, _ = m.OutboundFor("127.0.0.1")
			}
		}()
	}
	for i := 0; i < 50; i++ {
		opt := optA
		if i%2 == 1 {
			opt = optB
		}
		if err := m.Reload(opt); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}
