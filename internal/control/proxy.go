package control

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/i18n"
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
	// HasCredentials says the stored address carries a username or password
	// that Addr above does not show. A page that edits everything else about
	// the outbound must send KeepCredentials back, or the write would save
	// the redacted address and break the outbound at the next start.
	HasCredentials bool `json:"has_credentials,omitempty"`
	// KeepCredentials is a request field: keep whatever address is stored
	// for this name instead of taking Addr.
	KeepCredentials bool `json:"keep_credentials,omitempty"`
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
	// Referrers lists, per outbound or group name, the remotes that select it
	// with `proxy:`. Those live outside this section, so the page cannot
	// follow a rename into them — config.Validate refuses the whole write
	// when one is left pointing at a name that no longer exists. Naming them
	// here lets the page say what a rename is about to break while the dialog
	// is still open. It is a read-only field; a write ignores it.
	Referrers map[string][]string `json:"referrers,omitempty"`
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

// splitAddrUserinfo separates the userinfo of a proxy address from the rest,
// textually. It deliberately does not parse the address as a URL: url.Parse
// fails on a password holding a stray "%" or a space, and Outbound.URL()
// returns (nil, nil) for type "direct" by design. Both made the parse-based
// version answer "no credential here" for an address that plainly has one —
// fail-open, about a secret, on the read path. A "@" before the first "/" of
// the authority is what userinfo is, whatever the type claims.
func splitAddrUserinfo(addr string) (userinfo, rest string, ok bool) {
	scheme := ""
	if i := strings.Index(addr, "://"); i >= 0 {
		scheme, addr = addr[:i+3], addr[i+3:]
	}
	authority := addr
	if i := strings.IndexAny(addr, "/?#"); i >= 0 {
		authority = addr[:i]
	}
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return "", scheme + addr, false
	}
	return authority[:at], scheme + addr[at+1:], true
}

// redactAddr strips userinfo from a proxy address, whichever way it was
// written; the scheme and host survive so the page still shows where it goes.
func redactAddr(addr string) string {
	_, rest, ok := splitAddrUserinfo(addr)
	if !ok {
		return addr
	}
	return rest
}

// proxyUserinfo matches the userinfo of an address written into a message.
// Anchoring on "//" and stopping at the first "@" keeps the scheme and the
// host, which is what makes the message useful, and drops everything a
// password could be hiding in.
var proxyUserinfo = regexp.MustCompile(`//[^/@\s"']*@`)

// scrubProxyCredentials removes proxy userinfo from a message on its way to the
// page.
//
// The read path redacts and the write path refuses an address carrying a
// credential, but neither covers an error ABOUT an address: net/url does not
// redact, so a *url.Error prints the exact string it could not parse — and with
// keep_credentials the string being applied is the stored one, password and
// all. A password with a percent sign or a space is rejected by url.Parse,
// which is exactly the case splitAddrUserinfo exists for, so this path is
// reachable with ordinary input rather than a crafted one.
func scrubProxyCredentials(text string) string {
	return proxyUserinfo.ReplaceAllString(text, "//")
}

// localizedError is a refusal the page shows verbatim: it carries the catalog
// key and its arguments rather than a sentence, so the handler renders it in
// the reader's language. Error() exists for the Go error interface and for a
// log line; nothing shows it to a person.
type localizedError struct {
	key  string
	args []any
}

func (e *localizedError) Error() string { return fmt.Sprintf("%s %v", e.key, e.args) }

func refuse(key string, args ...any) error { return &localizedError{key: key, args: args} }

func addrHasCredentials(addr string) bool {
	_, _, ok := splitAddrUserinfo(addr)
	return ok
}

