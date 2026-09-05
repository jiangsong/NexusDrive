// Package sftp implements the Provider interface over SSH file transfer. It
// covers self-hosted storage — a NAS, a workstation, a rented box — which is
// the case a cloud-drive mount still has to serve well, and it is the backend
// the design calls out under generic protocols (docs/DESIGN.md §4.1).
//
// SFTP has no per-file id, no content hash and no change feed. Paths are the
// identity, and the capability matrix says so, which is what makes the VFS
// fall back to a size-and-mtime fingerprint and to TTL refresh instead of
// delta.
package sftp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	psftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
)

// RootID is the id of the configured root directory.
const RootID = "/"

// tempPrefix marks in-flight uploads. They are hidden from listings so a
// partially written file never shows up in the mount as if it were real.
const tempPrefix = ".cloudfs-upload."

// Provider is an SFTP backend.
type Provider struct {
	name string
	caps provider.Caps

	conn        *pool
	directories *directoryPool
	// cli, when set, replaces the connection entirely. Tests inject a client
	// speaking the real protocol over a pipe, with no SSH layer.
	cli *psftp.Client

	limiters *ratelimit.Registry
	// handles keeps remote file handles open across reads.
	handles *handleCache

	// root is the server-side directory the mount is rooted at. It is
	// resolved on first use because "~" and relative paths are only
	// meaningful once the session reports its working directory.
	rootMu   sync.Mutex
	rootRaw  string
	root     string
	rootDone bool
}

// Options configures a Provider.
type Options struct {
	// Name is the remote name used in metadata and cache keys.
	Name string
	// Host and Port address the SSH server. Port defaults to 22.
	Host string
	Port int
	// User defaults to the current OS user.
	User string
	// Password, KeyFiles and the ssh agent are offered in that order.
	Password      string
	KeyFiles      []string
	KeyPassphrase string
	UseAgent      bool
	// KnownHosts overrides ~/.ssh/known_hosts. InsecureHostKey disables
	// verification entirely and should only be used on a trusted link.
	KnownHosts      []string
	InsecureHostKey bool
	// HostKeyAlias verifies the server's key under this name instead of the
	// address dialled, like ssh's HostKeyAlias. It is what makes a connection
	// through a local proxy or port forward verifiable: the key on file
	// belongs to the real host, not to 127.0.0.1:2222.
	HostKeyAlias string
	// Root is the server-side directory to expose. "" and "." mean the login
	// directory; "~/x" and relative paths are resolved against it.
	Root string
	// PartSize is the upload chunk size. Concurrency bounds both the number
	// of parts uploaded at once and the in-flight requests per file.
	PartSize    int64
	Concurrency int
	// Sessions is how many SSH connections to spread requests over. One
	// connection is one cipher stream on one server core; two or more let a
	// sequential read use more of the link. Zero means 2.
	Sessions int
	// DirectorySessions bounds additional, dedicated SSH connections used for
	// cancellable directory scans. Zero means 2; maximum 32. File-transfer
	// sessions are never interrupted when a directory scan is cancelled.
	DirectorySessions int
	// PacketSize is the largest SFTP request payload. Zero picks 255 KiB
	// when the server announces itself as OpenSSH (its documented ceiling,
	// and what rclone uses) and the protocol's 32 KiB otherwise.
	PacketSize int
	// Dial opens the TCP connection. The daemon passes one that applies the
	// proxy rules; nil means dial directly.
	Dial dialFunc
	// Limiters, when set, applies the shared token buckets to this backend.
	Limiters *ratelimit.Registry
	// Client injects a ready SFTP session, for tests.
	Client *psftp.Client
	// KeepAlive is the file-connection keepalive interval. Zero selects 30s;
	// a negative value disables it. Directory connections have no keepalive.
	KeepAlive time.Duration
}

