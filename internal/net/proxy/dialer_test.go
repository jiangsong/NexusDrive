package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestOutboundURL(t *testing.T) {
	cases := []struct {
		o    Outbound
		want string
	}{
		{Outbound{Type: "direct"}, ""},
		{Outbound{Type: "http", Addr: "127.0.0.1:8080"}, "http://127.0.0.1:8080"},
		{Outbound{Type: "http", Addr: "http://user:pw@host:3128"}, "http://user:pw@host:3128"},
		{Outbound{Type: "socks5", Addr: "127.0.0.1:7890"}, "socks5://127.0.0.1:7890"},
	}
	for _, c := range cases {
		u, err := c.o.URL()
		if err != nil {
			t.Fatalf("%+v: %v", c.o, err)
		}
		got := ""
		if u != nil {
			got = u.String()
		}
		if got != c.want {
			t.Errorf("%+v URL = %q, want %q", c.o, got, c.want)
		}
	}
	if _, err := (Outbound{Type: "carrier-pigeon"}).URL(); err == nil {
		t.Error("unknown type should fail")
	}
}

func TestManagerResolvesGroups(t *testing.T) {
	m, err := NewManager(ManagerOptions{
		Outbounds: []Outbound{
			{Name: "a", Type: "socks5", Addr: "127.0.0.1:1080"},
			{Name: "b", Type: "http", Addr: "127.0.0.1:3128"},
		},
		Groups: []Group{{Name: "proxy", Type: Fallback, Members: []string{"a", "b"}}},
		Rules:  []string{"DOMAIN-SUFFIX,example.com,proxy", "FINAL,direct"},
		Check: func(_ context.Context, o Outbound, _ string, _ time.Duration) (time.Duration, error) {
			if o.Name == "a" {
				return 0, errors.New("down")
			}
			return 20 * time.Millisecond, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Before any check the first member is assumed usable.
	o, err := m.Resolve("proxy")
	if err != nil || o.Name != "a" {
		t.Fatalf("initial resolve = %+v, %v", o, err)
	}
	// After a check the unhealthy member is skipped.
	m.CheckNow(context.Background())
	o, err = m.Resolve("proxy")
	if err != nil || o.Name != "b" {
		t.Fatalf("after health check = %+v, %v", o, err)
	}
	var health map[string]Health = map[string]Health{}
	for _, h := range m.Health() {
		health[h.Name] = h
	}
	if health["a"].Healthy || !health["b"].Healthy {
		t.Fatalf("health = %+v", health)
	}
	// Rules pick the group for matching hosts and direct otherwise.
	o, _ = m.OutboundFor("api.example.com")
	if o.Name != "b" {
		t.Fatalf("routed outbound = %+v", o)
	}
	o, _ = m.OutboundFor("openapi.alipan.com")
	if o.Name != "direct" {
		t.Fatalf("unmatched host should go direct, got %+v", o)
	}
}

func TestURLTestPicksFastest(t *testing.T) {
	m, err := NewManager(ManagerOptions{
		Outbounds: []Outbound{
			{Name: "slow", Type: "http", Addr: "127.0.0.1:1"},
			{Name: "fast", Type: "http", Addr: "127.0.0.1:2"},
		},
		Groups: []Group{{Name: "best", Type: URLTest, Members: []string{"slow", "fast"}}},
		Check: func(_ context.Context, o Outbound, _ string, _ time.Duration) (time.Duration, error) {
			if o.Name == "slow" {
				return 500 * time.Millisecond, nil
			}
			return 10 * time.Millisecond, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.CheckNow(context.Background())
	o, err := m.Resolve("best")
	if err != nil || o.Name != "fast" {
		t.Fatalf("url-test resolve = %+v, %v", o, err)
	}
}

func TestAllMembersDownKeepsLastSelection(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	m, err := NewManager(ManagerOptions{
		Outbounds: []Outbound{{Name: "a", Type: "http", Addr: "127.0.0.1:1"}},
		Groups:    []Group{{Name: "g", Members: []string{"a"}}},
		Check: func(_ context.Context, _ Outbound, _ string, _ time.Duration) (time.Duration, error) {
			if healthy.Load() {
				return time.Millisecond, nil
			}
			return 0, errors.New("network unreachable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.CheckNow(context.Background())
	healthy.Store(false)
	m.CheckNow(context.Background())
	// Requests still have somewhere to go rather than failing outright.
	o, err := m.Resolve("g")
	if err != nil || o.Name != "a" {
		t.Fatalf("resolve with everything down = %+v, %v", o, err)
	}
	for _, h := range m.Health() {
		if h.Name == "a" && h.Healthy {
			t.Fatal("health should record the failure even though the selection stands")
		}
	}
}

func TestUnknownOutboundRejected(t *testing.T) {
	_, err := NewManager(ManagerOptions{
		Groups: []Group{{Name: "g", Members: []string{"ghost"}}},
	})
	if err == nil {
		t.Fatal("group with an unknown member should fail to build")
	}
	m, _ := NewManager(ManagerOptions{})
	if _, err := m.Resolve("nope"); err == nil {
		t.Fatal("unknown outbound should fail")
	}
	if o, err := m.Resolve(""); err != nil || o.Type != "direct" {
		t.Fatalf("empty name should mean direct, got %+v %v", o, err)
	}
}

// TestTransportRoutesThroughProxy runs a real HTTP proxy and a real origin
// server, then checks that rule routing actually sends the request through the
// proxy for matching hosts and direct otherwise.
func TestTransportRoutesThroughProxy(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("origin:" + r.Host))
	}))
	defer origin.Close()

	var proxied atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		// Forward the absolute-URI request to the origin.
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
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
	}))
	defer proxy.Close()

	originHost, _, _ := net.SplitHostPort(origin.Listener.Addr().String())
	_ = originHost

	m, err := NewManager(ManagerOptions{
		Outbounds: []Outbound{{Name: "px", Type: "http", Addr: proxy.URL}},
		Rules: []string{
			"DOMAIN-KEYWORD,127.0.0.1,px",
			"FINAL,direct",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := m.Client("", 5*time.Second)
	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if proxied.Load() != 1 {
		t.Fatalf("request did not go through the proxy: %d proxied", proxied.Load())
	}

	// An override pins a remote to direct even though the rules say proxy.
	before := proxied.Load()
	direct := m.Client("direct", 5*time.Second)
	resp, err = direct.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if proxied.Load() != before {
		t.Fatal("per-remote override should bypass the rules")
	}
}

func TestTransportReusesConnections(t *testing.T) {
	m, err := NewManager(ManagerOptions{Rules: []string{"FINAL,direct"}})
	if err != nil {
		t.Fatal(err)
	}
	tr := m.Transport("").(*ruleTransport)
	o := Outbound{Name: "direct", Type: "direct"}
	a, err := tr.transportFor(o)
	if err != nil {
		t.Fatal(err)
	}
	b, err := tr.transportFor(o)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("the same outbound must reuse one transport, otherwise connection pooling is lost")
	}
	tr.CloseIdleConnections()
}

func TestTransportForSetsMaxConnsPerHost(t *testing.T) {
	tr, err := transportFor(Outbound{Type: "direct"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tr.MaxConnsPerHost != 0 || tr.MaxIdleConnsPerHost != 8 {
		t.Fatalf("conns<=0 should keep today's defaults, got %+v", tr)
	}
	tr, err = transportFor(Outbound{Type: "direct"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if tr.MaxConnsPerHost != 5 || tr.MaxIdleConnsPerHost != 5 {
		t.Fatalf("conns=5 should bound both idle and in-flight, got %+v", tr)
	}
}

func TestClientWithLimitAppliesMaxConnsPerHost(t *testing.T) {
	m, err := NewManager(ManagerOptions{Rules: []string{"FINAL,direct"}})
	if err != nil {
		t.Fatal(err)
	}
	client, setConns := m.ClientWithLimit("", 5*time.Second)
	setConns(3)
	tr := client.Transport.(*ruleTransport)
	o := Outbound{Name: "direct", Type: "direct"}

	first, err := tr.transportFor(o)
	if err != nil {
		t.Fatal(err)
	}
	if first.MaxConnsPerHost != 3 {
		t.Fatalf("MaxConnsPerHost = %d, want 3", first.MaxConnsPerHost)
	}

	setConns(5)
	second, err := tr.transportFor(o)
	if err != nil {
		t.Fatal(err)
	}
	if second.MaxConnsPerHost != 5 {
		t.Fatalf("MaxConnsPerHost = %d, want 5", second.MaxConnsPerHost)
	}
	if first == second {
		t.Fatal("changing the limit should yield a distinct transport")
	}
}

func TestClientPlainStillGetsTodaysDefaults(t *testing.T) {
	m, err := NewManager(ManagerOptions{Rules: []string{"FINAL,direct"}})
	if err != nil {
		t.Fatal(err)
	}
	client := m.Client("", 5*time.Second)
	tr := client.Transport.(*ruleTransport)
	got, err := tr.transportFor(Outbound{Name: "direct", Type: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxConnsPerHost != 0 || got.MaxIdleConnsPerHost != 8 {
		t.Fatalf("Client() without ClientWithLimit should keep the old defaults, got %+v", got)
	}
}
