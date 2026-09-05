package sftp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	psftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
)

// The test endpoint performs real SSH encryption, password authentication,
// known_hosts verification and subsystem negotiation on a loopback TCP socket.
// Faults block specific server phases until the client closes the transport.
type directorySSHServer struct {
	root        string
	opts        Options
	phase       string
	reached     chan struct{}
	serve       func(ssh.Channel)
	connections atomic.Int64
	subsystems  atomic.Int64

	listener net.Listener
	mu       sync.Mutex
	raw      map[net.Conn]struct{}
	closed   bool
	wg       sync.WaitGroup
}

func newDirectorySSHServer(t testing.TB, phase string, serve func(ssh.Channel)) *directorySSHServer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &directorySSHServer{root: t.TempDir(), phase: phase, reached: make(chan struct{}, 32), serve: serve, listener: listener, raw: make(map[net.Conn]struct{})}
	known := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(known, []byte(knownhosts.Line([]string{"nas.test"}, signer.PublicKey())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	portNum, _ := strconv.Atoi(port)
	s.opts = Options{Name: "nas", Host: "127.0.0.1", Port: portNum, User: "tester", Password: "test-password", KnownHosts: []string{known}, HostKeyAlias: "nas.test", Root: s.root, Sessions: 1, DirectorySessions: 1}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if c.User() != "tester" || string(password) != "test-password" {
			return nil, errors.New("refused")
		}
		return nil, nil
	}}
	cfg.AddHostKey(signer)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				raw.Close()
				return
			}
			s.raw[raw] = struct{}{}
			s.mu.Unlock()
			s.connections.Add(1)
			s.wg.Add(1)
			go s.connection(raw, cfg)
		}
	}()
	t.Cleanup(func() {
		s.mu.Lock()
		s.closed = true
		listener.Close()
		for raw := range s.raw {
			raw.Close()
		}
		s.mu.Unlock()
		done := make(chan struct{})
		go func() { s.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("SSH server leaked a connection/channel")
		}
	})
	return s
}

func (s *directorySSHServer) connection(raw net.Conn, cfg *ssh.ServerConfig) {
	defer s.wg.Done()
	defer func() { raw.Close(); s.mu.Lock(); delete(s.raw, raw); s.mu.Unlock() }()
	if s.phase == "handshake" {
		s.reached <- struct{}{}
		io.Copy(io.Discard, raw)
		return
	}
	sc, channels, requests, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	done := make(chan struct{})
	s.wg.Add(2)
	go func() { defer s.wg.Done(); sc.Wait(); close(done) }()
	go func() { defer s.wg.Done(); ssh.DiscardRequests(requests) }()
	for c := range channels {
		if s.phase == "channel" {
			s.reached <- struct{}{}
			<-done
			return
		}
		if c.ChannelType() != "session" {
			c.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, reqs, err := c.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer ch.Close()
			for req := range reqs {
				var name struct{ Name string }
				if req.Type != "subsystem" || ssh.Unmarshal(req.Payload, &name) != nil || name.Name != "sftp" {
					req.Reply(false, nil)
					continue
				}
				if s.phase == "subsystem" {
					s.reached <- struct{}{}
					<-done
					return
				}
				if s.phase == "refused" {
					req.Reply(false, nil)
					return
				}
				req.Reply(true, nil)
				s.subsystems.Add(1)
				if s.serve != nil {
					s.serve(ch)
				} else {
					srv, err := psftp.NewServer(ch, psftp.WithServerWorkingDirectory(s.root))
					if err == nil {
						srv.Serve()
						srv.Close()
					}
				}
				return
			}
		}()
	}
}

func (s *directorySSHServer) provider(t testing.TB) *Provider {
	t.Helper()
	p, err := New(s.opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func awaitDirectorySignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("directory phase was not reached")
	}
}

func awaitDirectoryResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(4 * time.Second):
		t.Fatal("directory operation did not finish")
		return nil
	}
}

