package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// echoServer accepts one connection and echoes what it receives, so a test can
// tell that the tunnel carries bytes both ways.
func echoServer(t *testing.T) net.Listener {
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
	return ln
}

func newManager(t *testing.T, outs []Outbound, rules []string) *Manager {
	t.Helper()
	m, err := NewManager(ManagerOptions{Outbounds: outs, Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	return m
}

func roundTrip(t *testing.T, c net.Conn, msg string) string {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, msg+"\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(line)
}

func TestDialContextDirect(t *testing.T) {
	ln := echoServer(t)
	m := newManager(t, nil, []string{"FINAL,direct"})
	c, err := m.DialContext(context.Background(), "tcp", ln.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := roundTrip(t, c, "hello"); got != "hello" {
		t.Fatalf("echo = %q", got)
	}
}

// connectProxy is a minimal HTTP proxy that only understands CONNECT.
func connectProxy(t *testing.T, greet string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				br := bufio.NewReader(client)
				line, err := br.ReadString('\n')
				if err != nil {
					client.Close()
					return
				}
				for {
					h, err := br.ReadString('\n')
					if err != nil || strings.TrimSpace(h) == "" {
						break
					}
				}
				parts := strings.Fields(line)
				if len(parts) < 2 || parts[0] != "CONNECT" {
					io.WriteString(client, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
					client.Close()
					return
				}
				up, err := net.Dial("tcp", parts[1])
				if err != nil {
					io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					client.Close()
					return
				}
				io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"+greet)
				go func() { io.Copy(up, br); up.Close() }()
				go func() { io.Copy(client, up); client.Close() }()
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

func TestDialContextThroughHTTPProxy(t *testing.T) {
	target := echoServer(t)
	px := connectProxy(t, "")
	m := newManager(t,
		[]Outbound{{Name: "px", Type: "http", Addr: px.Addr().String()}},
		[]string{"FINAL,px"})
	c, err := m.DialContext(context.Background(), "tcp", target.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := roundTrip(t, c, "through the proxy"); got != "through the proxy" {
		t.Fatalf("echo = %q", got)
	}
}

// TestDialContextKeepsBytesReadAhead covers the case where the proxy sends
// payload in the same packet as its response. Handing back the bare connection
// would drop those bytes, and an SSH banner is exactly that shape.
func TestDialContextKeepsBytesReadAhead(t *testing.T) {
	target := echoServer(t)
	px := connectProxy(t, "SSH-2.0-banner\n")
	m := newManager(t,
		[]Outbound{{Name: "px", Type: "http", Addr: px.Addr().String()}},
		[]string{"FINAL,px"})
	c, err := m.DialContext(context.Background(), "tcp", target.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "SSH-2.0-banner" {
		t.Fatalf("read-ahead bytes were lost: got %q", line)
	}
}

func TestDialContextOverrideBeatsRules(t *testing.T) {
	target := echoServer(t)
	px := connectProxy(t, "")
	m := newManager(t,
		[]Outbound{{Name: "px", Type: "http", Addr: px.Addr().String()}},
		[]string{"FINAL,direct"})
	// The rules say direct; the per-remote override must win.
	c, err := m.DialContext(context.Background(), "tcp", target.Addr().String(), "px")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := roundTrip(t, c, "override"); got != "override" {
		t.Fatalf("echo = %q", got)
	}
}

func TestDialContextRejectsBadAddress(t *testing.T) {
	m := newManager(t, nil, []string{"FINAL,direct"})
	if _, err := m.DialContext(context.Background(), "tcp", "no-port-here", ""); err == nil {
		t.Fatal("an address without a port should be rejected")
	}
}

func TestDialContextUnknownOutbound(t *testing.T) {
	// A rule naming an outbound that does not exist is refused when the manager
	// is built — at daemon start, loudly — not accepted and left to fail on the
	// first request that happens to match it (which is how an overseas drive
	// used to break quietly). NewManager and Reload validate identically.
	_, err := NewManager(ManagerOptions{Rules: []string{"FINAL,nowhere"}})
	if err == nil {
		t.Fatal("NewManager accepted a rule targeting an unknown outbound")
	}
	m := newManager(t, nil, []string{"FINAL,direct"})
	if err := m.Reload(ManagerOptions{Rules: []string{"FINAL,nowhere"}}); err == nil {
		t.Fatal("Reload accepted a rule targeting an unknown outbound")
	}
}
