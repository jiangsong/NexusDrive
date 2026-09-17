package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"html"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/vfs"
)

// The LAN render page (docs/agent-first-design.md §8.1, T-55): with
// share.render.enabled a second, read-only HTTP service on
// share.render.listen serves one file per link — GET /r/<token> — for a
// person on the same network without CloudFS. The token is minted by the
// console (POST /share/render-link, behind the same guard as every
// mutating route), is random and unrelated to any provider credential,
// lives in memory only (never agent.db, never the browser's store), is
// good for one open, and expires after share.render.token_ttl. The page
// is the same renderMarkdown output as the console's, or escaped text,
// or the bytes of an image or a PDF inline; nothing else is served, no
// listing, no navigation.

// RenderLinks mints and redeems render tokens.
type RenderLinks struct {
	mu     sync.Mutex
	ttl    time.Duration
	tokens map[string]renderToken
	now    func() time.Time
}

type renderToken struct {
	path    string
	expires time.Time
}

// NewRenderLinks builds the token table.
func NewRenderLinks(ttl time.Duration) *RenderLinks {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &RenderLinks{ttl: ttl, tokens: map[string]renderToken{}, now: time.Now}
}

// Mint issues a one-time token for p.
func (l *RenderLinks) Mint(p string) (string, time.Time) {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	tok := hex.EncodeToString(buf)
	exp := l.now().Add(l.ttl)
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, v := range l.tokens {
		if l.now().After(v.expires) {
			delete(l.tokens, k)
		}
	}
	l.tokens[tok] = renderToken{path: p, expires: exp}
	return tok, exp
}

// Redeem consumes a token, returning its path; a token unknown, spent or
// expired is refused.
func (l *RenderLinks) Redeem(tok string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.tokens[tok]
	if !ok {
		return "", false
	}
	delete(l.tokens, tok)
	if l.now().After(v.expires) {
		return "", false
	}
	return v.path, true
}

// RenderLinkRequest is POST /share/render-link.
type RenderLinkRequest struct {
	Path string `json:"path"`
}

// RenderLinkResponse is the link to hand on.
type RenderLinkResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	// Once says the link is good for one open.
	Once bool `json:"once"`
}

func (s *Server) renderLink(w http.ResponseWriter, r *http.Request) {
	if !s.fsReady(w, r, http.MethodPost) {
		return
	}
	if s.collector.RenderLinks == nil || s.collector.RenderBase == "" {
		httpErrorT(w, r, http.StatusConflict, "err.render_disabled")
		return
	}
	var q RenderLinkRequest
	if !decodeMutation(w, r, &q) {
		return
	}
	p, ok := s.fsPath(w, q.Path)
	if !ok {
		return
	}
	a, err := s.collector.FS.StatPath(r.Context(), p)
	if err != nil {
		http.Error(w, err.Error(), fsStatus(err))
		return
	}
	if a.IsDir {
		http.Error(w, vfs.ErrIsDir.Error(), http.StatusBadRequest)
		return
	}
	tok, exp := s.collector.RenderLinks.Mint(p)
	writeJSON(w, RenderLinkResponse{URL: strings.TrimRight(s.collector.RenderBase, "/") + "/r/" + tok, ExpiresAt: exp, Once: true})
}

// RenderService is the LAN service.
type RenderService struct {
	links *RenderLinks
	fs    *vfs.FS
	srv   *http.Server
}

// NewRenderService builds the service over the token table and the VFS.
func NewRenderService(links *RenderLinks, fsys *vfs.FS) *RenderService {
	return &RenderService{links: links, fs: fsys}
}

// Handler serves GET /r/<token> and nothing else.
func (rs *RenderService) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/r/", rs.serve)
	return mux
}

// Start listens on addr until ctx ends.
func (rs *RenderService) Start(ctx context.Context, addr string) (string, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	rs.srv = &http.Server{Handler: rs.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = rs.srv.Serve(l) }()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = rs.srv.Shutdown(shutdownCtx)
	}()
	return l.Addr().String(), nil
}

func (rs *RenderService) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tok := strings.TrimPrefix(r.URL.Path, "/r/")
	if tok == "" || strings.Contains(tok, "/") {
		http.NotFound(w, r)
		return
	}
	p, ok := rs.links.Redeem(tok)
	if !ok {
		http.Error(w, "this link has expired or was already opened", http.StatusGone)
		return
	}
	a, err := rs.fs.StatPath(r.Context(), p)
	if err != nil || a.IsDir {
		http.NotFound(w, r)
		return
	}
	if a.Size > rawMax {
		http.Error(w, "file too large to render", http.StatusRequestEntityTooLarge)
		return
	}
	data, err := rs.fs.ReadFileRange(r.Context(), p, 0, 0)
	if err != nil {
		if errors.Is(err, vfs.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not read the file", http.StatusBadGateway)
		return
	}
	kind, _ := renderKind(p)
	name := path.Base(p)
	switch kind {
	case "image", "pdf":
		ct := "application/pdf"
		if kind == "image" {
			ct = "image/" + strings.TrimPrefix(strings.ToLower(path.Ext(p)), ".")
			if strings.HasSuffix(ct, "/jpg") {
				ct = "image/jpeg"
			}
			if strings.HasSuffix(ct, "/svg") {
				ct = "application/octet-stream"
			}
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", "inline")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		_, _ = w.Write(data)
		return
	}
	body := ""
	if kind == "markdown" && isText(data) {
		body = renderMarkdown(string(data))
	} else if isText(data) {
		body = "<pre>" + html.EscapeString(string(data)) + "</pre>"
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+strings.ReplaceAll(name, "\"", "")+"\"")
		_, _ = w.Write(data)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src https: http:; style-src 'unsafe-inline'")
	_, _ = w.Write([]byte("<!doctype html><html><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width\"><title>" + html.EscapeString(name) + "</title>" +
		"<style>body{max-width:820px;margin:32px auto;padding:0 16px;font:15px/1.6 system-ui,sans-serif;color:#1c1f26}pre{background:#f3f4f6;padding:12px;overflow:auto;border-radius:6px}code{font-family:ui-monospace,monospace;font-size:.92em}table{border-collapse:collapse}td,th{border:1px solid #ddd;padding:4px 8px}img{max-width:100%}blockquote{border-left:3px solid #ccc;margin:0;padding:0 12px;color:#555}</style></head><body>" +
		body + "<hr><p style=\"color:#888;font-size:12px\">" + html.EscapeString(name) + " · rendered by CloudFS; this link was good for one open.</p></body></html>"))
}

func isText(data []byte) bool {
	return strings.IndexByte(string(data), 0) < 0
}
