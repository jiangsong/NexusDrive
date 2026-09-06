package control

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/net/proxy"
)

// The proxy page. Explain and check read the live manager; config reads and
// writes the file. A proxy address may carry a username and password
// (socks5://user:pass@host), and that is a credential like any other: the
// reply redacts it and a request carrying one is refused, so the boundary the
// account endpoints keep holds here too.

// ProxyOutbound, ProxyGroup and ProxyConfig are config.Proxy for the wire.
type ProxyOutbound struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Addr string `json:"addr,omitempty"`
}

type ProxyGroup struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Members  []string `json:"members"`
	CheckURL string   `json:"check_url,omitempty"`
	Interval string   `json:"interval,omitempty"`
	Timeout  string   `json:"timeout,omitempty"`
}

type ProxyConfig struct {
	Outbounds []ProxyOutbound `json:"outbounds"`
	Groups    []ProxyGroup    `json:"groups"`
	Rules     []string        `json:"rules"`
	// Default is the name the built-in rules resolve "proxy" to for this
	// configuration, so the page can show where an unruled overseas host
	// will go.
	Default string `json:"default"`
}

// ProxyExplainResponse is GET /proxy/explain.
type ProxyExplainResponse struct {
	Host     string `json:"host"`
	Rule     string `json:"rule,omitempty"`
	Outbound string `json:"outbound"`
	// Resolved is the concrete outbound after group selection, when the
	// manager could resolve one.
	Resolved string `json:"resolved,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ProxyMutationResponse follows PUT /proxy/config.
type ProxyMutationResponse struct {
	Applied         bool   `json:"applied"`
	RestartRequired bool   `json:"restart_required"`
	Warning         string `json:"warning,omitempty"`
}

// redactAddr strips userinfo from a proxy address, whichever way it was
// written; the scheme and host survive so the page still shows where it goes.
func redactAddr(typ, addr string) string {
	u, err := (proxy.Outbound{Type: typ, Addr: addr}).URL()
	if err != nil || u == nil || u.User == nil {
		return addr
	}
	if strings.Contains(addr, "://") {
		return u.Scheme + "://" + u.Host
	}
	return u.Host
}

func addrHasCredentials(typ, addr string) bool {
	u, err := (proxy.Outbound{Type: typ, Addr: addr}).URL()
	return err == nil && u != nil && u.User != nil
}

func proxyConfigView(p config.Proxy) ProxyConfig {
	out := ProxyConfig{Outbounds: []ProxyOutbound{}, Groups: []ProxyGroup{}, Rules: p.Rules}
	if out.Rules == nil {
		out.Rules = []string{}
	}
	for _, o := range p.Outbounds {
		out.Outbounds = append(out.Outbounds, ProxyOutbound{Name: o.Name, Type: o.Type, Addr: redactAddr(o.Type, o.Addr)})
	}
	for _, g := range p.Groups {
		pg := ProxyGroup{Name: g.Name, Type: g.Type, Members: g.Members, CheckURL: g.CheckURL}
		if pg.Members == nil {
			pg.Members = []string{}
		}
		if g.Interval > 0 {
			pg.Interval = g.Interval.String()
		}
		if g.Timeout > 0 {
			pg.Timeout = g.Timeout.String()
		}
		out.Groups = append(out.Groups, pg)
	}
	out.Default = proxy.DefaultTargetFor(ProxyManagerOptions(p))
	return out
}

// ProxyManagerOptions is the one conversion from the configuration's proxy
// section to what the manager consumes. The daemon uses it to build the
// manager at start and to reload it; this package uses it to explain the
// configuration. One conversion, so a start and a reload cannot disagree.
func ProxyManagerOptions(p config.Proxy) proxy.ManagerOptions {
	var opt proxy.ManagerOptions
	for _, o := range p.Outbounds {
		opt.Outbounds = append(opt.Outbounds, proxy.Outbound{Name: o.Name, Type: o.Type, Addr: o.Addr})
	}
	for _, g := range p.Groups {
		opt.Groups = append(opt.Groups, proxy.Group{
			Name: g.Name, Type: proxy.GroupType(g.Type), Members: g.Members,
			CheckURL: g.CheckURL, Interval: g.Interval, Timeout: g.Timeout,
		})
	}
	opt.Rules = p.Rules
	return opt
}

func proxyConfigFromView(in ProxyConfig) (config.Proxy, error) {
	var p config.Proxy
	for _, o := range in.Outbounds {
		if addrHasCredentials(o.Type, o.Addr) {
			return p, fmt.Errorf("outbound %q carries a username or password; proxy credentials are set in the configuration file, not through this API", o.Name)
		}
		p.Outbounds = append(p.Outbounds, config.Outbound{Name: o.Name, Type: o.Type, Addr: o.Addr})
	}
	for _, g := range in.Groups {
		cg := config.Group{Name: g.Name, Type: g.Type, Members: g.Members, CheckURL: g.CheckURL}
		if g.Interval != "" {
			d, err := time.ParseDuration(g.Interval)
			if err != nil || d < 0 {
				return p, fmt.Errorf("group %q has an invalid interval", g.Name)
			}
			cg.Interval = d
		}
		if g.Timeout != "" {
			d, err := time.ParseDuration(g.Timeout)
			if err != nil || d < 0 {
				return p, fmt.Errorf("group %q has an invalid timeout", g.Name)
			}
			cg.Timeout = d
		}
		if g.CheckURL != "" {
			if u, err := url.Parse(g.CheckURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return p, fmt.Errorf("group %q has an invalid check_url", g.Name)
			}
		}
		p.Groups = append(p.Groups, cg)
	}
	p.Rules = in.Rules
	return p, nil
}

// GET /proxy/explain?host=www.googleapis.com
func (s *Server) proxyExplain(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("host")))
	if host == "" || len(host) > 253 || strings.ContainsAny(host, " /\\\x00") {
		http.Error(w, "host is required and must be a bare host name", http.StatusBadRequest)
		return
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if s.collector.Proxy == nil {
		http.Error(w, "no proxy manager on this daemon", http.StatusNotImplemented)
		return
	}
	out := ProxyExplainResponse{Host: host}
	target := proxy.Target{Host: host}
	if rule := s.collector.Proxy.Router().Explain(target); rule != nil {
		if rule.Kind == proxy.Final {
			out.Rule = string(rule.Kind) + "," + rule.Outbound
		} else {
			out.Rule = string(rule.Kind) + "," + rule.Match + "," + rule.Outbound
		}
	}
	out.Outbound = s.collector.Proxy.Router().Outbound(target)
	if o, err := s.collector.Proxy.Resolve(out.Outbound); err != nil {
		out.Error = err.Error()
	} else {
		out.Resolved = o.Name
	}
	writeJSON(w, out)
}

// POST /proxy/check probes every group now and returns the health table.
func (s *Server) proxyCheck(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.collector.Proxy == nil {
		http.Error(w, "no proxy manager on this daemon", http.StatusNotImplemented)
		return
	}
	health := s.collector.Proxy.CheckNow(r.Context())
	out := make([]ProxyStatus, 0, len(health))
	for _, h := range health {
		out = append(out, ProxyStatus{Name: h.Name, Healthy: h.Healthy, LatencyMS: h.Latency.Milliseconds(), Error: h.Err})
	}
	writeJSON(w, out)
}

// GET|PUT /proxy/config
func (s *Server) proxyConfig(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	cfg := s.collector.Config
	switch r.Method {
	case http.MethodGet:
		if cfg == nil {
			writeJSON(w, proxyConfigView(config.Proxy{}))
			return
		}
		writeJSON(w, proxyConfigView(cfg.Proxy))
	case http.MethodPut:
		if cfg == nil || cfg.SourcePath == "" {
			http.Error(w, "this daemon has no configuration file", http.StatusConflict)
			return
		}
		var in ProxyConfig
		if !decodeMutation(w, r, &in) {
			return
		}
		p, err := proxyConfigFromView(in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := config.SetProxy(cfg.SourcePath, p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.reloadConfigView()
		out := ProxyMutationResponse{RestartRequired: true}
		if s.collector.ReloadProxy != nil {
			if err := s.collector.ReloadProxy(p); err != nil {
				// Saved but not live: say exactly that, never "applied".
				out.Warning = "saved, but the running daemon could not apply it: " + err.Error()
			} else {
				out.Applied, out.RestartRequired = true, false
			}
		}
		writeJSON(w, out)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