// New builds an SFTP provider. It does not connect; the first call does.
func New(opt Options) (*Provider, error) {
	if opt.DirectorySessions < 0 || opt.DirectorySessions > 32 {
		return nil, errors.New("sftp: directory_connections must be between 0 and 32")
	}
	if opt.Client == nil && opt.Host == "" {
		return nil, errors.New("sftp: host is required")
	}
	partSize := opt.PartSize
	if partSize <= 0 {
		partSize = 8 << 20
	}
	conc := opt.Concurrency
	if conc <= 0 {
		conc = 4
	}
	p := &Provider{
		name:     opt.Name,
		cli:      opt.Client,
		limiters: opt.Limiters,
		handles:  newHandleCache(),
		rootRaw:  opt.Root,
		caps: provider.Caps{
			// SFTP exposes no content hash, so the VFS uses size and mtime as
			// the change fingerprint (provider.EnsureVersion).
			HashTypes:   nil,
			RapidUpload: nil,
			RangeRead:   true,

			PartSize:       partSize,
			MaxParts:       100000,
			UploadParallel: conc,
			// Up to one part goes in a single request: create, write,
			// rename. Larger files use the resumable session.
			SinglePutMax: partSize,

			ServerMove:   true,
			ServerRename: true,
			// SSH has a copy-data extension, but it is not universal and
			// pkg/sftp does not expose it; claiming it would make the VFS
			// pick a path that fails on most servers.
			ServerCopy: false,
			Delta:      false,

			LinkTTL: 0,
			// A URL would need this process's SSH credentials, so no other
			// process can use one.
			LinkShareable: false,
			// SSH has no server-side request quota and no risk control: the
			// real limit is the connection, not a rate. Measured on a LAN,
			// holding uploads to 32/s made a 500-file batch take 74 s to
			// drain where the link needed 5 s, and 128/s capped unlinks at
			// ~170/s where the server did 1000/s; 1024 downloads/s then
			// capped cold random reads at ~920 IOPS, one token per 16 KiB
			// miss. These numbers are ceilings the link will not reach, so
			// the AIMD limiter only ever backs off when the server actually
			// pushes back.
			QPS:             provider.QPS{Meta: 8192, Download: 8192, Upload: 4096},
			MaxConnsPerHost: conc,
			Tier:            provider.TierOfficial,
		},
	}
	if opt.Client != nil {
		return p, nil
	}
	methods, err := authMethods(opt)
	if err != nil {
		return nil, err
	}
	hk, rawHostKey, err := hostKeyCallback(opt)
	if err != nil {
		return nil, err
	}
	verifyAs := ""
	if opt.HostKeyAlias != "" {
		// The alias names the real server, whose entry in known_hosts is
		// for its own port — 22 unless the alias says otherwise. The port
		// being dialled belongs to the proxy and must not leak in here.
		verifyAs = net.JoinHostPort(opt.HostKeyAlias, "22")
		if _, _, err := net.SplitHostPort(opt.HostKeyAlias); err == nil {
			verifyAs = opt.HostKeyAlias
		}
		hk = aliased(hk, verifyAs)
	}
	// Advertise the host key types known_hosts already has for this server,
	// so a host that is known does not fail as a mismatch.
	addr := net.JoinHostPort(opt.Host, strconv.Itoa(portOrDefault(opt.Port)))
	algoAddr := addr
	if verifyAs != "" {
		algoAddr = verifyAs
	}
	algos := knownHostAlgos(rawHostKey, algoAddr)

	usr := opt.User
	if usr == "" {
		usr = defaultUser()
	}
	dial := opt.Dial
	if dial == nil {
		dial = directDial
	}
	keepAlive := opt.KeepAlive
	if keepAlive == 0 {
		keepAlive = 30 * time.Second
	}
	sessions := opt.Sessions
	if sessions <= 0 {
		sessions = 2
	}
	directorySessions := opt.DirectorySessions
	if directorySessions == 0 {
		directorySessions = 2
	}
	p.caps.MaxConnsPerHost = sessions + directorySessions
	p.caps.StreamList = true
	cfg := &ssh.ClientConfig{
		User: usr, Auth: methods, HostKeyCallback: hk,
		HostKeyAlgorithms: algos, Timeout: 15 * time.Second,
	}
	p.directories = newDirectoryPool(directorySessions, addr, cfg, dial)
	p.conn = &pool{}
	for i := 0; i < sessions; i++ {
		p.conn.sessions = append(p.conn.sessions, &conn{
			addr:    addr,
			cfg:     cfg,
			dial:    dial,
			keepAlv: keepAlive,
			packet:  opt.PacketSize,
			opts: []psftp.ClientOption{
				// Several requests in flight are what make SFTP reach line
				// rate; the defaults are tuned for interactive use. The
				// packet size is chosen per connection once the server has
				// said who it is.
				psftp.MaxConcurrentRequestsPerFile(conc * 16),
				psftp.UseConcurrentReads(true),
				psftp.UseConcurrentWrites(true),
			},
			onDrop: p.handles.purge,
		})
	}
	return p, nil
}

