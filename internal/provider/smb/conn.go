package smb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	smb2 "github.com/hirochachacha/go-smb2"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
)

// sessionOptions configures the live SMB connection.
type sessionOptions struct {
	Host   string
	Port   int
	Share  string
	Remote string

	User, Password, Domain string
	Hash                   []byte

	Dial        provider.DialFunc
	Limiters    *ratelimit.Registry
	IdleTimeout time.Duration
	// OnDrop is called whenever the mounted share is discarded, because every
	// handle opened on it died with the connection.
	OnDrop func()
}

// session dials, mounts and reuses one SMB share.
//
// The connection is established on first use rather than at construction, so
// a misconfigured remote does not stop the daemon from starting, and a server
// that is down does not block a mount of a different remote.
type session struct {
	opt  sessionOptions
	addr string

	mu       sync.Mutex
	conn     net.Conn
	smb      *smb2.Session
	share    *smb2.Share
	lastUsed time.Time
	closed   bool
}

func newSession(opt sessionOptions) *session {
	port := opt.Port
	if port == 0 {
		port = 445
	}
	if opt.IdleTimeout == 0 {
		opt.IdleTimeout = 2 * time.Minute
	}
	return &session{opt: opt, addr: net.JoinHostPort(opt.Host, strconv.Itoa(port))}
}

// use runs fn against a mounted share.
func (s *session) use(ctx context.Context, class ratelimit.Class, fn func(fileSystem) error) error {
	if s.opt.Limiters != nil {
		l := s.opt.Limiters.Limiter(ratelimit.Key{Remote: s.opt.Remote, Class: class})
		if err := l.Wait(ctx); err != nil {
			return err
		}
	}
	mount := func(ctx context.Context) (fileSystem, error) {
		share, err := s.mounted(ctx)
		if err != nil {
			return nil, err
		}
		return shareAdapter{share.WithContext(ctx)}, nil
	}
	return runWithRetry(ctx, mount, s.drop, fn)
}

// runWithRetry runs fn once and, if the connection died under it, once more on
// a fresh mount. An SMB session dies with its TCP connection, so every further
// attempt on the dead one would fail identically; the retry belongs here
// rather than in each caller. A cancelled context is the caller's decision and
// is never retried.
func runWithRetry(ctx context.Context, mount func(context.Context) (fileSystem, error), drop func(), fn func(fileSystem) error) error {
	for attempt := 0; attempt < 2; attempt++ {
		fs, err := mount(ctx)
		if err != nil {
			return err
		}
		err = fn(fs)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		if attempt == 0 && isConnectionLoss(err) {
			drop()
			continue
		}
		return err
	}
	return errors.New("smb: request failed on a fresh connection")
}

// mounted returns the live share, dialling and mounting if needed.
func (s *session) mounted(ctx context.Context) (*smb2.Share, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("smb: provider is closed")
	}
	if s.share != nil {
		if s.opt.IdleTimeout > 0 && time.Since(s.lastUsed) > s.opt.IdleTimeout {
			share, smbSession, conn := s.share, s.smb, s.conn
			s.share, s.smb, s.conn = nil, nil, nil
			s.mu.Unlock()
			s.notifyDrop()
			closeMount(share, smbSession, conn)
			s.mu.Lock()
		} else {
			share := s.share
			s.lastUsed = time.Now()
			s.mu.Unlock()
			return share, nil
		}
	}
	if s.share != nil {
		share := s.share
		s.lastUsed = time.Now()
		s.mu.Unlock()
		return share, nil
	}
	s.mu.Unlock()

	conn, smbSession, share, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		closeMount(share, smbSession, conn)
		return nil, errors.New("smb: provider is closed")
	}
	if s.share != nil {
		// Another goroutine won the race; keep its mount and drop ours rather
		// than leaving two sessions open on the server.
		existing := s.share
		s.lastUsed = time.Now()
		s.mu.Unlock()
		closeMount(share, smbSession, conn)
		return existing, nil
	}
	s.conn, s.smb, s.share, s.lastUsed = conn, smbSession, share, time.Now()
	s.mu.Unlock()
	return share, nil
}

