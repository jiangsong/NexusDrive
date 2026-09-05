package netem

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// echoServer answers each line with the same line.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

func newProxy(t *testing.T) *Proxy {
	t.Helper()
	p, err := Listen("127.0.0.1:0", echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func roundTrip(t *testing.T, addr, msg string) (string, time.Duration) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	start := time.Now()
	if _, err := io.WriteString(c, msg+"\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(line), time.Since(start)
}

func TestTransparentByDefault(t *testing.T) {
	p := newProxy(t)
	got, rtt := roundTrip(t, p.Addr(), "hello")
	if got != "hello" {
		t.Fatalf("echo = %q", got)
	}
	if rtt > 200*time.Millisecond {
		t.Fatalf("an unimpaired proxy took %s", rtt)
	}
	// The downstream byte count is added after the write that answered us,
	// so it can trail the reply by a moment.
	deadline := time.Now().Add(time.Second)
	for {
		st := p.Stats()
		if st.Connections == 1 && st.BytesUp > 0 && st.BytesDown > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stats = %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestLatencyIsPaidPerDirection is the property that makes the emulation
// honest: a round trip costs twice the one-way latency, no more.
func TestLatencyIsPaidPerDirection(t *testing.T) {
	p := newProxy(t)
	p.Set(Config{Latency: 40 * time.Millisecond})
	_, rtt := roundTrip(t, p.Addr(), "ping")
	if rtt < 80*time.Millisecond {
		t.Fatalf("round trip %s is below the 80 ms two-way latency", rtt)
	}
	if rtt > 300*time.Millisecond {
		t.Fatalf("round trip %s is far above the configured latency; chunks are not pipelined", rtt)
	}
}

// TestLatencyDoesNotSerialiseThroughput: with latency in place, a stream of
// many chunks must not pay the delay once per chunk.
func TestLatencyDoesNotSerialiseThroughput(t *testing.T) {
	p := newProxy(t)
	p.Set(Config{Latency: 30 * time.Millisecond})
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	payload := make([]byte, 2<<20) // 64 chunks of 32 KiB
	start := time.Now()
	go func() { c.Write(payload) }()
	if _, err := io.ReadFull(c, make([]byte, len(payload))); err != nil {
		t.Fatal(err)
	}
	el := time.Since(start)
	// 64 chunks × 60 ms would be almost 4 s if serialised.
	if el > 1500*time.Millisecond {
		t.Fatalf("2 MiB took %s under 30 ms latency; delay is being paid per chunk", el)
	}
}

func TestStallCountsAndDelays(t *testing.T) {
	p := newProxy(t)
	p.Set(Config{StallProb: 1, StallFor: 60 * time.Millisecond})
	_, rtt := roundTrip(t, p.Addr(), "x")
	if rtt < 120*time.Millisecond {
		t.Fatalf("a certain stall in both directions should cost ≥120 ms, took %s", rtt)
	}
	if p.Stats().Stalls < 2 {
		t.Fatalf("stalls = %d", p.Stats().Stalls)
	}
}

func TestResetTearsDownTheConnection(t *testing.T) {
	p := newProxy(t)
	p.Set(Config{ResetProb: 1})
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "boom\n")
	if _, err := bufio.NewReader(c).ReadString('\n'); err == nil {
		t.Fatal("a reset connection should not answer")
	}
	if p.Stats().Resets == 0 {
		t.Fatal("reset was not counted")
	}
}

func TestCutRefusesAndThenRecovers(t *testing.T) {
	p := newProxy(t)
	p.Cut(300 * time.Millisecond)
	c, err := net.Dial("tcp", p.Addr())
	if err == nil {
		c.SetDeadline(time.Now().Add(2 * time.Second))
		io.WriteString(c, "hello\n")
		if _, err := bufio.NewReader(c).ReadString('\n'); err == nil {
			t.Fatal("a cut proxy answered")
		}
		c.Close()
	}
	if !p.Stats().Cut {
		t.Fatal("stats should report the cut")
	}
	time.Sleep(350 * time.Millisecond)
	if got, _ := roundTrip(t, p.Addr(), "back"); got != "back" {
		t.Fatalf("after the cut expires the link should work, got %q", got)
	}
}

func TestBandwidthCapSlowsTransfer(t *testing.T) {
	p := newProxy(t)
	p.Set(Config{BandwidthBps: 1 << 20}) // 1 MiB/s
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	payload := make([]byte, 512<<10)
	start := time.Now()
	go func() { c.Write(payload) }()
	if _, err := io.ReadFull(c, make([]byte, len(payload))); err != nil {
		t.Fatal(err)
	}
	// 512 KiB there and back at 1 MiB/s per direction is about a second.
	if el := time.Since(start); el < 400*time.Millisecond {
		t.Fatalf("512 KiB round trip took %s under a 1 MiB/s cap", el)
	}
}

func TestControlEndpoint(t *testing.T) {
	p := newProxy(t)
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/set?latency=25ms&jitter=5ms&stall_prob=0.01&bandwidth=20mbit", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set: %s", resp.Status)
	}
	cfg := p.Get()
	if cfg.Latency != 25*time.Millisecond || cfg.Jitter != 5*time.Millisecond || cfg.StallProb != 0.01 {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.BandwidthBps != 2_500_000 {
		t.Fatalf("20mbit = %d B/s, want 2500000", cfg.BandwidthBps)
	}
	if cfg.StallFor == 0 {
		t.Fatal("a stall probability without a duration should get the default")
	}
	// GET on a mutating endpoint is refused.
	if resp, _ := http.Get(srv.URL + "/set?latency=1s"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /set = %s", resp.Status)
	}
	if resp, _ := http.Post(srv.URL+"/set?stall_prob=7", "", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("out-of-range probability = %s", resp.Status)
	}
	http.Post(srv.URL+"/clear", "", nil)
	if p.Get() != (Config{}) {
		t.Fatal("clear did not reset the config")
	}
	resp, _ = http.Post(srv.URL+"/cut?for=50ms", "", nil)
	resp.Body.Close()
	if !p.Stats().Cut {
		t.Fatal("cut via HTTP did not take effect")
	}
}

func TestParseBandwidth(t *testing.T) {
	cases := map[string]int64{"20mbit": 2_500_000, "1gbit": 125_000_000, "800kbit": 100_000, "4MiB": 4 << 20, "1024": 1024}
	for in, want := range cases {
		got, err := ParseBandwidth(in)
		if err != nil || got != want {
			t.Errorf("%s = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseBandwidth("fast"); err == nil {
		t.Fatal("nonsense should be rejected")
	}
}