// Name returns the remote name.
func (p *Provider) Name() string { return p.name }

// Capabilities returns the capability matrix.
func (p *Provider) Capabilities() provider.Caps { return p.caps }

// Close releases the cached handles and the SSH session.
func (p *Provider) Close() error {
	if p.directories != nil {
		p.directories.Close()
	}
	p.handles.closeAll()
	if p.conn != nil {
		return p.conn.Close()
	}
	return nil
}

// HandleStats reports remote handles opened and closed by the read path.
func (p *Provider) HandleStats() (opens, closes int64) { return p.handles.stats() }

// do runs fn against a live session, applying the rate limiter for class.
func (p *Provider) do(ctx context.Context, class ratelimit.Class, fn func(*psftp.Client) error) error {
	return p.doSized(ctx, class, 0, fn)
}

// doSized is do for a request moving size bytes: small ones stay on the
// first session, large ones are spread over the pool.
func (p *Provider) doSized(ctx context.Context, class ratelimit.Class, size int, fn func(*psftp.Client) error) error {
	if p.limiters != nil {
		l := p.limiters.Limiter(ratelimit.Key{Remote: p.name, Class: class})
		if err := l.Wait(ctx); err != nil {
			return err
		}
	}
	if p.cli != nil {
		return fn(p.cli)
	}
	if size > 0 && size <= smallRead {
		return p.conn.doFirst(ctx, fn)
	}
	return p.conn.do(ctx, fn)
}

// smallRead is the largest read that stays on the first session.
const smallRead = 256 << 10

// resolveRoot turns the configured root into an absolute server path.
func (p *Provider) resolveRoot(ctx context.Context) (string, error) {
	p.rootMu.Lock()
	defer p.rootMu.Unlock()
	if p.rootDone {
		return p.root, nil
	}
	raw := p.rootRaw
	if strings.HasPrefix(raw, "/") {
		p.root, p.rootDone = path.Clean(raw), true
		return p.root, nil
	}
	var wd string
	err := p.do(ctx, ratelimit.Meta, func(c *psftp.Client) error {
		var e error
		wd, e = c.Getwd()
		return e
	})
	if err != nil {
		return "", mapErr(err)
	}
	rel := strings.TrimPrefix(raw, "~/")
	rel = strings.TrimPrefix(rel, "~")
	if rel == "" || rel == "." {
		p.root = path.Clean(wd)
	} else {
		p.root = path.Join(wd, rel)
	}
	p.rootDone = true
	return p.root, nil
}

// abs maps an id to the absolute server path.
func (p *Provider) abs(ctx context.Context, id string) (string, error) {
	root, err := p.resolveRoot(ctx)
	if err != nil {
		return "", err
	}
	clean := path.Clean("/" + strings.TrimPrefix(id, "/"))
	if clean == "/" {
		return root, nil
	}
	return root + clean, nil
}

// List returns the children of a directory. SFTP has no pagination, so the
// cursor is always empty.
func (p *Provider) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	if p.directories != nil {
		var out []provider.Entry
		err := p.ListStream(ctx, dirID, func(e provider.Entry) error {
			out = append(out, e)
			return nil
		})
		if err != nil {
			return nil, "", err
		}
		return out, "", nil
	}
	abs, err := p.abs(ctx, dirID)
	if err != nil {
		return nil, "", err
	}
	var infos []os.FileInfo
	err = p.do(ctx, ratelimit.Meta, func(c *psftp.Client) error {
		var e error
		infos, e = c.ReadDir(abs)
		return e
	})
	if err != nil {
		return nil, "", mapErr(err)
	}
	parent := path.Clean("/" + strings.TrimPrefix(dirID, "/"))
	out := make([]provider.Entry, 0, len(infos))
	for _, fi := range infos {
		if strings.HasPrefix(fi.Name(), tempPrefix) {
			continue // an upload of ours that has not been renamed into place
		}
		out = append(out, entryFrom(parent, fi))
	}
	return out, "", nil
}

