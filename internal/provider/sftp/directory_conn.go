package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
)

const directoryCloseTimeout = 2 * time.Second

// Directory connections never carry ordinary file handles. A lease has one
// exclusive subsystem and may interrupt its TCP transport at any phase. Slots
// bound both SSH connections and concurrently active directory decoders.
type directoryPool struct {
	ctx    context.Context
	cancel context.CancelFunc
	slots  chan *directoryConn
	all    []*directoryConn
}

type directoryConn struct {
	addr string
	cfg  *ssh.ClientConfig
	dial dialFunc

	mu     sync.Mutex
	raw    net.Conn
	client *ssh.Client
	closed bool
}

func newDirectoryPool(n int, addr string, cfg *ssh.ClientConfig, dial dialFunc) *directoryPool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &directoryPool{ctx: ctx, cancel: cancel, slots: make(chan *directoryConn, n)}
	for range n {
		c := &directoryConn{addr: addr, cfg: cfg, dial: dial}
		p.all = append(p.all, c)
		p.slots <- c
	}
	return p
}

func (p *directoryPool) Close() {
	p.cancel()
	for _, c := range p.all {
		c.drop(true)
	}
}

// stopDirectoryCancellation waits out a callback that already started. A
// lease must not be returned while an old cancellation can drop its next use.
func stopDirectoryCancellation(ctx context.Context, fn func()) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		fn()
	})
	return func() {
		if !stop() {
			<-done
		}
	}
}

func (p *directoryPool) scan(ctx context.Context, root, id string, visit func(provider.Entry) error, admit func(context.Context) error) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	stopShutdown := stopDirectoryCancellation(p.ctx, cancel)
	defer stopShutdown()
	defer cancel()
	if p.ctx.Err() != nil {
		return net.ErrClosed
	}
	if err := admit(ctx); err != nil {
		return err
	}
	var c *directoryConn
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return net.ErrClosed
	case c = <-p.slots:
	}
	defer func() { p.slots <- c }()
	if p.ctx.Err() != nil {
		return net.ErrClosed
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	stop := stopDirectoryCancellation(ctx, func() { c.drop(false) })
	defer func() {
		stop()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	stream, err := c.open(ctx)
	if err != nil {
		c.drop(false)
		return err
	}
	err = scanDirectoryAtRoot(ctx, stream, root, id, visit)
	if err != nil {
		// Never retry an initialized stream: even a failed final CLOSE can
		// follow callbacks already delivered to the TEMP staging collector.
		c.drop(false)
	}
	return err
}

func (c *directoryConn) drop(permanent bool) {
	c.mu.Lock()
	raw := c.raw
	c.raw, c.client = nil, nil
	c.closed = c.closed || permanent
	c.mu.Unlock()
	if raw != nil {
		// Closing TCP also interrupts handshake, channel requests and writes.
		// No SSH close packet (which could block) is needed on a failed link.
		raw.Close()
	}
}

func (c *directoryConn) connect(ctx context.Context) (*ssh.Client, net.Conn, bool, error) {
	c.mu.Lock()
	client, raw, closed := c.client, c.raw, c.closed
	c.mu.Unlock()
	if closed {
		return nil, nil, false, net.ErrClosed
	}
	if client != nil {
		return client, raw, true, nil
	}
	raw, err := c.dial(ctx, "tcp", c.addr)
	if err != nil {
		return nil, nil, false, err
	}
	c.mu.Lock()
	if c.closed || ctx.Err() != nil {
		c.mu.Unlock()
		raw.Close()
		return nil, nil, false, net.ErrClosed
	}
	c.raw = raw
	c.mu.Unlock()
	// Distinguish a local verifier's rejection from remote text, without
	// exposing banners or classifying an unknown/changed host as retryable.
	cfg := *c.cfg
	var hostKeyRejected atomic.Bool
	cfg.HostKeyCallback = func(host string, addr net.Addr, key ssh.PublicKey) error {
		err := c.cfg.HostKeyCallback(host, addr, key)
		hostKeyRejected.Store(err != nil)
		return err
	}
	sc, chans, reqs, err := ssh.NewClientConn(raw, c.addr, &cfg)
	if err != nil {
		if hostKeyRejected.Load() {
			return nil, nil, false, fmt.Errorf("%w: directory SSH host key verification failed", provider.ErrAuth)
		}
		if isAuthErr(err) {
			return nil, nil, false, fmt.Errorf("%w: directory SSH authentication failed", provider.ErrAuth)
		}
		// Do not propagate remote banners or authentication text.
		return nil, nil, false, fmt.Errorf("%w: directory SSH handshake failed", provider.ErrTransient)
	}
	client = ssh.NewClient(sc, chans, reqs)
	c.mu.Lock()
	if c.closed || ctx.Err() != nil || c.raw != raw {
		c.mu.Unlock()
		raw.Close()
		return nil, nil, false, net.ErrClosed
	}
	c.client = client
	c.mu.Unlock()
	return client, raw, false, nil
}

func (c *directoryConn) open(ctx context.Context) (stream *directoryChannel, err error) {
	timeout := c.cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	setup, cancel := context.WithTimeout(ctx, timeout)
	stop := stopDirectoryCancellation(setup, func() { c.drop(false) })
	defer cancel()
	defer func() {
		stop()
		if setup.Err() != nil {
			err = setup.Err()
			if stream != nil {
				stream.Close()
				stream = nil
			}
		}
	}()
	for attempt := 0; ; attempt++ {
		client, raw, reused, e := c.connect(setup)
		if e != nil {
			return nil, e
		}
		ch, requests, e := client.OpenChannel("session", nil)
		if e != nil {
			if reused && attempt == 0 && isConnDead(e) && setup.Err() == nil {
				c.drop(false)
				continue // No subsystem or protocol callbacks have run yet.
			}
			return nil, e
		}
		stream = newDirectoryChannel(ch, requests, raw)
		ok, e := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{"sftp"}))
		if e != nil || !ok {
			c.drop(false)
			stream.Close()
			if e == nil {
				e = fmt.Errorf("%w: SFTP subsystem refused", provider.ErrUnsupported)
			}
			return nil, e
		}
		return stream, nil
	}
}

