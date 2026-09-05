// Package netem is a TCP proxy that degrades the link on purpose: latency,
// jitter, stalls, resets, a bandwidth cap and outright cuts. It stands between
// cloudfs and a real backend so the WAN conditions a cloud drive actually
// runs under can be produced on a LAN without root or iptables.
//
// Loss is modelled as it is felt by a TCP stream, not as dropped bytes: a lost
// segment costs the sender a retransmit timeout, so here a "lost" chunk stalls
// the stream for a while. Dropping bytes from an SSH session would only
// corrupt it, which no real network does.
package netem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Config describes the impairment applied to each direction independently.
type Config struct {
	// Latency is added to every chunk in each direction, so the round-trip
	// time grows by twice this value. Jitter is the half-width of a uniform
	// spread around it.
	Latency time.Duration `json:"latency"`
	Jitter  time.Duration `json:"jitter"`
	// StallProb is the chance, per chunk, of a retransmit-timeout style
	// pause of StallFor.
	StallProb float64       `json:"stall_prob"`
	StallFor  time.Duration `json:"stall_for"`
	// ResetProb is the chance, per chunk, that the connection is torn down.
	ResetProb float64 `json:"reset_prob"`
	// BandwidthBps caps each direction; 0 means unlimited.
	BandwidthBps int64 `json:"bandwidth_bps"`
}

// Stats counts what the proxy did.
type Stats struct {
	Connections int64 `json:"connections"`
	Active      int64 `json:"active"`
	BytesUp     int64 `json:"bytes_up"`
	BytesDown   int64 `json:"bytes_down"`
	Stalls      int64 `json:"stalls"`
	Resets      int64 `json:"resets"`
	Refused     int64 `json:"refused"`
	Cut         bool  `json:"cut"`
}

// Proxy forwards one listening address to one target.
type Proxy struct {
	ln     net.Listener
	target string

	mu       sync.Mutex
	cfg      Config
	cutUntil time.Time
	conns    map[net.Conn]struct{}

	stats struct {
		connections, active, up, down, stalls, resets, refused atomic.Int64
	}
	rng   *rand.Rand
	rngMu sync.Mutex
	done  chan struct{}
}

// Listen starts a proxy on addr forwarding to target. It begins transparent;
// call Set to impair it.
func Listen(addr, target string) (*Proxy, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("netem: listen %s: %w", addr, err)
	}
	p := &Proxy{
		ln: ln, target: target, conns: map[net.Conn]struct{}{},
		rng: rand.New(rand.NewSource(time.Now().UnixNano())), done: make(chan struct{}),
	}
	go p.accept()
	return p, nil
}

// Addr is the address clients connect to.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Set replaces the impairment. It applies to bytes from now on; existing
// connections are kept.
func (p *Proxy) Set(cfg Config) {
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
}

// Get returns the current impairment.
func (p *Proxy) Get() Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

// Cut severs every connection and refuses new ones for d. It is the outage a
// reliability test needs: the backend is simply gone for a while.
func (p *Proxy) Cut(d time.Duration) {
	p.mu.Lock()
	p.cutUntil = time.Now().Add(d)
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

// Stats reports counters.
func (p *Proxy) Stats() Stats {
	p.mu.Lock()
	cut := time.Now().Before(p.cutUntil)
	p.mu.Unlock()
	return Stats{
		Connections: p.stats.connections.Load(), Active: p.stats.active.Load(),
		BytesUp: p.stats.up.Load(), BytesDown: p.stats.down.Load(),
		Stalls: p.stats.stalls.Load(), Resets: p.stats.resets.Load(),
		Refused: p.stats.refused.Load(), Cut: cut,
	}
}

// Close stops the proxy and drops every connection.
func (p *Proxy) Close() error {
	select {
	case <-p.done:
		return nil
	default:
		close(p.done)
	}
	err := p.ln.Close()
	p.Cut(0)
	return err
}

func (p *Proxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		cut := time.Now().Before(p.cutUntil)
		p.mu.Unlock()
		if cut {
			p.stats.refused.Add(1)
			c.Close()
			continue
		}
		go p.serve(c)
	}
}

func (p *Proxy) serve(client net.Conn) {
	p.stats.connections.Add(1)
	up, err := net.DialTimeout("tcp", p.target, 10*time.Second)
	if err != nil {
		client.Close()
		return
	}
	p.mu.Lock()
	p.conns[client] = struct{}{}
	p.conns[up] = struct{}{}
	p.mu.Unlock()
	p.stats.active.Add(1)

	kill := func() {
		client.Close()
		up.Close()
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.pipe(client, up, &p.stats.up, kill) }()
	go func() { defer wg.Done(); p.pipe(up, client, &p.stats.down, kill) }()
	wg.Wait()
	kill()
	p.mu.Lock()
	delete(p.conns, client)
	delete(p.conns, up)
	p.mu.Unlock()
	p.stats.active.Add(-1)
}

type chunk struct {
	data    []byte
	sendAt  time.Time
	stalled bool
}