func TestDirectorySSHStreamsAndReusesAuthenticatedConnections(t *testing.T) {
	s := newDirectorySSHServer(t, "", nil)
	for i := range 513 {
		write(t, s.root, fmt.Sprintf("f-%04d", i), []byte("content"))
	}
	write(t, s.root, "sub/space #?%.txt", []byte("nested"))
	write(t, s.root, tempPrefix+"hidden", []byte("in flight"))
	var dials atomic.Int64
	s.opts.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" || addr != s.listener.Addr().String() {
			return nil, errors.New("incorrect injected dial target")
		}
		dials.Add(1)
		return directDial(ctx, network, addr)
	}
	for _, root := range []string{s.root, "", ".", "~", "~/sub", "sub"} {
		t.Run(root, func(t *testing.T) {
			s.opts.Root = root
			p := s.provider(t)
			if !p.Capabilities().StreamList || p.Capabilities().MaxConnsPerHost != 2 {
				t.Fatalf("incorrect capabilities: %+v", p.Capabilities())
			}
			before := dials.Load()
			for range 3 {
				n := 0
				err := p.ListStream(t.Context(), "/", func(e provider.Entry) error {
					n++
					if e.Version == "" || e.ParentID != "/" || e.ID != "/"+e.Name {
						return fmt.Errorf("incorrect entry: %+v", e)
					}
					return nil
				})
				want := 514
				if root == "sub" || root == "~/sub" {
					want = 1
				}
				if err != nil || n != want {
					t.Fatalf("entries=%d want=%d err=%v", n, want, err)
				}
			}
			if dials.Load()-before != 1 {
				t.Fatal("directory listing reauthenticated on every call")
			}
			if p.conn.sessions[0].cli != nil {
				t.Fatal("directory listing used the ordinary file connection")
			}
		})
	}
}