// Stat returns one entry.
func (p *Provider) Stat(ctx context.Context, id string) (provider.Entry, error) {
	abs, err := p.abs(ctx, id)
	if err != nil {
		return provider.Entry{}, err
	}
	var fi os.FileInfo
	err = p.do(ctx, ratelimit.Meta, func(c *psftp.Client) error {
		var e error
		fi, e = c.Stat(abs)
		return e
	})
	if err != nil {
		return provider.Entry{}, mapErr(err)
	}
	clean := path.Clean("/" + strings.TrimPrefix(id, "/"))
	return entryFrom(path.Dir(clean), fi), nil
}

// ReadRange returns n bytes from off. n <= 0 means to the end of the file.
// It is ReadRangeAt with a buffer of its own; callers that can supply the
// buffer should, and save a copy.
func (p *Provider) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	want := n
	if want <= 0 {
		abs, err := p.abs(ctx, id)
		if err != nil {
			return nil, err
		}
		var size int64
		err = p.do(ctx, ratelimit.Meta, func(c *psftp.Client) error {
			fi, e := c.Stat(abs)
			if e != nil {
				return e
			}
			size = fi.Size()
			return nil
		})
		if err != nil {
			return nil, mapErr(err)
		}
		want = size - off
		if want < 0 {
			want = 0
		}
	}
	buf := make([]byte, want)
	got, err := p.ReadRangeAt(ctx, id, version, off, buf)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(buf[:got])), nil
}

// ReadRangeAt fills buf from off on a cached handle. pkg/sftp splits a
// request larger than the packet size into concurrent READs, so a 4 MiB
// block is one pipelined burst rather than 128 round trips, and a 16 KiB
// sub-block is exactly one. A short answer is followed up, because a server
// that caps requests below our packet size answers with fewer bytes and
// pkg/sftp reports that as EOF; only a zero-length answer is the end.
func (p *Provider) ReadRangeAt(ctx context.Context, id, version string, off int64, buf []byte) (int, error) {
	abs, err := p.abs(ctx, id)
	if err != nil {
		return 0, err
	}
	got := 0
	err = p.doSized(ctx, ratelimit.Download, len(buf), func(c *psftp.Client) error {
		got = 0
		ch, e := p.leaseHandle(c, abs, version)
		if e != nil {
			return e
		}
		defer p.handles.release(ch)
		for got < len(buf) {
			r, e := ch.f.ReadAt(buf[got:], off+int64(got))
			got += r
			if e != nil {
				if errors.Is(e, io.EOF) {
					if r == 0 {
						break // the file really ends here
					}
					continue // short answer: ask again for the rest
				}
				return e
			}
			if r == 0 {
				break
			}
		}
		return nil
	})
	if err != nil {
		return 0, mapErr(err)
	}
	return got, nil
}

// leaseHandle returns an open handle on abs for the given version, opening
// one if the cache has none.
func (p *Provider) leaseHandle(c *psftp.Client, abs, version string) (*cachedHandle, error) {
	// A handle belongs to the session that opened it, so the key carries
	// the client: with several sessions the same path has one per session.
	key := handleKey{path: abs, version: version, cli: c}
	if ch := p.handles.lease(key); ch != nil {
		return ch, nil
	}
	f, err := c.Open(abs)
	if err != nil {
		return nil, err
	}
	return p.handles.add(key, f), nil
}

// DownloadURL is not available: an SFTP path is only reachable with this
// process's credentials.
func (p *Provider) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	return provider.Link{}, fmt.Errorf("%w: sftp has no shareable download url", provider.ErrUnsupported)
}

// BeginUpload creates the staging file the parts are written into. The final
// name only appears once every part is in place, so a reader never sees a
// half-written file.
func (p *Provider) BeginUpload(ctx context.Context, parentID, name string, size int64, h provider.Hashes) (provider.UploadSession, error) {
	parent := path.Clean("/" + strings.TrimPrefix(parentID, "/"))
	target := path.Join(parent, name)
	tempName := fmt.Sprintf("%s%s.%d", tempPrefix, name, time.Now().UnixNano())
	temp := path.Join(parent, tempName)
	absTemp, err := p.abs(ctx, temp)
	if err != nil {
		return provider.UploadSession{}, err
	}
	err = p.do(ctx, ratelimit.Upload, func(c *psftp.Client) error {
		f, e := c.Create(absTemp)
		if e != nil {
			return e
		}
		return f.Close()
	})
	if err != nil {
		return provider.UploadSession{}, mapErr(err)
	}
	return provider.UploadSession{
		ID:       temp,
		PartSize: p.caps.PartSize,
		Opaque: map[string]string{
			"temp":   temp,
			"target": target,
			"size":   strconv.FormatInt(size, 10),
		},
	}, nil
}

