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
	"sync/atomic"
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
	// routing is the rule set and the outbounds and groups it names. It is
	// replaced as a whole by Reload and read without a lock by every request,
	// so a request sees either the old configuration or the new one, never a
	// mix of the two.
	routing atomic.Pointer[routingState]

	mu     sync.RWMutex
	health map[string]Health
	// selected caches the member a group currently selects.
	selected map[string]string
	// checkers holds the stop channel of the health goroutine per group,
	// once StartHealthChecks has run; Reload starts and stops them to match
	// the new group set.
	checkers map[string]chan struct{}
	checkCtx context.Context

	stopOnce sync.Once
	stopC    chan struct{}
	// checkFn is injectable for tests.
	checkFn func(ctx context.Context, o Outbound, url string, timeout time.Duration) (time.Duration, error)
}

// routingState is one consistent configuration: what NewManager built, or
// what Reload replaced it with.
type routingState struct {
	router    *Router
	outbounds map[string]Outbound
	groups    map[string]Group
}

// buildRouting validates opt into a routingState. NewManager and Reload share
// it, so a configuration Reload accepts is exactly one NewManager would.
func buildRouting(opt ManagerOptions) (*routingState, error) {
	rules := opt.Rules
	if len(rules) == 0 {
		rules = defaultRulesFor(opt)
	}
	router, err := NewRouter(rules)
	if err != nil {
		return nil, err
	}
	router.GeoIP = opt.GeoIP
	st := &routingState{
		router:    router,
		outbounds: map[string]Outbound{"direct": {Name: "direct", Type: "direct"}},
		groups:    map[string]Group{},
	}
	for _, o := range opt.Outbounds {
		if _, err := o.URL(); err != nil {
			return nil, err
		}
		st.outbounds[o.Name] = o
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
			if _, ok := st.outbounds[member]; !ok {
				return nil, fmt.Errorf("proxy: group %q references unknown outbound %q", g.Name, member)
			}
		}
		st.groups[g.Name] = g
	}
	// A rule that names an outbound or group which does not exist is refused
	// here, not discovered by the first request that happens to match it. This
	// lives in buildRouting so NewManager (daemon start) and Reload validate
	// identically: a configuration one accepts is exactly one the other does.
	for _, r := range st.router.Rules() {
		if _, ok := st.outbounds[r.Outbound]; ok {
			continue
		}
		if _, ok := st.groups[r.Outbound]; ok {
			continue
		}
		return nil, fmt.Errorf("proxy: rule %s targets unknown outbound %q", r.Kind, r.Outbound)
	}
	return st, nil
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
	st, err := buildRouting(opt)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		health:   map[string]Health{},
		selected: map[string]string{},
		stopC:    make(chan struct{}),
		checkFn:  opt.Check,
	}
	if m.checkFn == nil {
		m.checkFn = probe
	}
	m.routing.Store(st)
	for name, g := range st.groups {
		if len(g.Members) > 0 {
			// Assume healthy until a check says otherwise, so the first
			// request does not have to wait for a probe.
			m.selected[name] = g.Members[0]
		}
	}
	return m, nil
}

