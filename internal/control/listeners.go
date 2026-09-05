package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Running owns the bound endpoints and their lifetime. Binding happens before
// Start returns, so a bad address cannot silently leave a daemon unmanageable.
type Running struct {
	servers   []*http.Server
	listeners []net.Listener
	lock      *os.File
	once      sync.Once
	done      chan struct{}
}

func (r *Running) Close() error {
	r.once.Do(func() {
		close(r.done)
		for _, s := range r.servers {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.Shutdown(ctx)
			cancel()
			_ = s.Close()
		}
		for _, l := range r.listeners {
			_ = l.Close()
		}
		if r.lock != nil {
			_ = r.lock.Close()
		}
	})
	return nil
}

// Start serves the same handlers on a private Unix socket and optional
// loopback TCP endpoint. A flock serializes stale-socket recovery and startup.
func (s *Server) Start(ctx context.Context, socket, tcp string) (_ *Running, err error) {
	r := &Running{done: make(chan struct{})}
	defer func() {
		if err != nil {
			r.Close()
		}
	}()
	if socket != "" {
		if err = os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
			return nil, err
		}
		fd, e := unix.Open(socket+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
		if e != nil {
			return nil, fmt.Errorf("control: socket lock: %w", e)
		}
		r.lock = os.NewFile(uintptr(fd), socket+".lock")
		if e = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); e != nil {
			return nil, fmt.Errorf("control: socket %s already owned: %w", socket, e)
		}
		if st, e := os.Lstat(socket); e == nil {
			if st.Mode()&os.ModeSocket == 0 {
				return nil, fmt.Errorf("control: refusing to replace non-socket %s", socket)
			}
			conn, e := net.DialTimeout("unix", socket, 200*time.Millisecond)
			if e == nil {
				conn.Close()
				return nil, fmt.Errorf("control: socket %s is active", socket)
			}
			if !errors.Is(e, syscall.ECONNREFUSED) && !errors.Is(e, os.ErrNotExist) {
				return nil, e
			}
			if e = os.Remove(socket); e != nil {
				return nil, e
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return nil, e
		}
		l, e := net.Listen("unix", socket)
		if e != nil {
			return nil, e
		}
		r.listeners = append(r.listeners, l)
		if e = os.Chmod(socket, 0600); e != nil {
			return nil, e
		}
	}
	if tcp != "" {
		if !loopbackAddr(tcp) {
			return nil, fmt.Errorf("control: TCP address must be loopback: %s", tcp)
		}
		l, e := net.Listen("tcp", tcp)
		if e != nil {
			return nil, e
		}
		r.listeners = append(r.listeners, l)
	}
	if os.Getenv("CLOUDFS_PPROF") == "1" {
		s.enablePprof()
	}
	for _, l := range r.listeners {
		srv := &http.Server{Handler: s.mux, ReadHeaderTimeout: 5 * time.Second}
		r.servers = append(r.servers, srv)
		go srv.Serve(l)
	}
	go func() {
		select {
		case <-ctx.Done():
			r.Close()
		case <-r.done:
		}
	}()
	return r, nil
}

// FetchStatus prefers the daemon's private socket, then its TCP endpoint.
// Missing listeners allow offline inspection; permissions and protocol errors
// are surfaced instead of being disguised as an offline daemon.
func FetchStatus(ctx context.Context, socket, tcp string) (Status, bool, error) {
	for _, endpoint := range []struct{ network, address string }{{"unix", socket}, {"tcp", tcp}} {
		if endpoint.address == "" {
			continue
		}
		if endpoint.network == "tcp" && !loopbackAddr(endpoint.address) {
			return Status{}, false, errors.New("control: TCP address must be loopback")
		}
		d := net.Dialer{Timeout: time.Second}
		tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, endpoint.network, endpoint.address)
		}}
		client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://cloudfs/status", nil)
		resp, err := client.Do(req)
		if err != nil {
			tr.CloseIdleConnections()
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
				continue
			}
			return Status{}, false, err
		}
		var status Status
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("control: status returned %s", resp.Status)
		} else {
			err = json.NewDecoder(resp.Body).Decode(&status)
		}
		resp.Body.Close()
		tr.CloseIdleConnections()
		return status, true, err
	}
	return Status{}, false, nil
}