// UploadPart writes one chunk at its own offset, so parts may be sent in any
// order and a resumed upload can skip the ones already recorded.
func (p *Provider) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	temp := s.Opaque["temp"]
	if temp == "" {
		return provider.PartToken{}, fmt.Errorf("%w: upload session has no staging path", provider.ErrConflict)
	}
	abs, err := p.abs(ctx, temp)
	if err != nil {
		return provider.PartToken{}, err
	}
	partSize := s.PartSize
	if partSize <= 0 {
		partSize = p.caps.PartSize
	}
	off := int64(idx) * partSize
	var written int64
	err = p.do(ctx, ratelimit.Upload, func(c *psftp.Client) error {
		f, e := c.OpenFile(abs, os.O_WRONLY)
		if e != nil {
			return e
		}
		defer f.Close()
		if _, e := f.Seek(off, io.SeekStart); e != nil {
			return e
		}
		written, e = io.Copy(f, r)
		return e
	})
	if err != nil {
		return provider.PartToken{}, mapErr(err)
	}
	// A short copy that is not reported as an error would otherwise be
	// completed into a truncated file that looks successful.
	if n > 0 && written != n {
		return provider.PartToken{}, fmt.Errorf("%w: part %d wrote %d of %d bytes",
			provider.ErrTransient, idx, written, n)
	}
	return provider.PartToken{Index: idx, ETag: strconv.FormatInt(written, 10)}, nil
}

// CompleteUpload verifies the staged size and moves it into place atomically.
func (p *Provider) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	temp, target := s.Opaque["temp"], s.Opaque["target"]
	if temp == "" || target == "" {
		return provider.Entry{}, fmt.Errorf("%w: upload session is missing its paths", provider.ErrConflict)
	}
	absTemp, err := p.abs(ctx, temp)
	if err != nil {
		return provider.Entry{}, err
	}
	absTarget, err := p.abs(ctx, target)
	if err != nil {
		return provider.Entry{}, err
	}
	want, _ := strconv.ParseInt(s.Opaque["size"], 10, 64)
	// A cached read handle would keep serving the file being replaced, and
	// some servers refuse to rename over an open file.
	p.handles.evictPath(absTarget)
	var fi os.FileInfo
	err = p.do(ctx, ratelimit.Upload, func(c *psftp.Client) error {
		st, e := c.Stat(absTemp)
		if e != nil {
			return e
		}
		if want > 0 && st.Size() != want {
			return fmt.Errorf("%w: staged file is %d bytes, expected %d",
				provider.ErrTransient, st.Size(), want)
		}
		// PosixRename replaces the destination atomically. Servers without
		// the extension need the destination removed first, which opens a
		// window where the name does not exist; there is no way around it.
		if e := c.PosixRename(absTemp, absTarget); e != nil {
			if e2 := c.Remove(absTarget); e2 != nil && !isNotExist(e2) {
				return e
			}
			if e2 := c.Rename(absTemp, absTarget); e2 != nil {
				return e2
			}
		}
		fi, e = c.Stat(absTarget)
		return e
	})
	if err != nil {
		return provider.Entry{}, mapErr(err)
	}
	return entryFrom(path.Dir(path.Clean("/"+strings.TrimPrefix(target, "/"))), fi), nil
}