// Reload replaces the routing configuration of a running manager. Requests in
// flight finish on whatever they resolved; the next request resolves against
// the new rules. Health for outbounds that still exist is kept; state for
// groups and outbounds that are gone is dropped, and health checking follows
// the new group set when it is running. A configuration Reload refuses is one
// NewManager would refuse, and the old configuration stays in force.
//
// This is what lets a proxy change land without restarting the daemon: every
// provider's HTTP client asks this manager per request, so nothing but the
// manager's own state has to move.
func (m *Manager) Reload(opt ManagerOptions) error {
	// buildRouting validates rule targets, so a configuration Reload refuses is
	// one NewManager would refuse, and the old one stays in force.
	st, err := buildRouting(opt)
	if err != nil {
		return err
	}
	old := m.routing.Swap(st)
	m.mu.Lock()
	defer m.mu.Unlock()
	for name := range m.selected {
		if _, ok := st.groups[name]; !ok {
			delete(m.selected, name)
		}
	}
	for name := range m.health {
		if _, ok := st.outbounds[name]; !ok {
			delete(m.health, name)
		}
	}
	for name, g := range st.groups {
		// A group whose selection no longer names a member starts over.
		if sel, ok := m.selected[name]; !ok || !containsString(g.Members, sel) {
			if len(g.Members) > 0 {
				m.selected[name] = g.Members[0]
			}
		}
	}
	if m.checkers != nil {
		for name, stop := range m.checkers {
			ng, ok := st.groups[name]
			if !ok || old == nil || !sameGroup(old.groups[name], ng) {
				close(stop)
				delete(m.checkers, name)
			}
		}
		for name, g := range st.groups {
			if _, running := m.checkers[name]; !running {
				m.startCheckerLocked(g)
			}
		}
	}
	return nil
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func sameGroup(a, b Group) bool {
	if a.Name != b.Name || a.Type != b.Type || a.CheckURL != b.CheckURL || a.Interval != b.Interval || a.Timeout != b.Timeout || len(a.Members) != len(b.Members) {
		return false
	}
	for i := range a.Members {
		if a.Members[i] != b.Members[i] {
			return false
		}
	}
	return true
}

// Router exposes the rule router for `cloudfs proxy test`. It is the router
// of the configuration in force when called; a Reload afterwards does not
// change the returned value.
func (m *Manager) Router() *Router { return m.routing.Load().router }

// StartHealthChecks runs periodic probes until Stop. Reload keeps the set of
// probed groups in step with the configuration from then on.
func (m *Manager) StartHealthChecks(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.checkers != nil {
		return
	}
	m.checkers = map[string]chan struct{}{}
	m.checkCtx = ctx
	for _, g := range m.routing.Load().groups {
		m.startCheckerLocked(g)
	}
}

// startCheckerLocked runs one group's probe loop until Stop, ctx, or the
// group's own stop channel — which Reload closes when the group changes or
// goes away. m.mu must be held.
func (m *Manager) startCheckerLocked(g Group) {
	stop := make(chan struct{})
	m.checkers[g.Name] = stop
	ctx := m.checkCtx
	go func() {
		t := time.NewTicker(g.Interval)
		defer t.Stop()
		m.checkGroup(ctx, g)
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.stopC:
				return
			case <-stop:
				return
			case <-t.C:
				m.checkGroup(ctx, g)
			}
		}
	}()
}

// Stop ends health checking.
func (m *Manager) Stop() { m.stopOnce.Do(func() { close(m.stopC) }) }