func (s *session) dial(ctx context.Context) (net.Conn, *smb2.Session, *smb2.Share, error) {
	dial := s.opt.Dial
	if dial == nil {
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
	}
	conn, err := dial(ctx, "tcp", s.addr)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("smb: dial %s: %w", s.addr, err)
	}
	initiator := &smb2.NTLMInitiator{
		User: s.opt.User, Password: s.opt.Password,
		Domain: s.opt.Domain, Hash: s.opt.Hash,
	}
	dialer := &smb2.Dialer{Initiator: initiator}
	smbSession, err := dialer.DialContext(ctx, conn)
	if err != nil {
		conn.Close()
		// A rejected logon is an auth failure, not something to retry into a
		// lockout.
		if errors.Is(err, os.ErrPermission) {
			return nil, nil, nil, fmt.Errorf("%w: smb logon rejected: %v", provider.ErrAuth, err)
		}
		return nil, nil, nil, fmt.Errorf("smb: negotiate with %s: %w", s.addr, err)
	}
	share, err := smbSession.Mount(s.opt.Share)
	if err != nil {
		smbSession.Logoff()
		conn.Close()
		if errors.Is(err, os.ErrPermission) {
			return nil, nil, nil, fmt.Errorf("%w: smb share %q refused: %v", provider.ErrAuth, s.opt.Share, err)
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil, fmt.Errorf("%w: smb share %q not found", provider.ErrNotFound, s.opt.Share)
		}
		return nil, nil, nil, fmt.Errorf("smb: mount %q: %w", s.opt.Share, err)
	}
	return conn, smbSession, share, nil
}

// drop discards the current mount. Every handle opened on it is dead, so the
// read cache is told before anything tries to reuse one.
func (s *session) drop() {
	s.mu.Lock()
	share, smbSession, conn := s.share, s.smb, s.conn
	s.share, s.smb, s.conn = nil, nil, nil
	s.mu.Unlock()
	if share == nil && smbSession == nil && conn == nil {
		return
	}
	s.notifyDrop()
	closeMount(share, smbSession, conn)
}

func (s *session) notifyDrop() {
	if s.opt.OnDrop != nil {
		s.opt.OnDrop()
	}
}

func (s *session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	share, smbSession, conn := s.share, s.smb, s.conn
	s.share, s.smb, s.conn = nil, nil, nil
	s.mu.Unlock()
	s.notifyDrop()
	closeMount(share, smbSession, conn)
	return nil
}

func closeMount(share *smb2.Share, smbSession *smb2.Session, conn net.Conn) {
	if share != nil {
		_ = share.Umount()
	}
	if smbSession != nil {
		_ = smbSession.Logoff()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

// shareAdapter narrows *smb2.Share to the interface the driver uses. The
// wrappers exist because the library returns a concrete *smb2.File, and
// returning that through an interface-typed result would turn a nil file into
// a non-nil interface value.
type shareAdapter struct{ share *smb2.Share }

func (a shareAdapter) Stat(name string) (os.FileInfo, error) { return a.share.Stat(name) }

func (a shareAdapter) ReadDir(name string) ([]os.FileInfo, error) { return a.share.ReadDir(name) }

func (a shareAdapter) Mkdir(name string, perm os.FileMode) error { return a.share.Mkdir(name, perm) }

func (a shareAdapter) Remove(name string) error { return a.share.Remove(name) }

func (a shareAdapter) Rename(oldpath, newpath string) error { return a.share.Rename(oldpath, newpath) }

func (a shareAdapter) Open(name string) (fileHandle, error) {
	f, err := a.share.Open(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (a shareAdapter) OpenFile(name string, flag int, perm os.FileMode) (fileHandle, error) {
	f, err := a.share.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (a shareAdapter) Create(name string) (fileHandle, error) {
	f, err := a.share.Create(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

var (
	_ runner     = (*session)(nil)
	_ fileSystem = shareAdapter{}
	_ fileHandle = (*smb2.File)(nil)
)
