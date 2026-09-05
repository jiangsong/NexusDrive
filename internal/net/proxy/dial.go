package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// DialContext opens a raw TCP connection to addr through the outbound the
// rules select for its host, or through override when that is set.
//
// It exists because not every backend speaks HTTP. SFTP and SMB need a
// net.Conn, and routing them through a separate code path would mean the rule
// set no longer governs the whole process, which is the one property the proxy
// layer is supposed to guarantee (docs/DESIGN.md §4.2).
func (m *Manager) DialContext(ctx context.Context, network, addr, override string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("proxy: bad address %q: %w", addr, err)
	}
	name := override
	if name == "" {
		name = m.router.Outbound(Target{Host: host})
	}
	o, err := m.Resolve(name)
	if err != nil {
		return nil, err
	}
	u, err := o.URL()
	if err != nil {
		return nil, err
	}
	if u == nil {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	switch o.Type {
	case "socks5":
		var auth *xproxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &xproxy.Auth{User: u.User.Username(), Password: pw}
		}
		d, err := xproxy.SOCKS5("tcp", u.Host, auth, xproxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("proxy: socks5 %s: %w", u.Host, err)
		}
		cd, ok := d.(xproxy.ContextDialer)
		if !ok {
			return nil, errors.New("proxy: socks5 dialer does not support contexts")
		}
		return cd.DialContext(ctx, network, addr)
	case "http", "https":
		return dialCONNECT(ctx, u, network, addr)
	}
	return nil, fmt.Errorf("proxy: outbound %q has unsupported type %q for raw dialing", o.Name, o.Type)
}

// dialCONNECT tunnels a TCP connection through an HTTP proxy. An HTTP proxy
// cannot forward arbitrary bytes without it.
func dialCONNECT(ctx context.Context, proxyURL *url.URL, network, addr string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, proxyURL.Host)
	if err != nil {
		return nil, fmt.Errorf("proxy: dial %s: %w", proxyURL.Host, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: http.Header{},
	}
	if proxyURL.User != nil {
		pw, _ := proxyURL.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+basicAuth(proxyURL.User.Username(), pw))
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy: CONNECT %s: %w", addr, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy: CONNECT %s: %w", addr, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy: CONNECT %s refused: %s", addr, resp.Status)
	}
	if br.Buffered() > 0 {
		// The proxy already sent payload bytes; handing back the bare conn
		// would lose them.
		peek, _ := br.Peek(br.Buffered())
		conn = &prefixConn{Conn: conn, prefix: append([]byte(nil), peek...)}
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

// prefixConn replays bytes that were read ahead during the CONNECT handshake.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

func basicAuth(user, pass string) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	src := []byte(user + ":" + pass)
	var out []byte
	for i := 0; i < len(src); i += 3 {
		var b [3]byte
		n := copy(b[:], src[i:])
		v := uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
		out = append(out, table[v>>18&63], table[v>>12&63])
		if n > 1 {
			out = append(out, table[v>>6&63])
		} else {
			out = append(out, '=')
		}
		if n > 2 {
			out = append(out, table[v&63])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}
