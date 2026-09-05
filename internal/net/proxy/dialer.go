package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// Outbound is one egress path: direct, an HTTP proxy or a SOCKS5 proxy.
type Outbound struct {
	Name string
	Type string // direct | http | socks5
	Addr string
}

// URL renders the outbound as a proxy URL, or nil for direct.
func (o Outbound) URL() (*url.URL, error) {
	switch o.Type {
	case "direct", "":
		return nil, nil
	case "http", "https":
		addr := o.Addr
		if !strings.Contains(addr, "://") {
			addr = "http://" + addr
		}
		return url.Parse(addr)
	case "socks5":
		addr := o.Addr
		if !strings.Contains(addr, "://") {
			addr = "socks5://" + addr
		}
		return url.Parse(addr)
	default:
		return nil, fmt.Errorf("proxy: unknown outbound type %q", o.Type)
	}
}

// GroupType selects how a group picks among its members.
type GroupType string

const (
	// Fallback uses the first healthy member, in order.
	Fallback GroupType = "fallback"
	// URLTest uses the member with the lowest measured latency.
	URLTest GroupType = "url-test"
)

// Group is a set of outbounds with health checking.
type Group struct {
	Name     string
	Type     GroupType
	Members  []string
	CheckURL string
	Interval time.Duration
	Timeout  time.Duration
}

// Health is the last observed state of one outbound.
type Health struct {
	Name    string
	Healthy bool
	Latency time.Duration
	Checked time.Time
	Err     string
}

// Manager resolves outbound names, runs health checks and hands out the
// http.Transport every provider uses, so proxy rules and rate limits apply
// uniformly (docs/DESIGN.md §4.2).
type Manager struct {
	router    *Router
	outbounds map[string]Outbound
	groups    map[string]Group

	mu     sync.RWMutex
	health map[string]Health
	// resolved caches the member a group currently selects.
	selected map[string]string

	stopOnce sync.Once
	stopC    chan struct{}
	// checkFn is injectable for tests.
	checkFn func(ctx context.Context, o Outbound, url string, timeout time.Duration) (time.Duration, error)
}

// ManagerOptions configures New.
type ManagerOptions struct {
	Outbounds []Outbound
	Groups    []Group
	Rules     []string
	// GeoIP resolves an IP to an ISO country code for GEOIP rules.
	GeoIP func(net.IP) string
	// Check overrides the health probe (tests).
	Check func(ctx context.Context, o Outbound, url string, timeout time.Duration) (time.Duration, error)
}

// NewManager builds a manager. Rules default to DefaultRules when empty.
func NewManager(opt ManagerOptions) (*Manager, error) {
	rules := opt.Rules
	if len(rules) == 0 {
		rules = DefaultRules
	}
	router, err := NewRouter(rules)
	if err != nil {
		return nil, err
	}
	router.GeoIP = opt.GeoIP

	m := &Manager{
		router:    router,
		outbounds: map[string]Outbound{"direct": {Name: "direct", Type: "direct"}},
		groups:    map[string]Group{},
		health:    map[string]Health{},
		selected:  map[string]string{},
		stopC:     make(chan struct{}),
		checkFn:   opt.Check,
	}
	if m.checkFn == nil {
		m.checkFn = probe
	}
	for _, o := range opt.Outbounds {
		if _, err := o.URL(); err != nil {
			return nil, err
		}
		m.outbounds[o.Name] = o
	}
	for _, g := range opt.Groups {
		if g.Type == "" {
			g.Type = Fallback
		}
		if g.CheckURL == "" {
			g.CheckURL = "https://www.gstatic.com/generate_204"
		}
		if g.Interval <= 0 {
			g.Interval = time.Minute
		}
		if g.Timeout <= 0 {
			g.Timeout = 5 * time.Second
		}
		for _, member := range g.Members {
			if _, ok := m.outbounds[member]; !ok {
				return nil, fmt.Errorf("proxy: group %q references unknown outbound %q", g.Name, member)
			}
		}
		m.groups[g.Name] = g
		if len(g.Members) > 0 {
			// Assume healthy until a check says otherwise, so the first
			// request does not have to wait for a probe.
			m.selected[g.Name] = g.Members[0]
		}
	}
	return m, nil
}

// Router exposes the rule router for `cloudfs proxy test`.
func (m *Manager) Router() *Router { return m.router }

// StartHealthChecks runs periodic probes until Stop.
func (m *Manager) StartHealthChecks(ctx context.Context) {
	for name := range m.groups {
		g := m.groups[name]
		go func(g Group) {
			t := time.NewTicker(g.Interval)
			defer t.Stop()
			m.checkGroup(ctx, g)
			for {
				select {
				case <-ctx.Done():
					return
				case <-m.stopC:
					return
				case <-t.C:
					m.checkGroup(ctx, g)
				}
			}
		}(g)
	}
}

// Stop ends health checking.
func (m *Manager) Stop() { m.stopOnce.Do(func() { close(m.stopC) }) }

// CheckNow probes every group once and returns the results. `cloudfs proxy
// test` and `doctor` use it.
func (m *Manager) CheckNow(ctx context.Context) []Health {
	for _, g := range m.groups {
		m.checkGroup(ctx, g)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Health, 0, len(m.health))
	for _, h := range m.health {
		out = append(out, h)
	}
	return out
}

