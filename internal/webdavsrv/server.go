// Package webdavsrv exposes a read-only VFS subtree over WebDAV. It is an
// adapter over the same VFS used by FUSE and MCP; it never resolves providers
// or cache files directly.
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

// Backend is the narrow VFS surface required by the WebDAV adapter.
type Backend interface {
	StatPath(context.Context, string) (vfs.Attr, error)
	ReadDirPath(context.Context, string) ([]vfs.Attr, error)
	Open(context.Context, uint64, bool) (*vfs.Handle, error)
	Read(context.Context, *vfs.Handle, []byte, int64) (int, error)
	Release(context.Context, *vfs.Handle) error
	DownloadURL(context.Context, string) (provider.Link, error)
}

type Options struct {
	FS       Backend
	Addr     string
	Prefix   string
	Root     string
	Token    string
	Strategy string
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

	files := &readOnlyFS{backend: opt.FS, root: opt.Root}
	dav := &webdav.Handler{
		Prefix:     opt.Prefix,
		FileSystem: files,
		LockSystem: webdav.NewMemLS(),
	}
	var handler http.Handler = readOnlyMethods(directDownloads(dav, files, opt.Prefix, opt.Strategy))
	if opt.Token != "" {
		handler = requireToken(handler, opt.Token)
	}
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

func directDownloads(next http.Handler, files *readOnlyFS, prefix, strategy string) http.Handler {
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

func readOnlyMethods(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			next.ServeHTTP(w, r)
		case http.MethodOptions:
			w.Header().Set("Allow", "OPTIONS, GET, HEAD, PROPFIND")
			w.Header().Set("DAV", "1")
			w.Header().Set("MS-Author-Via", "DAV")
			w.WriteHeader(http.StatusOK)
		case "PROPFIND":
			if depth := r.Header.Get("Depth"); depth != "0" && depth != "1" {
				http.Error(w, "PROPFIND requires Depth: 0 or 1", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Allow", "GET, HEAD, OPTIONS, PROPFIND")
			http.Error(w, "read-only WebDAV endpoint", http.StatusMethodNotAllowed)
		}
	})
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