// PutFile stores a small file in one go, through the same staging-and-rename
// path CompleteUpload uses, so a reader never sees a half-written file.
func (p *Provider) PutFile(ctx context.Context, parentID, name string, r io.Reader, size int64, h provider.Hashes) (provider.Entry, error) {
	parent := path.Clean("/" + strings.TrimPrefix(parentID, "/"))
	target := path.Join(parent, name)
	temp := path.Join(parent, fmt.Sprintf("%s%s.%d", tempPrefix, name, time.Now().UnixNano()))
	absTemp, err := p.abs(ctx, temp)
	if err != nil {
		return provider.Entry{}, err
	}
	absTarget, err := p.abs(ctx, target)
	if err != nil {
		return provider.Entry{}, err
	}
	p.handles.evictPath(absTarget)
	var fi os.FileInfo
	err = p.do(ctx, ratelimit.Upload, func(c *psftp.Client) error {
		f, e := c.Create(absTemp)
		if e != nil {
			return e
		}
		written, e := io.Copy(f, r)
		if cerr := f.Close(); e == nil {
			e = cerr
		}
		if e != nil {
			c.Remove(absTemp)
			return e
		}
		if written != size {
			c.Remove(absTemp)
			return fmt.Errorf("%w: wrote %d of %d bytes", provider.ErrTransient, written, size)
		}
		if e := c.PosixRename(absTemp, absTarget); e != nil {
			if e2 := c.Remove(absTarget); e2 != nil && !isNotExist(e2) {
				c.Remove(absTemp)
				return e
			}
			if e2 := c.Rename(absTemp, absTarget); e2 != nil {
				c.Remove(absTemp)
				return e2
			}
		}
		fi, e = c.Stat(absTarget)
		return e
	})
	if err != nil {
		return provider.Entry{}, mapErr(err)
	}
	return entryFrom(parent, fi), nil
}

// AbortUpload removes the staging file of an upload that will not complete.
func (p *Provider) AbortUpload(ctx context.Context, s provider.UploadSession) error {
	temp := s.Opaque["temp"]
	if temp == "" {
		return nil
	}
	abs, err := p.abs(ctx, temp)
	if err != nil {
		return err
	}
	err = p.do(ctx, ratelimit.Upload, func(c *psftp.Client) error {
		e := c.Remove(abs)
		if isNotExist(e) {
			return nil
		}
		return e
	})
	return mapErr(err)
}

// Mkdir creates a directory.
func (p *Provider) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	parent := path.Clean("/" + strings.TrimPrefix(parentID, "/"))
	id := path.Join(parent, name)
	abs, err := p.abs(ctx, id)
	if err != nil {
		return provider.Entry{}, err
	}
	var fi os.FileInfo
	err = p.do(ctx, ratelimit.Meta, func(c *psftp.Client) error {
		if e := c.Mkdir(abs); e != nil {
			if isExist(e) {
				return fmt.Errorf("%w: %s", provider.ErrExists, id)
			}
			return e
		}
		var e error
		fi, e = c.Stat(abs)
		return e
	})
	if err != nil {
		return provider.Entry{}, mapErr(err)
	}
	return entryFrom(parent, fi), nil
}

// Rename changes the name of an entry within its directory.
func (p *Provider) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	clean := path.Clean("/" + strings.TrimPrefix(id, "/"))
	return p.moveTo(ctx, id, path.Join(path.Dir(clean), newName))
}

// Move relocates an entry into another directory, keeping its name.
func (p *Provider) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	clean := path.Clean("/" + strings.TrimPrefix(id, "/"))
	parent := path.Clean("/" + strings.TrimPrefix(newParentID, "/"))
	return p.moveTo(ctx, id, path.Join(parent, path.Base(clean)))
}

func (p *Provider) moveTo(ctx context.Context, id, target string) (provider.Entry, error) {
	absSrc, err := p.abs(ctx, id)
	if err != nil {
		return provider.Entry{}, err
	}
	absDst, err := p.abs(ctx, target)
	if err != nil {
		return provider.Entry{}, err
	}
	p.handles.evictPath(absSrc)
	p.handles.evictPath(absDst)
	var fi os.FileInfo
	err = p.do(ctx, ratelimit.Meta, func(c *psftp.Client) error {
		if e := c.PosixRename(absSrc, absDst); e != nil {
			if e2 := c.Rename(absSrc, absDst); e2 != nil {
				return e2
			}
		}
		var e error
		fi, e = c.Stat(absDst)
		return e
	})
	if err != nil {
		return provider.Entry{}, mapErr(err)
	}
	return entryFrom(path.Dir(path.Clean("/"+strings.TrimPrefix(target, "/"))), fi), nil
}