func (m *Manager) checkGroup(ctx context.Context, g Group) {
	type result struct {
		name    string
		latency time.Duration
		err     error
	}
	results := make([]result, 0, len(g.Members))
	for _, member := range g.Members {
		o := m.outbounds[member]
		lat, err := m.checkFn(ctx, o, g.CheckURL, g.Timeout)
		results = append(results, result{name: member, latency: lat, err: err})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var best string
	var bestLat time.Duration
	for _, r := range results {
		h := Health{Name: r.name, Healthy: r.err == nil, Latency: r.latency, Checked: time.Now()}
		if r.err != nil {
			h.Err = r.err.Error()
		}
		m.health[r.name] = h
		if r.err != nil {
			continue
		}
		switch g.Type {
		case URLTest:
			if best == "" || r.latency < bestLat {
				best, bestLat = r.name, r.latency
			}
		default: // Fallback: first healthy in declared order
			if best == "" {
				best, bestLat = r.name, r.latency
			}
		}
	}
	if best != "" {
		m.selected[g.Name] = best
	}
	// Every member is down: keep the previous selection so a transient probe
	// failure does not strand requests with no outbound at all.
}

// probe measures how long a HEAD request through o takes.
func probe(ctx context.Context, o Outbound, target string, timeout time.Duration) (time.Duration, error) {
	tr, err := transportFor(o)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Transport: tr, Timeout: timeout}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return 0, fmt.Errorf("proxy: health check returned %s", resp.Status)
	}
	return time.Since(start), nil
}

// Resolve maps an outbound-or-group name to a concrete outbound.
func (m *Manager) Resolve(name string) (Outbound, error) {
	if name == "" {
		name = "direct"
	}
	m.mu.RLock()
	sel, isGroup := m.selected[name]
	m.mu.RUnlock()
	if isGroup {
		name = sel
	} else if _, ok := m.groups[name]; ok {
		return Outbound{}, fmt.Errorf("proxy: group %q has no healthy member", name)
	}
	o, ok := m.outbounds[name]
	if !ok {
		return Outbound{}, fmt.Errorf("proxy: unknown outbound %q", name)
	}
	return o, nil
}

// OutboundFor returns the outbound the rules pick for a host.
func (m *Manager) OutboundFor(host string) (Outbound, error) {
	name := m.router.Outbound(Target{Host: host})
	return m.Resolve(name)
}

// Health returns the last known state of every probed outbound.
func (m *Manager) Health() []Health {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Health, 0, len(m.health))
	for _, h := range m.health {
		out = append(out, h)
	}
	return out
}

// transportFor builds an http.Transport that egresses through o.
func transportFor(o Outbound) (*http.Transport, error) {
	tr := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	u, err := o.URL()
	if err != nil {
		return nil, err
	}
	if u == nil {
		return tr, nil
	}
	switch o.Type {
	case "http", "https":
		tr.Proxy = http.ProxyURL(u)
	case "socks5":
		var auth *xproxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &xproxy.Auth{User: u.User.Username(), Password: pw}
		}
		dialer, err := xproxy.SOCKS5("tcp", u.Host, auth, xproxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("proxy: socks5 %s: %w", u.Host, err)
		}
		ctxDialer, ok := dialer.(xproxy.ContextDialer)
		if !ok {
			return nil, errors.New("proxy: socks5 dialer does not support contexts")
		}
		tr.DialContext = ctxDialer.DialContext
	}
	return tr, nil
}

// ruleTransport routes each request through the outbound its host resolves to.
// One instance serves every provider, so a single rule set governs the whole
// process.
type ruleTransport struct {
	m *Manager
	// override, when set, pins every request to one outbound (per-remote
	// configuration wins over the rules).
	override string

	mu         sync.Mutex
	transports map[string]*http.Transport
}

func (t *ruleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	name := t.override
	if name == "" {
		name = t.m.router.Outbound(Target{Host: req.URL.Hostname()})
	}
	o, err := t.m.Resolve(name)
	if err != nil {
		return nil, err
	}
	tr, err := t.transportFor(o)
	if err != nil {
		return nil, err
	}
	return tr.RoundTrip(req)
}

func (t *ruleTransport) transportFor(o Outbound) (*http.Transport, error) {
	key := o.Name + "|" + o.Type + "|" + o.Addr
	t.mu.Lock()
	defer t.mu.Unlock()
	if tr, ok := t.transports[key]; ok {
		return tr, nil
	}
	tr, err := transportFor(o)
	if err != nil {
		return nil, err
	}
	if t.transports == nil {
		t.transports = map[string]*http.Transport{}
	}
	t.transports[key] = tr
	return tr, nil
}

// CloseIdleConnections releases pooled connections.
func (t *ruleTransport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tr := range t.transports {
		tr.CloseIdleConnections()
	}
}

// Transport returns an http.RoundTripper that applies the rules. Pass a
// non-empty override to pin a remote to one outbound regardless of the rules.
func (m *Manager) Transport(override string) http.RoundTripper {
	return &ruleTransport{m: m, override: override, transports: map[string]*http.Transport{}}
}

// Client returns an http.Client using Transport(override).
func (m *Manager) Client(override string, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &http.Client{Transport: m.Transport(override), Timeout: timeout}
}