// SSH Channel.Close only sends CLOSE. Wait for the peer to release the channel
// and its stderr/request queues before reusing the connection; a broken peer
// is bounded by a deadline and cannot leak two drain goroutines per listing.
type directoryChannel struct {
	ssh.Channel
	raw      net.Conn
	requests chan struct{}
	stderr   chan struct{}
	once     sync.Once
	err      error
}

func newDirectoryChannel(ch ssh.Channel, requests <-chan *ssh.Request, raw net.Conn) *directoryChannel {
	s := &directoryChannel{Channel: ch, raw: raw, requests: make(chan struct{}), stderr: make(chan struct{})}
	go func() {
		defer close(s.requests)
		ssh.DiscardRequests(requests)
	}()
	go func() {
		defer close(s.stderr)
		io.Copy(io.Discard, ch.Stderr())
	}()
	return s
}

func (s *directoryChannel) Close() error {
	s.once.Do(func() {
		timer := time.NewTimer(directoryCloseTimeout)
		defer timer.Stop()
		s.raw.SetWriteDeadline(time.Now().Add(directoryCloseTimeout))
		s.err = s.Channel.Close()
		for _, done := range []chan struct{}{s.requests, s.stderr} {
			select {
			case <-done:
			case <-timer.C:
				s.raw.Close()
				// Both drains are now released by the SSH receive loop.
				<-s.requests
				<-s.stderr
				return
			}
		}
		s.raw.SetWriteDeadline(time.Time{})
	})
	return s.err
}

// ListStream delivers each member once, under the shared metadata limiter.
// The injected pkg/sftp test client has no exclusive SSH transport and does
// not advertise this capability; it retains the legacy List implementation.
func (p *Provider) ListStream(ctx context.Context, dirID string, visit func(provider.Entry) error) error {
	if visit == nil {
		return errors.New("sftp: nil directory visitor")
	}
	if p.directories == nil {
		return provider.ErrUnsupported
	}
	var visitorErr error
	err := p.directories.scan(ctx, p.rootRaw, dirID, func(e provider.Entry) error {
		visitorErr = visit(e)
		return visitorErr
	}, func(ctx context.Context) error {
		if p.limiters != nil {
			return p.limiters.Limiter(ratelimit.Key{Remote: p.name, Class: ratelimit.Meta}).Wait(ctx)
		}
		return ctx.Err()
	})
	if visitorErr != nil {
		return visitorErr // Do not erase caller error identity in mapErr.
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, net.ErrClosed) {
		return err
	}
	return mapErr(err)
}

var _ provider.StreamLister = (*Provider)(nil)