// Delete removes a file, or a directory and everything under it. The VFS has
// already decided that a recursive delete was asked for.
//
// It tries the plain remove first: for a file that is one round trip, where
// stat-then-remove was two, and unlink throughput is bounded by round trips.
// Only when the server refuses is the path stat'ed to see if it is a
// directory needing the depth-first walk.
func (p *Provider) Delete(ctx context.Context, id string) error {
	abs, err := p.abs(ctx, id)
	if err != nil {
		return err
	}
	p.handles.evictPath(abs)
	err = p.do(ctx, ratelimit.Meta, func(c *psftp.Client) error {
		e := c.Remove(abs)
		if e == nil || isNotExist(e) {
			return e
		}
		fi, se := c.Stat(abs)
		if se != nil {
			return se
		}
		if !fi.IsDir() {
			return e
		}
		return removeTree(c, abs)
	})
	return mapErr(err)
}

// removeTree deletes a directory depth-first, since SFTP only removes empty
// directories.
func removeTree(c *psftp.Client, abs string) error {
	infos, err := c.ReadDir(abs)
	if err != nil {
		return err
	}
	for _, fi := range infos {
		child := abs + "/" + fi.Name()
		if fi.IsDir() {
			if err := removeTree(c, child); err != nil {
				return err
			}
			continue
		}
		if err := c.Remove(child); err != nil && !isNotExist(err) {
			return err
		}
	}
	return c.RemoveDirectory(abs)
}

// entryFrom converts a stat result into an Entry.
func entryFrom(parent string, fi os.FileInfo) provider.Entry {
	kind := provider.KindFile
	if fi.IsDir() {
		kind = provider.KindDir
	}
	id := path.Join(parent, fi.Name())
	if parent == "" {
		id = "/" + fi.Name()
	}
	e := provider.Entry{
		ID:       id,
		ParentID: parent,
		Name:     fi.Name(),
		Kind:     kind,
		Size:     fi.Size(),
		ModTime:  fi.ModTime(),
	}
	// SFTP reports no change token, so the fingerprint is size and mtime.
	provider.EnsureVersion(&e)
	return e
}

func isNotExist(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	var se *psftp.StatusError
	if errors.As(err, &se) {
		return se.Code == 2 // SSH_FX_NO_SUCH_FILE
	}
	return strings.Contains(err.Error(), "does not exist") ||
		strings.Contains(err.Error(), "no such file")
}

func isExist(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrExist) {
		return true
	}
	var se *psftp.StatusError
	if errors.As(err, &se) && se.Code == 11 { // SSH_FX_FILE_ALREADY_EXISTS
		return true
	}
	// Version 3 of the protocol, which OpenSSH speaks, has no dedicated code
	// for this: it answers SSH_FX_FAILURE and puts the reason in the message.
	msg := err.Error()
	return strings.Contains(msg, "file exists") || strings.Contains(msg, "already exists")
}

// mapErr translates SFTP failures into the sentinels internal/net/retry
// classifies, so the upload queue and the VFS behave the same for every
// backend.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, provider.ErrNotFound) || errors.Is(err, provider.ErrExists) ||
		errors.Is(err, provider.ErrTransient) || errors.Is(err, provider.ErrAuth) ||
		errors.Is(err, provider.ErrConflict) || errors.Is(err, provider.ErrUnsupported) {
		return err
	}
	if isNotExist(err) {
		return fmt.Errorf("%w: %v", provider.ErrNotFound, err)
	}
	if isExist(err) {
		return fmt.Errorf("%w: %v", provider.ErrExists, err)
	}
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w: %v", provider.ErrAuth, err)
	}
	var se *psftp.StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case 3: // SSH_FX_PERMISSION_DENIED
			return fmt.Errorf("%w: %v", provider.ErrAuth, err)
		case 4: // SSH_FX_FAILURE, which servers also use for "not empty"
			return fmt.Errorf("%w: %v", provider.ErrConflict, err)
		case 8: // SSH_FX_OP_UNSUPPORTED
			return fmt.Errorf("%w: %v", provider.ErrUnsupported, err)
		}
		return fmt.Errorf("%w: %v", provider.ErrTransient, err)
	}
	if isConnDead(err) {
		return fmt.Errorf("%w: %v", provider.ErrTransient, err)
	}
	return err
}

// aliased makes a host key callback check the key under alias.
func aliased(cb ssh.HostKeyCallback, alias string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		return cb(alias, remote, key)
	}
}

func portOrDefault(p int) int {
	if p == 0 {
		return 22
	}
	return p
}

var (
	_ provider.Provider      = (*Provider)(nil)
	_ provider.SinglePutter  = (*Provider)(nil)
	_ provider.RangeReaderAt = (*Provider)(nil)
)