func proxyConfigView(p config.Proxy, remotes map[string]config.Remote) ProxyConfig {
	out := ProxyConfig{Outbounds: []ProxyOutbound{}, Groups: []ProxyGroup{}, Rules: p.Rules}
	for name, r := range remotes {
		if r.Proxy == "" {
			continue
		}
		if out.Referrers == nil {
			out.Referrers = map[string][]string{}
		}
		out.Referrers[r.Proxy] = append(out.Referrers[r.Proxy], name)
	}
	for _, names := range out.Referrers {
		sort.Strings(names) // map order is not an order a page should show
	}
	if out.Rules == nil {
		out.Rules = []string{}
	}
	for _, o := range p.Outbounds {
		_, withoutUserinfo, hasCredential := splitAddrUserinfo(o.Addr)
		if !hasCredential {
			withoutUserinfo = o.Addr
		}
		out.Outbounds = append(out.Outbounds, ProxyOutbound{
			Name: o.Name, Type: o.Type, Addr: withoutUserinfo,
			HasCredentials: hasCredential,
		})
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

// proxyConfigFromView converts a write request, resolving each outbound
// against what is stored today: an address that carries a credential never
// travels through this API in either direction, so the only safe ways to
// write one are to keep the stored address or to send a new address that has
// no credential in it.
func proxyConfigFromView(in ProxyConfig, current config.Proxy) (config.Proxy, error) {
	stored := map[string]config.Outbound{}
	for _, o := range current.Outbounds {
		stored[o.Name] = o
	}
	var p config.Proxy
	seen := map[string]bool{}
	for _, o := range in.Outbounds {
		// Two outbounds with one name make every lookup below ambiguous, and
		// a keep_credentials write would copy one outbound's credential onto
		// the other. The configuration has no use for the duplicate either.
		if seen[o.Name] {
			return p, refuse("err.proxy_duplicate_outbound", o.Name)
		}
		seen[o.Name] = true
		if addrHasCredentials(o.Addr) {
			return p, refuse("err.proxy_addr_credential", o.Name)
		}
		addr := o.Addr
		typ := o.Type
		was, known := stored[o.Name]
		hadCredential := known && addrHasCredentials(was.Addr)
		redacted := ""
		if hadCredential {
			redacted = redactAddr(was.Addr)
		}
		switch {
		case o.KeepCredentials && !hadCredential:
			// Nothing stored under this name has a credential to keep. The
			// usual way to get here is a rename, where the page carried the
			// flag over with the new name: honouring it would write the
			// redacted address it was shown and lose the password.
			return p, refuse("err.proxy_keep_unknown", o.Name)
		case o.KeepCredentials && o.Type != was.Type:
			// The type decides whether this outbound proxies at all, and the
			// stored address is only meaningful under the type it was
			// written for. Changing both at once is how a credential gets
			// laundered into a "direct" outbound and read back out.
			return p, refuse("err.proxy_keep_type", o.Name)
		case o.KeepCredentials && addr != "" && addr != redacted:
			// Keep the stored one, or replace it. Both at once is a
			// contradiction, and silently taking the stored one discards an
			// edit somebody made on purpose.
			return p, refuse("err.proxy_keep_and_addr", o.Name)
		case o.KeepCredentials:
			addr, typ = was.Addr, was.Type
		case hadCredential && addr == redacted:
			// The page sent back what it was shown. Saving that would drop
			// the credential without anyone deciding to. This is a sentence a
			// person acts on, so it carries its own key and is rendered in
			// the request's language at the boundary.
			return p, refuse("err.proxy_redacted_addr", o.Name)
		}
		p.Outbounds = append(p.Outbounds, config.Outbound{Name: o.Name, Type: typ, Addr: addr})
	}
	for _, g := range in.Groups {
		cg := config.Group{Name: g.Name, Type: g.Type, Members: g.Members, CheckURL: g.CheckURL}
		if g.Interval != "" {
			d, err := time.ParseDuration(g.Interval)
			if err != nil || d < 0 {
				return p, refuse("err.proxy_group_interval", g.Name)
			}
			cg.Interval = d
		}
		if g.Timeout != "" {
			d, err := time.ParseDuration(g.Timeout)
			if err != nil || d < 0 {
				return p, refuse("err.proxy_group_timeout", g.Name)
			}
			cg.Timeout = d
		}
		if g.CheckURL != "" {
			if u, err := url.Parse(g.CheckURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return p, refuse("err.proxy_group_checkurl", g.Name)
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
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	host := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("host")))
	if host == "" || len(host) > 253 || strings.ContainsAny(host, " /\\\x00") {
		httpErrorT(w, r, http.StatusBadRequest, "err.host_required")
		return
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if s.collector.Proxy == nil {
		httpErrorT(w, r, http.StatusNotImplemented, "err.no_proxy_manager")
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
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	if s.collector.Proxy == nil {
		httpErrorT(w, r, http.StatusNotImplemented, "err.no_proxy_manager")
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
	cfg := s.collector.ConfigView()
	switch r.Method {
	case http.MethodGet:
		if cfg == nil {
			writeJSON(w, proxyConfigView(config.Proxy{}, nil))
			return
		}
		writeJSON(w, proxyConfigView(cfg.Proxy, cfg.Remotes))
	case http.MethodPut:
		if cfg == nil || cfg.SourcePath == "" {
			httpErrorT(w, r, http.StatusConflict, "err.no_config")
			return
		}
		var in ProxyConfig
		if !decodeMutation(w, r, &in) {
			return
		}
		p, err := proxyConfigFromView(in, cfg.Proxy)
		if err != nil {
			var refused *localizedError
			if errors.As(err, &refused) {
				httpErrorT(w, r, http.StatusBadRequest, refused.key, refused.args...)
				return
			}
			http.Error(w, scrubProxyCredentials(err.Error()), http.StatusBadRequest)
			return
		}
		if err := config.SetProxy(cfg.SourcePath, p); err != nil {
			// A rejected section is the request's fault; a file that cannot
			// be written is this machine's — a full disk or a read-only
			// mount is not a bad request, and answering 400 sends the person
			// looking for a mistake they did not make.
			status := http.StatusBadRequest
			if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.ENOSPC) || errors.Is(err, fs.ErrNotExist) {
				status = http.StatusInternalServerError
			}
			http.Error(w, scrubProxyCredentials(err.Error()), status)
			return
		}
		s.reloadConfigView()
		out := ProxyMutationResponse{RestartRequired: true}
		if s.collector.ReloadProxy != nil {
			if err := s.collector.ReloadProxy(p); err != nil {
				// Saved but not live: say exactly that, never "applied". The
				// page prints this verbatim, so it is rendered in the
				// request's language; the daemon's own reason is passed
				// through untranslated, as every other pass-through is.
				out.Warning = i18n.T(LangFrom(r), "proxy.saved_not_applied", scrubProxyCredentials(err.Error()))
			} else {
				out.Applied, out.RestartRequired = true, false
			}
		}
		writeJSON(w, out)
	default:
		allowMethod(w, r, http.MethodGet, http.MethodPut)
	}
}