// CheckNow probes every group once and returns the results. `cloudfs proxy
// test` and `doctor` use it.
func (m *Manager) CheckNow(ctx context.Context) []Health {
	for _, g := range m.routing.Load().groups {
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
	outbounds := m.routing.Load().outbounds
	for _, member := range g.Members {
		o, ok := outbounds[member]
		if !ok {
			continue // the group was reloaded out from under this probe
		}
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
		if _, still := m.routing.Load().groups[g.Name]; still {
			m.selected[g.Name] = best
		}
	}
	// Every member is down: keep the previous selection so a transient probe
	// failure does not strand requests with no outbound at all.
}

// probe measures how long a HEAD request through o takes.
func probe(ctx context.Context, o Outbound, target string, timeout time.Duration) (time.Duration, error) {
	tr, err := transportFor(o, 0)
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
	return m.resolveIn(m.routing.Load(), name)
}

// resolveIn resolves a route name to an outbound within one routing snapshot.
// Callers that also picked the name from a snapshot (OutboundFor, RoundTrip)
// pass that same snapshot in, so a routing decision and the outbound/group
// definition it resolves to can never come from two different configurations
// across a concurrent Reload — the invariant routingState documents.
func (m *Manager) resolveIn(st *routingState, name string) (Outbound, error) {
	if name == "" {
		name = "direct"
	}
	m.mu.RLock()
	sel, isGroup := m.selected[name]
	m.mu.RUnlock()
	if _, isConfiguredGroup := st.groups[name]; isConfiguredGroup {
		if !isGroup {
			return Outbound{}, fmt.Errorf("proxy: group %q has no healthy member", name)
		}
		name = sel
	}
	o, ok := st.outbounds[name]
	if !ok {
		return Outbound{}, fmt.Errorf("proxy: unknown outbound %q", name)
	}
	return o, nil
}

// OutboundFor returns the outbound the rules pick for a host.
func (m *Manager) OutboundFor(host string) (Outbound, error) {
	st := m.routing.Load()
	name := st.router.Outbound(Target{Host: host})
	return m.resolveIn(st, name)
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

// transportFor builds an http.Transport that egresses through o. conns
// bounds both the idle and in-flight connections per host; conns<=0 keeps
// today's behaviour (8 idle, unbounded in flight).
func transportFor(o Outbound, conns int) (*http.Transport, error) {
	idle := 8
	var maxConns int
	if conns > 0 {
		idle = conns
		maxConns = conns
	}
	tr := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: idle,
		MaxConnsPerHost:     maxConns,
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

// transportKey identifies one cached transport: an outbound plus the
// connection limit it was built with. The limit is part of the key so that
// changing it (setConns) never hands an in-flight caller a transport that
// was just closed out from under it — callers keep whatever transport they
// already picked up, and only new lookups see the new limit.
type transportKey struct {
	name, typ, addr string
	conns           int
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
	conns      int // 0 = today's defaults; see transportFor.
	transports map[transportKey]*http.Transport
}

func (t *ruleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	st := t.m.routing.Load()
	name := t.override
	if name == "" {
		name = st.router.Outbound(Target{Host: req.URL.Hostname()})
	}
	o, err := t.m.resolveIn(st, name)
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
	t.mu.Lock()
	conns := t.conns
	key := transportKey{o.Name, o.Type, o.Addr, conns}
	if tr, ok := t.transports[key]; ok {
		t.mu.Unlock()
		return tr, nil
	}
	t.mu.Unlock()

	tr, err := transportFor(o, conns)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if existing, ok := t.transports[key]; ok {
		// Another caller built one for the same key while we were building
		// ours (or the limit briefly cycled back); keep the one already in
		// the cache and drop the duplicate we just made.
		tr.CloseIdleConnections()
		return existing, nil
	}
	if t.transports == nil {
		t.transports = map[transportKey]*http.Transport{}
	}
	t.transports[key] = tr
	return tr, nil
}

// setConns changes the connection limit applied to transports built from now
// on. Transports already cached under the old limit are closed and dropped:
// a request in flight through one keeps using it (RoundTrip already has the
// pointer), but nothing new picks it up again.
func (t *ruleTransport) setConns(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conns == n {
		return
	}
	t.conns = n
	for k, tr := range t.transports {
		if k.conns != n {
			tr.CloseIdleConnections()
			delete(t.transports, k)
		}
	}
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
	return &ruleTransport{m: m, override: override, transports: map[transportKey]*http.Transport{}}
}

// ClientWithLimit returns an http.Client using Transport(override), plus a
// setter that bounds the connections per host across every outbound this
// client's transport builds. Passing conns<=0 to the setter (or never
// calling it) keeps today's defaults: 8 idle per host, unbounded in flight.
func (m *Manager) ClientWithLimit(override string, timeout time.Duration) (*http.Client, func(conns int)) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	rt := &ruleTransport{m: m, override: override, transports: map[transportKey]*http.Transport{}}
	return &http.Client{Transport: rt, Timeout: timeout}, rt.setConns
}

// Client returns an http.Client using Transport(override), with today's
// fixed connection limits. It is a thin wrapper over ClientWithLimit for
// callers that have no per-remote override to apply.
func (m *Manager) Client(override string, timeout time.Duration) *http.Client {
	client, _ := m.ClientWithLimit(override, timeout)
	return client
}
