// Package webdavsrv exposes one VFS subtree over WebDAV. It is an adapter over
// the same VFS used by FUSE and MCP; it never resolves providers or cache
// files directly.
//
// The endpoint is read-only unless the configuration says otherwise. Writing
// is opt-in because a DAV endpoint that can delete a subtree is a different
// exposure than one that can only read it, and the difference should be a
// decision rather than a default.
package webdavsrv

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"

	"golang.org/x/net/webdav"
)

// Backend is the narrow VFS surface a read-only WebDAV endpoint requires.
type Backend interface {
	StatPath(context.Context, string) (vfs.Attr, error)
	ReadDirPath(context.Context, string) ([]vfs.Attr, error)
	Open(context.Context, uint64, bool) (*vfs.Handle, error)
	Read(context.Context, *vfs.Handle, []byte, int64) (int, error)
	Release(context.Context, *vfs.Handle) error
	HandleAttr(context.Context, *vfs.Handle) vfs.Attr
	DownloadURL(context.Context, string) (provider.Link, error)
}

// WriteBackend adds what a writable endpoint needs. *vfs.FS satisfies it; a
// read-only endpoint never holds one, so the write paths cannot be reached
// even if a method somehow got past the gate.
type WriteBackend interface {
	Backend
	Create(ctx context.Context, parent uint64, name string) (*vfs.Handle, error)
	Write(ctx context.Context, h *vfs.Handle, p []byte, off int64) (int, error)
	Truncate(ctx context.Context, h *vfs.Handle, size int64) error
	Mkdir(ctx context.Context, parent uint64, name string) (vfs.Attr, error)
	Remove(ctx context.Context, parent uint64, name string, recursive bool) error
	Rename(ctx context.Context, oldParent uint64, oldName string, newParent uint64, newName string) error
	Copy(ctx context.Context, src, dst string) (vfs.Attr, error)
}

type Options struct {
	FS       Backend
	Addr     string
	Prefix   string
	Root     string
	Token    string
	Strategy string
	// Writable turns on the mutating verbs. It requires FS to implement
	// WriteBackend; Start refuses rather than serving a half-writable
	// endpoint that fails at the first PUT.
	Writable bool
}

type Running struct {
	server *http.Server
	listen net.Listener
	done   chan struct{}
	once   sync.Once
}

func (r *Running) Addr() string { return r.listen.Addr().String() }

func (r *Running) Close() error {
	var err error
	r.once.Do(func() {
		// Stop accepting keep-alive reuse before graceful shutdown. Without
		// this, a client that has just completed a ranged response can remain
		// in StateNew briefly and consume the entire shutdown deadline.
		r.server.SetKeepAlivesEnabled(false)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = r.server.Shutdown(ctx)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		_ = r.listen.Close()
	})
	return err
}

// Start binds synchronously, so a bad or unsafe address cannot leave the
// daemon running without the configured endpoint.
func Start(ctx context.Context, opt Options) (*Running, error) {
	if opt.FS == nil {
		return nil, errors.New("webdav: VFS is required")
	}
	if opt.Addr == "" {
		return nil, errors.New("webdav: HTTP address is required")
	}
	if opt.Prefix == "" {
		opt.Prefix = "/dav"
	}
	if opt.Root == "" {
		opt.Root = "/"
	}
	if opt.Strategy == "" {
		opt.Strategy = "proxy"
	}
	if opt.Strategy != "proxy" && opt.Strategy != "redirect" && opt.Strategy != "auto" {
		return nil, errors.New("webdav: strategy must be proxy, redirect, or auto")
	}
	if !canonicalPrefix(opt.Prefix) || !canonicalRoot(opt.Root) {
		return nil, errors.New("webdav: prefix and root must be canonical absolute paths")
	}
	if len(opt.Token) > 4096 || strings.ContainsAny(opt.Token, "\x00\r\n") {
		return nil, errors.New("webdav: invalid CLOUDFS_WEBDAV_TOKEN")
	}
	if !loopbackAddr(opt.Addr) && len(opt.Token) < 16 {
		return nil, fmt.Errorf("webdav: refusing to serve %s without a CLOUDFS_WEBDAV_TOKEN of at least 16 bytes", opt.Addr)
	}

	files := &davFS{backend: opt.FS, root: opt.Root}
	if opt.Writable {
		writer, ok := opt.FS.(WriteBackend)
		if !ok {
			return nil, errors.New("webdav: this VFS cannot serve a writable endpoint")
		}
		files.writer = writer
	}
	dav := &webdav.Handler{
		Prefix:     opt.Prefix,
		FileSystem: files,
		LockSystem: webdav.NewMemLS(),
	}
	var handler http.Handler = methodGate(
		correctWriteStatus(
			checkMoveDestination(
				serverSideCopy(directDownloads(dav, files, opt.Prefix, opt.Strategy), files, opt.Prefix),
				opt.Prefix)),
		opt.Writable)
	if opt.Token != "" {
		handler = requireToken(handler, opt.Token)
	}
	handler = tagOrigin(handler)
	listener, err := net.Listen("tcp", opt.Addr)
	if err != nil {
		return nil, err
	}
	r := &Running{
		listen: listener,
		server: &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second},
		done:   make(chan struct{}),
	}
	go func() {
		defer close(r.done)
		if err := r.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = listener.Close()
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = r.Close()
		case <-r.done:
		}
	}()
	return r, nil
}

