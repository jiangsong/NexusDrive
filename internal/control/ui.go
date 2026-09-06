package control

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed web
var webFS embed.FS

// asset is one embedded file, its content type, and an ETag computed once from
// its bytes. The whole binary is replaced atomically on upgrade, so a
// content-hash ETag is the whole cache story: no version query strings, no
// build step to stamp them.
type asset struct {
	body        []byte
	contentType string
	etag        string
}

// buildAssets walks the embedded tree once, at EnableUI time, into a path→asset
// map. Serving from this map rather than http.FileServer keeps the "exact path
// or 404" contract the tests lock in, and lets each response set its own
// security headers.
func buildAssets() map[string]asset {
	assets := map[string]asset{}
	_ = fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, readErr := webFS.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		rel := strings.TrimPrefix(p, "web/")
		urlPath := "/ui/" + rel
		if rel == "index.html" {
			urlPath = "/"
		}
		sum := sha256.Sum256(body)
		assets[urlPath] = asset{body: body, contentType: contentTypeFor(p), etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		return nil
	})
	return assets
}

func contentTypeFor(p string) string {
	switch path.Ext(p) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// The CSP is the same allow-list the single-file page used, tightened: scripts
// and styles now come from 'self' (the multi-file app), not 'unsafe-inline'.
// connect-src 'self' still lets the page reach the control API and the SSE
// stream on the same origin, and nothing else.
const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func (s *Server) statusUI(w http.ResponseWriter, r *http.Request) {
	a, ok := s.assets[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "use GET", http.StatusMethodNotAllowed)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.contentType)
	h.Set("Content-Security-Policy", uiCSP)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("ETag", a.etag)
	// The document is dynamic per boot (its inline nothing changes, but the
	// app it loads does); assets revalidate against the ETag, which changes
	// with any binary upgrade. Both are safe to keep out of a shared cache.
	if r.URL.Path == "/" {
		h.Set("Cache-Control", "no-store")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if match := r.Header.Get("If-None-Match"); match != "" && match == a.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(a.body)
	}
}