func TestDirectorySSHCancellationDoesNotInvalidateFileHandles(t *testing.T) {
	s := newDirectorySSHServer(t, "", nil)
	write(t, s.root, "file", []byte("unchanged"))
	p := s.provider(t)
	read := func() {
		r, err := p.ReadRange(t.Context(), "/file", "version", 0, 9)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(b) != "unchanged" {
			t.Fatalf("read=%q err=%v", b, err)
		}
	}
	read()
	opens, closes := p.HandleStats()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	entered := make(chan struct{})
	go func() {
		done <- p.ListStream(ctx, "/", func(provider.Entry) error { close(entered); <-ctx.Done(); return ctx.Err() })
	}()
	awaitDirectorySignal(t, entered)
	read()
	cancel()
	if err := awaitDirectoryResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	read()
	if gotOpens, gotCloses := p.HandleStats(); gotOpens != opens || gotCloses != closes {
		t.Fatalf("listing cancellation purged ordinary handles: %d/%d -> %d/%d", opens, closes, gotOpens, gotCloses)
	}
	if err := p.ListStream(t.Context(), "/", func(provider.Entry) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestDirectorySSHCancellationInterruptsSetupAndProtocol(t *testing.T) {
	for _, phase := range []string{"dial", "handshake", "channel", "subsystem", "init", "root", "open", "read", "stat", "close"} {
		t.Run(phase, func(t *testing.T) {
			reached := make(chan struct{}, 1)
			block := map[string]byte{"init": dirInit, "root": dirRealPath, "open": dirOpen, "read": dirRead, "stat": dirStat, "close": dirClose}[phase]
			s := newDirectorySSHServer(t, phase, func(ch ssh.Channel) {
				f := &directoryFixture{mode: "partial-attrs"}
				for {
					req, err := readDirectoryPacket(ch)
					if err != nil {
						return
					}
					if req[0] == block {
						reached <- struct{}{}
						io.Copy(io.Discard, ch)
						return
					}
					reply := f.reply(req)
					if req[0] == dirRealPath {
						reply = directoryTestNames(binary.BigEndian.Uint32(req[1:5]), directoryTestMember("/root", []byte{0, 0, 0, 0}))
					}
					if writeDirectoryPacket(ch, reply) != nil {
						return
					}
				}
			})
			if phase == "root" {
				s.opts.Root = "~"
			}
			if phase == "dial" {
				s.opts.Dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
					reached <- struct{}{}
					<-ctx.Done()
					return nil, ctx.Err()
				}
			}
			if phase == "handshake" || phase == "channel" || phase == "subsystem" {
				reached = s.reached
			}
			p := s.provider(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- p.ListStream(ctx, "/", func(provider.Entry) error { return nil }) }()
			awaitDirectorySignal(t, reached)
			cancel()
			if err := awaitDirectoryResult(t, done); !errors.Is(err, context.Canceled) {
				t.Fatalf("phase %s: %v", phase, err)
			}
		})
	}
}

func TestDirectorySSHBoundsLeasesAndClosesWaiters(t *testing.T) {
	s := newDirectorySSHServer(t, "", nil)
	write(t, s.root, "file", []byte("data"))
	s.opts.DirectorySessions = 2
	p := s.provider(t)
	entered := make(chan struct{}, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 12)
	for range 2 {
		go func() {
			done <- p.ListStream(ctx, "/", func(provider.Entry) error { entered <- struct{}{}; <-ctx.Done(); return ctx.Err() })
		}()
	}
	awaitDirectorySignal(t, entered)
	awaitDirectorySignal(t, entered)
	waitCtx, stopWait := context.WithCancel(t.Context())
	waiter := make(chan error, 1)
	go func() { waiter <- p.ListStream(waitCtx, "/", func(provider.Entry) error { return nil }) }()
	stopWait()
	if err := awaitDirectoryResult(t, waiter); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for range 10 {
		go func() {
			done <- p.ListStream(t.Context(), "/", func(provider.Entry) error { t.Error("waiter exceeded directory slots"); return nil })
		}()
	}
	p.Close()
	cancel() // User callbacks must cooperate; the provider cannot kill them.
	for range 12 {
		if err := awaitDirectoryResult(t, done); err == nil {
			t.Fatal("closed pool succeeded")
		}
	}
	if got := s.connections.Load(); got != 2 {
		t.Fatalf("directory connections=%d, want 2", got)
	}
	if err := p.ListStream(t.Context(), "/", func(provider.Entry) error { return nil }); err == nil {
		t.Fatal("closed pool reopened")
	}
}

func TestDirectorySSHRejectsAuthenticationAndSubsystemRefusal(t *testing.T) {
	for _, mode := range []string{"password", "host-key", "refused"} {
		t.Run(mode, func(t *testing.T) {
			s := newDirectorySSHServer(t, mode, nil)
			if mode == "password" {
				s.opts.Password = "wrong"
			}
			if mode == "host-key" {
				s.opts.HostKeyAlias = "unknown.test"
			}
			p := s.provider(t)
			err := p.ListStream(t.Context(), "/", func(provider.Entry) error { t.Error("unauthorized listing delivered entries"); return nil })
			if err == nil {
				t.Fatal("connection refusal was ignored")
			}
			if (mode == "password" || mode == "host-key") && !errors.Is(err, provider.ErrAuth) {
				t.Fatal(err)
			}
			if mode == "refused" && !errors.Is(err, provider.ErrUnsupported) {
				t.Fatal(err)
			}
			if s.connections.Load() != 1 {
				t.Fatal("refusal was retried")
			}
		})
	}
}

func TestDirectorySSHOldCancellationCannotCloseNextLease(t *testing.T) {
	s := newDirectorySSHServer(t, "", nil)
	write(t, s.root, "file", []byte("data"))
	p := s.provider(t)
	for range 50 {
		old, cancel := context.WithCancel(t.Context())
		if err := p.ListStream(old, "/", func(provider.Entry) error { return nil }); err != nil {
			cancel()
			t.Fatal(err)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- p.ListStream(t.Context(), "/", func(provider.Entry) error { close(entered); <-release; return nil })
		}()
		awaitDirectorySignal(t, entered)
		cancel()
		close(release)
		if err := awaitDirectoryResult(t, done); err != nil {
			t.Fatalf("old cancellation dropped next lease: %v", err)
		}
	}
	if s.connections.Load() != 1 {
		t.Fatalf("late cancellation caused reconnects: %d", s.connections.Load())
	}
}

func TestDirectorySSHReconnectsClosedIdleConnectionBeforeProtocol(t *testing.T) {
	s := newDirectorySSHServer(t, "", nil)
	write(t, s.root, "file", []byte("data"))
	p := s.provider(t)
	if err := p.ListStream(t.Context(), "/", func(provider.Entry) error { return nil }); err != nil {
		t.Fatal(err)
	}
	c := p.directories.all[0]
	c.mu.Lock()
	client, raw := c.client, c.raw
	c.mu.Unlock()
	raw.Close()
	client.Wait() // Make the dead-idle phase deterministic, not a timing race.
	n := 0
	if err := p.ListStream(t.Context(), "/", func(provider.Entry) error { n++; return nil }); err != nil || n != 1 {
		t.Fatalf("reconnect entries=%d err=%v", n, err)
	}
	if s.connections.Load() != 2 {
		t.Fatalf("connection count=%d", s.connections.Load())
	}
}

func TestDirectorySSHSetupDeadlineDoesNotLimitWholeListing(t *testing.T) {
	for _, blocked := range []bool{true, false} {
		t.Run(fmt.Sprint(blocked), func(t *testing.T) {
			phase := ""
			if blocked {
				phase = "subsystem"
			}
			s := newDirectorySSHServer(t, phase, nil)
			write(t, s.root, "file", []byte("data"))
			p := s.provider(t)
			// Change only this test's directory configuration before first use.
			cfg := *p.directories.all[0].cfg
			cfg.Timeout = 100 * time.Millisecond
			p.directories.all[0].cfg = &cfg
			err := p.ListStream(t.Context(), "/", func(provider.Entry) error { time.Sleep(150 * time.Millisecond); return nil })
			if blocked && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("setup deadline: %v", err)
			}
			if !blocked && err != nil {
				t.Fatalf("setup deadline leaked into directory IO: %v", err)
			}
		})
	}
}