func directDownloads(next http.Handler, files *davFS, prefix, strategy string) http.Handler {
	if strategy == "proxy" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
			next.ServeHTTP(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, prefix)
		virtual, err := files.resolve(name)
		if err == nil {
			var link provider.Link
			link, err = files.backend.DownloadURL(r.Context(), virtual)
			if err == nil && len(link.Headers) == 0 && safeRedirect(link.URL) {
				w.Header().Set("Cache-Control", "private, no-store")
				http.Redirect(w, r, link.URL, http.StatusFound)
				return
			}
		}
		if strategy == "redirect" && !errors.Is(err, vfs.ErrIsDir) {
			http.Error(w, "direct download is unavailable for this file", http.StatusBadGateway)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func safeRedirect(raw string) bool {
	if len(raw) == 0 || len(raw) > 8192 || strings.ContainsAny(raw, "\x00\r\n") {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

// readMethods and writeMethods are what OPTIONS advertises and what the gate
// lets through. A verb absent from both is refused whatever the handler
// underneath would have done with it.
const (
	readMethods  = "OPTIONS, GET, HEAD, PROPFIND"
	writeMethods = readMethods + ", PUT, DELETE, MKCOL, MOVE, COPY, PROPPATCH, LOCK, UNLOCK"
)

func methodGate(next http.Handler, writable bool) http.Handler {
	allow := readMethods
	// Class 2 is the locking class. It is only claimed when LOCK is actually
	// served, because a client that sees "2" and gets 405 on LOCK is worse off
	// than one that never tried.
	davClass := "1"
	if writable {
		allow, davClass = writeMethods, "1, 2"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			next.ServeHTTP(w, r)
		case http.MethodOptions:
			w.Header().Set("Allow", allow)
			w.Header().Set("DAV", davClass)
			w.Header().Set("MS-Author-Via", "DAV")
			w.WriteHeader(http.StatusOK)
		case "PROPFIND":
			if depth := r.Header.Get("Depth"); depth != "0" && depth != "1" {
				http.Error(w, "PROPFIND requires Depth: 0 or 1", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			next.ServeHTTP(w, r)
		case http.MethodPut, http.MethodDelete, "MKCOL", "MOVE", "COPY", "PROPPATCH", "LOCK", "UNLOCK":
			if !writable {
				refuse(w, allow, "read-only WebDAV endpoint")
				return
			}
			// A PUT body is the file and is streamed; the XML bodies are not,
			// and an unbounded one would be read into memory by the handler.
			if r.Method != http.MethodPut {
				r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			}
			next.ServeHTTP(w, r)
		default:
			refuse(w, allow, "unsupported WebDAV method")
		}
	})
}

func refuse(w http.ResponseWriter, allow, reason string) {
	w.Header().Set("Allow", allow)
	http.Error(w, reason, http.StatusMethodNotAllowed)
}

func canonicalPrefix(p string) bool {
	return p != "/" && strings.HasPrefix(p, "/") && path.Clean(p) == p && !strings.ContainsAny(p, "\x00\\")
}

func canonicalRoot(p string) bool {
	return strings.HasPrefix(p, "/") && path.Clean(p) == p && !strings.ContainsAny(p, "\x00\\")
}

func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// tagOrigin marks every request as WebDAV's so the changes it makes through
// the VFS carry vfs.OriginAPI, like the other out-of-kernel adapters.
func tagOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(vfs.WithOrigin(r.Context(), "webdav")))
	})
}

func requireToken(next http.Handler, token string) http.Handler {
	expectedBearer := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorized := subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(expectedBearer)) == 1
		if user, password, ok := r.BasicAuth(); ok && subtle.ConstantTimeCompare([]byte(user), []byte("cloudfs")) == 1 && subtle.ConstantTimeCompare([]byte(password), []byte(token)) == 1 {
			authorized = true
		}
		if !authorized {
			w.Header().Set("WWW-Authenticate", `Basic realm="CloudFS WebDAV", charset="UTF-8", Bearer realm="CloudFS WebDAV"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