// pipe copies src to dst through a delay queue. The reader never blocks on
// the delay, so latency is paid once per chunk in flight rather than once per
// chunk in sequence — which is how a real link behaves.
func (p *Proxy) pipe(src, dst net.Conn, counter *atomic.Int64, kill func()) {
	queue := make(chan chunk, 4096)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for c := range queue {
			if wait := time.Until(c.sendAt); wait > 0 {
				time.Sleep(wait)
			}
			cfg := p.Get()
			if cfg.BandwidthBps > 0 {
				// Pace to the cap: this chunk occupies the wire for its
				// serialisation time.
				time.Sleep(time.Duration(float64(len(c.data)) / float64(cfg.BandwidthBps) * float64(time.Second)))
			}
			if _, err := dst.Write(c.data); err != nil {
				kill()
				return
			}
			counter.Add(int64(len(c.data)))
		}
	}()
	// A large buffer keeps the copy off the syscall floor; the target is
	// hundreds of MB/s on the unimpaired tier.
	buf := make([]byte, 256<<10)
	var carry time.Time // keeps chunks ordered when a stall pushes one back
	for {
		n, err := src.Read(buf)
		if n > 0 {
			cfg := p.Get()
			if cfg.ResetProb > 0 && p.chance(cfg.ResetProb) {
				p.stats.resets.Add(1)
				kill()
				break
			}
			delay := cfg.Latency
			if cfg.Jitter > 0 {
				delay += time.Duration((p.uniform()*2 - 1) * float64(cfg.Jitter))
			}
			stalled := false
			if cfg.StallProb > 0 && p.chance(cfg.StallProb) {
				delay += cfg.StallFor
				stalled = true
				p.stats.stalls.Add(1)
			}
			sendAt := time.Now().Add(delay)
			if sendAt.Before(carry) {
				sendAt = carry
			}
			carry = sendAt
			data := make([]byte, n)
			copy(data, buf[:n])
			select {
			case queue <- chunk{data: data, sendAt: sendAt, stalled: stalled}:
			case <-p.done:
				err = io.EOF
			}
		}
		if err != nil {
			break
		}
	}
	close(queue)
	<-writerDone
	if tc, ok := dst.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
}

func (p *Proxy) chance(prob float64) bool { return p.uniform() < prob }

func (p *Proxy) uniform() float64 {
	p.rngMu.Lock()
	defer p.rngMu.Unlock()
	return p.rng.Float64()
}

// ServeHTTP is the control interface a script drives:
//
//	GET  /stats
//	GET  /config
//	POST /set?latency=25ms&jitter=5ms&stall_prob=0.01&stall_for=200ms&reset_prob=0&bandwidth=20mbit
//	POST /cut?for=30s
//	POST /clear
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/stats":
		json.NewEncoder(w).Encode(p.Stats())
	case "/config":
		json.NewEncoder(w).Encode(p.Get())
	case "/set":
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		cfg, err := parseConfig(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.Set(cfg)
		json.NewEncoder(w).Encode(cfg)
	case "/cut":
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		d, err := time.ParseDuration(r.URL.Query().Get("for"))
		if err != nil {
			http.Error(w, "for= must be a duration", http.StatusBadRequest)
			return
		}
		p.Cut(d)
		json.NewEncoder(w).Encode(map[string]any{"cut_for": d.String()})
	case "/clear":
		p.Set(Config{})
		json.NewEncoder(w).Encode(Config{})
	default:
		http.NotFound(w, r)
	}
}

// parseConfig reads impairment parameters from query form.
func parseConfig(q map[string][]string) (Config, error) {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	var c Config
	var err error
	dur := func(k string, dst *time.Duration) {
		if err != nil {
			return
		}
		if v := get(k); v != "" {
			*dst, err = time.ParseDuration(v)
		}
	}
	prob := func(k string, dst *float64) {
		if err != nil {
			return
		}
		if v := get(k); v != "" {
			*dst, err = strconv.ParseFloat(v, 64)
			if err == nil && (*dst < 0 || *dst > 1) {
				err = fmt.Errorf("%s must be between 0 and 1", k)
			}
		}
	}
	dur("latency", &c.Latency)
	dur("jitter", &c.Jitter)
	dur("stall_for", &c.StallFor)
	prob("stall_prob", &c.StallProb)
	prob("reset_prob", &c.ResetProb)
	if err != nil {
		return c, err
	}
	if v := get("bandwidth"); v != "" {
		c.BandwidthBps, err = ParseBandwidth(v)
		if err != nil {
			return c, err
		}
	}
	if c.StallProb > 0 && c.StallFor == 0 {
		c.StallFor = 200 * time.Millisecond
	}
	return c, nil
}

// ParseBandwidth accepts "20mbit", "1gbit", "500kbit" or a plain byte rate.
func ParseBandwidth(s string) (int64, error) {
	var n float64
	var unit string
	if _, err := fmt.Sscanf(s, "%g%s", &n, &unit); err != nil {
		if _, err2 := fmt.Sscanf(s, "%g", &n); err2 != nil {
			return 0, fmt.Errorf("bandwidth %q: %w", s, err)
		}
		unit = ""
	}
	switch unit {
	case "", "B", "bps":
		return int64(n), nil
	case "kbit":
		return int64(n * 1000 / 8), nil
	case "mbit":
		return int64(n * 1000 * 1000 / 8), nil
	case "gbit":
		return int64(n * 1000 * 1000 * 1000 / 8), nil
	case "KiB", "kB":
		return int64(n * 1024), nil
	case "MiB", "MB":
		return int64(n * 1024 * 1024), nil
	}
	return 0, fmt.Errorf("bandwidth %q: unknown unit %q", s, unit)
}

// Serve runs the control HTTP server on addr until ctx ends.
func (p *Proxy) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: p, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
