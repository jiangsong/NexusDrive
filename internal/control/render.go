package control

import (
	"errors"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cloudfs/internal/agent"
	"cloudfs/internal/share"
	"cloudfs/internal/vfs"
)

// The render page and the share route (docs/agent-first-design.md §8,
// T-55, ui-plan G8). GET /fs/render?path= classifies a file and, for a
// Markdown or text one, hands the console its HTML (renderMarkdown — no
// markup from the source survives) or escaped text; an image or a PDF
// is shown by the browser from GET /fs/raw?path=, which streams the
// bytes with the file's own media type; anything else is a download.
// POST /share is the share tool's policy (internal/share) behind the
// same typed confirmation as every other irreversible route.

// RenderResponse is GET /fs/render.
type RenderResponse struct {
	Path string `json:"path"`
	// Kind is markdown | text | code | image | pdf | other.
	Kind string `json:"kind"`
	Mime string `json:"mime"`
	Size int64  `json:"size"`
	// HTML is the rendered Markdown (markdown); Text the escaped source
	// (text, code) — each at most renderMax bytes, Truncated says so.
	HTML      string `json:"html,omitempty"`
	Text      string `json:"text,omitempty"`
	Language  string `json:"language,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// Raw is the URL the page loads an image or a PDF from.
	Raw string `json:"raw,omitempty"`
	// Cached is the fraction the cache holds: the page says when showing
	// the file would download it.
	Cached    float64 `json:"cached"`
	LocalOnly bool    `json:"local_only"`
	// ConsoleURL is this file's link, to copy.
	ConsoleURL string `json:"console_url,omitempty"`
	// Share says whether the mount's provider can create public links.
	Share bool `json:"share"`
}

// renderMax bounds what one render reads and returns.
const renderMax = 4 << 20

// renderKinds maps an extension to a kind and a code language.
var codeLanguages = map[string]string{
	".go": "go", ".py": "python", ".js": "javascript", ".mjs": "javascript", ".ts": "typescript", ".json": "json", ".yaml": "yaml", ".yml": "yaml",
	".toml": "toml", ".sh": "bash", ".rs": "rust", ".c": "c", ".h": "c", ".cpp": "cpp", ".java": "java", ".rb": "ruby", ".sql": "sql", ".html": "html", ".css": "css", ".xml": "xml",
}

func renderKind(p string) (kind, lang string) {
	ext := strings.ToLower(path.Ext(p))
	switch ext {
	case ".md", ".markdown":
		return "markdown", ""
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp", ".avif":
		return "image", ""
	case ".pdf":
		return "pdf", ""
	case ".txt", ".log", ".csv", ".tsv", ".rst", ".ini", ".cfg", ".env", "":
		return "text", ""
	}
	if l, ok := codeLanguages[ext]; ok {
		return "code", l
	}
	return "other", ""
}

// GET /fs/render?path=
func (s *Server) fsRender(w http.ResponseWriter, r *http.Request) {
	if !s.fsReady(w, r, http.MethodGet) {
		return
	}
	p, ok := s.fsPath(w, r.URL.Query().Get("path"))
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
	kind, lang := renderKind(p)
	resp := RenderResponse{Path: p, Kind: kind, Size: a.Size, Language: lang, Cached: a.Cached, LocalOnly: a.LocalOnly}
	resp.Mime = mime.TypeByExtension(path.Ext(p))
	if resp.Mime == "" {
		resp.Mime = "application/octet-stream"
	}
	if cfg := s.collector.ConfigView(); cfg != nil && cfg.Share.ConsoleLinksOn() && cfg.Control.Metrics != "" {
		resp.ConsoleURL = agent.ConsoleURL("http://"+loopbackHost(cfg.Control.Metrics), p)
	}
	if t, err := s.collector.FS.ShareTargetOf(r.Context(), p); err == nil && t.Sharer != nil {
		resp.Share = true
	}
	switch kind {
	case "image", "pdf":
		resp.Raw = "/fs/raw?path=" + strings.ReplaceAll(strings.ReplaceAll(p, "%", "%25"), "&", "%26")
	case "markdown", "text", "code":
		length := a.Size
		if length > renderMax {
			length, resp.Truncated = renderMax, true
		}
		var data []byte
		if length > 0 {
			data, err = s.collector.FS.ReadFileRange(r.Context(), p, 0, length)
			if err != nil {
				http.Error(w, err.Error(), fsStatus(err))
				return
			}
		}
		if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
			resp.Kind, resp.Truncated = "other", false
			break
		}
		if kind == "markdown" {
			resp.HTML = renderMarkdown(string(data))
		} else {
			resp.Text = string(data)
		}
	}
	writeJSON(w, resp)
}

// loopbackHost renders a wildcard listen address as loopback for a link.
func loopbackHost(addr string) string {
	host, port, err := splitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host + ":" + port
}

func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", errors.New("no port")
	}
	return strings.Trim(addr[:i], "[]"), addr[i+1:], nil
}

// GET /fs/raw?path= streams a file with its media type, inline, for the
// render page's images and PDFs. Same guard as every fs route: the page
// itself is same-origin, so the browser's fetch carries the right
// Sec-Fetch-Site. Files larger than rawMax are refused: this is a viewer,
// not a download path (that is /fs/download-url).
const rawMax = 64 << 20

func (s *Server) fsRaw(w http.ResponseWriter, r *http.Request) {
	if !s.fsReady(w, r, http.MethodGet) {
		return
	}
	p, ok := s.fsPath(w, r.URL.Query().Get("path"))
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
	if a.Size > rawMax {
		httpErrorT(w, r, http.StatusRequestEntityTooLarge, "err.length_range", rawMax)
		return
	}
	data, err := s.collector.FS.ReadFileRange(r.Context(), p, 0, 0)
	if err != nil {
		http.Error(w, err.Error(), fsStatus(err))
		return
	}
	ct := mime.TypeByExtension(path.Ext(p))
	if ct == "" || strings.HasPrefix(ct, "text/html") || strings.Contains(ct, "svg") || strings.Contains(ct, "xml") {
		// Never a type the browser would execute or let script into.
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// ShareRequest is POST /share.
type ShareRequest struct {
	Path string `json:"path"`
	// ExpiresHours is the link lifetime; 0 means share.default_expiry.
	ExpiresHours int    `json:"expires_hours,omitempty"`
	Confirm      bool   `json:"confirm"`
	Force        bool   `json:"force,omitempty"`
	Code         string `json:"code,omitempty"`
}

// ShareResponse is what a share produced.
type ShareResponse struct {
	share.Result
	ConsoleURL string `json:"console_url,omitempty"`
}

func (s *Server) shareRoute(w http.ResponseWriter, r *http.Request) {
	if !s.fsReady(w, r, http.MethodPost) {
		return
	}
	var q ShareRequest
	if !decodeMutation(w, r, &q) {
		return
	}
	p, ok := s.fsPath(w, q.Path)
	if !ok {
		return
	}
	if !confirmed(w, r, q.Confirm, "confirm.share", p) {
		return
	}
	if q.ExpiresHours < 0 {
		httpErrorT(w, r, http.StatusBadRequest, "err.invalid_query_param")
		return
	}
	expires := time.Duration(q.ExpiresHours) * time.Hour
	if cfg := s.collector.ConfigView(); expires == 0 && cfg != nil {
		expires = cfg.Share.DefaultExpiry
	}
	res, err := share.Create(r.Context(), s.collector.FS, share.Request{Path: p, Expires: expires, Confirm: true, Force: q.Force, Code: q.Code})
	if err != nil {
		switch {
		case errors.Is(err, share.ErrNotSynced), errors.Is(err, share.ErrNotCached), errors.Is(err, share.ErrUnsupported):
			writeJSONStatus(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": shareCode(err)})
		case errors.Is(err, share.ErrCredentials):
			writeJSONStatus(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "credentials", "findings": res.Findings})
		default:
			http.Error(w, err.Error(), fsStatus(err))
		}
		return
	}
	out := ShareResponse{Result: res}
	if cfg := s.collector.ConfigView(); cfg != nil && cfg.Share.ConsoleLinksOn() && cfg.Control.Metrics != "" {
		out.ConsoleURL = agent.ConsoleURL("http://"+loopbackHost(cfg.Control.Metrics), p)
	}
	writeJSON(w, out)
}

func shareCode(err error) string {
	switch {
	case errors.Is(err, share.ErrNotSynced):
		return "not_synced"
	case errors.Is(err, share.ErrNotCached):
		return "not_cached"
	case errors.Is(err, share.ErrUnsupported):
		return "unsupported"
	}
	return "refused"
}