func TestDirectorySSHLimiterAndFactoryConfiguration(t *testing.T) {
	s := newDirectorySSHServer(t, "", nil)
	registry := ratelimit.NewRegistry(func(ratelimit.Key) ratelimit.Options { return ratelimit.Options{Rate: 0.01, Burst: 1} }, ratelimit.BreakerOptions{})
	s.opts.Limiters = registry
	p := s.provider(t)
	if err := registry.Limiter(ratelimit.Key{Remote: "nas", Class: ratelimit.Meta}).Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := p.ListStream(ctx, "/", func(provider.Entry) error { t.Error("metadata limiter bypassed"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if s.connections.Load() != 0 {
		t.Fatal("directory connected before metadata admission")
	}
	for _, count := range []int{-1, 33} {
		if _, err := New(Options{Host: "test", DirectorySessions: count}); err == nil {
			t.Fatalf("invalid directory limit %d accepted", count)
		}
	}
	backend, err := Factory("nas", map[string]any{"host": s.listener.Addr().String(), "user": "tester", "password": "test-password", "known_hosts": s.opts.KnownHosts[0], "host_key_alias": "nas.test", "connections": 3, "directory_connections": 4})
	if err != nil {
		t.Fatal(err)
	}
	configured := backend.(*Provider)
	defer configured.Close()
	if len(configured.directories.all) != 4 || configured.Capabilities().MaxConnsPerHost != 7 {
		t.Fatalf("factory lost directory limit: %+v", configured.Capabilities())
	}
}

func TestDirectorySSHCloseInterruptsMetadataAdmission(t *testing.T) {
	s := newDirectorySSHServer(t, "", nil)
	admitting := make(chan struct{}, 1)
	registry := ratelimit.NewRegistry(func(ratelimit.Key) ratelimit.Options {
		return ratelimit.Options{Rate: 0.01, Burst: 1, Now: func() time.Time {
			select {
			case admitting <- struct{}{}:
			default:
			}
			return time.Now()
		}}
	}, ratelimit.BreakerOptions{})
	s.opts.Limiters = registry
	p := s.provider(t)
	registry.Limiter(ratelimit.Key{Remote: "nas", Class: ratelimit.Meta}).Wait(t.Context())
	<-admitting
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.ListStream(ctx, "/", func(provider.Entry) error { return nil }) }()
	awaitDirectorySignal(t, admitting)
	p.Close()
	if err := awaitDirectoryResult(t, done); err == nil {
		t.Fatal("closed provider passed metadata admission")
	}
	if s.connections.Load() != 0 {
		t.Fatal("closed provider connected after metadata admission")
	}
}

func TestDirectorySSHCloseRejectsLateDialPublication(t *testing.T) {
	s := newDirectorySSHServer(t, "", nil)
	entered, release := make(chan struct{}), make(chan struct{})
	client, server := net.Pipe()
	defer server.Close()
	raw := &directoryCloseTestConn{Conn: client, done: make(chan struct{})}
	defer raw.Close()
	s.opts.Dial = func(context.Context, string, string) (net.Conn, error) {
		close(entered)
		<-release // Deliberately returns a connection after provider shutdown.
		return raw, nil
	}
	p := s.provider(t)
	done := make(chan error, 1)
	go func() { done <- p.ListStream(t.Context(), "/", func(provider.Entry) error { return nil }) }()
	awaitDirectorySignal(t, entered)
	p.Close()
	close(release)
	if err := awaitDirectoryResult(t, done); err == nil {
		t.Fatal("late dial reopened closed provider")
	}
	awaitDirectorySignal(t, raw.done)
	c := p.directories.all[0]
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed || c.raw != nil || c.client != nil {
		t.Fatal("late connection escaped shutdown")
	}
}

func TestDirectorySSHUsesBracketedIPv6ForBothPools(t *testing.T) {
	p, err := New(Options{Host: "2001:db8::42", Port: 2222, Password: "unused", InsecureHostKey: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, c := range p.conn.sessions {
		if c.addr != "[2001:db8::42]:2222" {
			t.Fatalf("file pool IPv6 address=%q", c.addr)
		}
	}
	for _, c := range p.directories.all {
		if c.addr != "[2001:db8::42]:2222" {
			t.Fatalf("directory pool IPv6 address=%q", c.addr)
		}
	}
}

func TestDirectorySSHNeverReplaysPrefixAndPreservesVisitorError(t *testing.T) {
	for _, mode := range []string{"visitor", "transport-eof", "close-failed"} {
		t.Run(mode, func(t *testing.T) {
			s := newDirectorySSHServer(t, "", func(ch ssh.Channel) {
				f := &directoryFixture{mode: mode}
				for {
					req, err := readDirectoryPacket(ch)
					if err != nil {
						return
					}
					reply := f.reply(req)
					if len(reply) == 0 || writeDirectoryPacket(ch, reply) != nil {
						return
					}
				}
			})
			p := s.provider(t)
			visitorErr := errors.New("storage refused batch")
			n := 0
			err := p.ListStream(t.Context(), "/", func(provider.Entry) error {
				n++
				if mode == "visitor" {
					return visitorErr
				}
				return nil
			})
			if err == nil || n != 1 || s.subsystems.Load() != 1 {
				t.Fatalf("entries=%d subsystems=%d err=%v", n, s.subsystems.Load(), err)
			}
			if mode == "visitor" && !errors.Is(err, visitorErr) {
				t.Fatal("visitor error identity lost")
			}
		})
	}
}
