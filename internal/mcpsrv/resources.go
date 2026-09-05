package mcpsrv

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"cloudfs/internal/vfs"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Apply after SDK handlers: v1.7.0's defaulting currently overwrites a handler's
// CacheScope with "public". Cloud contents and allowed root names are private,
// and without content subscriptions they must be revalidated on every read.
func privateResourceResponses(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		if err == nil {
			switch r := result.(type) {
			case *mcp.ReadResourceResult:
				r.Cacheable = mcp.Cacheable{TTLMs: 0, CacheScope: "private"}
			case *mcp.ListResourcesResult:
				r.Cacheable = mcp.Cacheable{TTLMs: 0, CacheScope: "private"}
			case *mcp.ListResourceTemplatesResult:
				r.Cacheable = mcp.Cacheable{TTLMs: 0, CacheScope: "private"}
			}
		}
		return result, err
	}
}

// The authority identifies a configured remote, while the path is the FULL
// virtual path. This keeps two mounts of one remote unambiguous and preserves
// the same allowlist boundary as tools. Non-host remote names use a reversible
// authority without changing configuration or exposing provider object IDs.
func resourceAuthority(remote string) string {
	if remote != "" {
		safe := true
		for _, r := range remote {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
				safe = false
				break
			}
		}
		if safe {
			return remote
		}
	}
	return "r~" + hex.EncodeToString([]byte(remote))
}

func resourceURI(remote, p string) string {
	return (&url.URL{Scheme: "cloudfs", Host: resourceAuthority(remote), Path: p}).String()
}

func within(p, root string) bool {
	return root == "/" || p == root || strings.HasPrefix(p, root+"/")
}

func (s *Server) resourceMount(p string) (vfs.Mount, bool) {
	// VFS mounts are ordered deepest first, including shadowing mounts.
	for _, m := range s.opt.FS.Mounts() {
		if within(p, m.Prefix) {
			return m, true
		}
	}
	return vfs.Mount{}, false
}

func (s *Server) registerResources() {
	roots := map[string]*mcp.Resource{}
	authorities := map[string]bool{}
	for _, m := range s.opt.FS.Mounts() {
		allowed := s.opt.Allow
		if len(allowed) == 0 {
			allowed = []string{"/"}
		}
		for _, a := range allowed {
			p := m.Prefix
			if within(a, m.Prefix) {
				p = a
			} else if !within(m.Prefix, a) {
				continue
			}
			owner, ok := s.resourceMount(p)
			if !ok || owner.Prefix != m.Prefix {
				continue
			}
			uri := resourceURI(m.Remote, p)
			roots[uri] = &mcp.Resource{URI: uri, Name: p, Title: "CloudFS " + p,
				Description: "Allowed virtual filesystem root. Read files as bounded text/blob or directories as paginated JSON. Follow cloudfs/next_uri metadata for remaining bytes; file pages may span versions."}
			authorities[resourceAuthority(m.Remote)] = true
		}
	}
	for _, r := range roots {
		s.mcp.AddResource(r, s.readResource)
	}
	for authority := range authorities {
		s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{
			URITemplate: "cloudfs://" + authority + "/{+path}{?offset,length,cursor}",
			Name:        "CloudFS paths on " + authority,
			Description: "Full virtual path, not a provider-relative path. Files accept nonnegative offset and positive length; directories accept the returned cursor. Every path is checked against the allowlist and its owning mount."}, s.readResource)
	}
}

type resourceQuery struct {
	path, remote, cursor string
	offset, length       int64
	ranged               bool
}

func (s *Server) parseResource(raw string) (resourceQuery, error) {
	var q resourceQuery
	if len(raw) > 32768 {
		return q, errors.New("resource URI exceeds length limit")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "cloudfs" || u.Opaque != "" || u.User != nil || u.Fragment != "" || u.Host == "" || u.ForceQuery {
		return q, errors.New("invalid cloudfs resource URI")
	}
	// Reject traversal rather than cleaning it into another resource. Decode
	// once only; literal percent signs in filenames must remain literal.
	p := u.Path
	if p == "" || len(p) > 4096 || !utf8.ValidString(p) || !strings.HasPrefix(p, "/") || path.Clean(p) != p || strings.ContainsAny(p, "\x00\\") {
		return q, errors.New("resource requires a canonical absolute virtual path")
	}
	if _, err := s.checkPath(p); err != nil {
		return q, mcp.ResourceNotFoundError(raw)
	}
	m, ok := s.resourceMount(p)
	if !ok || u.Host != resourceAuthority(m.Remote) {
		return q, mcp.ResourceNotFoundError(raw)
	}
	params, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return q, errors.New("invalid resource query")
	}
	q.path, q.remote, q.length = p, m.Remote, int64(s.opt.Limits.MaxBytes)
	for name, values := range params {
		if len(values) != 1 || values[0] == "" {
			return q, errors.New("empty or duplicate resource parameter")
		}
		switch name {
		case "offset", "length":
			n, err := strconv.ParseInt(values[0], 10, 64)
			if err != nil || n < 0 || name == "length" && n == 0 || strconv.FormatInt(n, 10) != values[0] {
				return q, errors.New("offset must be nonnegative and length positive decimal integers")
			}
			q.ranged = true
			if name == "offset" {
				q.offset = n
			} else {
				q.length = min(n, int64(s.opt.Limits.MaxRangeBytes))
			}
		case "cursor":
			b, err := base64.RawURLEncoding.DecodeString(values[0])
			if err != nil || len(b) == 0 || len(b) > 4096 || !utf8.Valid(b) || bytes.ContainsAny(b, "/\x00\\") || string(b) == "." || string(b) == ".." || base64.RawURLEncoding.EncodeToString(b) != values[0] {
				return q, errors.New("invalid resource directory cursor")
			}
			q.cursor = string(b)
		default:
			return q, errors.New("unknown resource parameter")
		}
	}
	if q.ranged && q.cursor != "" {
		return q, errors.New("directory cursor cannot be combined with a file range")
	}
	// Offset-only requests must still respect the range cap when customized.
	if q.ranged {
		q.length = min(q.length, int64(s.opt.Limits.MaxRangeBytes))
	}
	return q, nil
}

func resourceReadError(uri string, err error) error {
	if errors.Is(err, vfs.ErrNotFound) || errors.Is(err, vfs.ErrNotDir) {
		return mcp.ResourceNotFoundError(uri)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// Provider and local storage errors can contain credentials, signed URLs,
	// internal object IDs or cache paths. None belong in a resource response.
	return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "resource read failed; inspect local storage and remote status"}
}

func (s *Server) readResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	q, err := s.parseResource(req.Params.URI)
	if err != nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: err.Error()}
	}
	a, err := s.opt.FS.StatPath(ctx, q.path)
	if err != nil {
		return nil, resourceReadError(req.Params.URI, err)
	}
	if a.IsDir {
		if q.ranged {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "directory resources do not accept byte ranges"}
		}
		return s.readDirectoryResource(ctx, req.Params.URI, q)
	}
	if q.cursor != "" {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "file resources do not accept directory cursors"}
	}
	data, err := s.opt.FS.ReadFileRange(ctx, q.path, q.offset, q.length)
	if err != nil {
		return nil, resourceReadError(req.Params.URI, err)
	}
	meta := mcp.Meta{"cloudfs/size": a.Size, "cloudfs/offset": q.offset, "cloudfs/bytes": len(data), "cloudfs/truncated": false}
	// Compare by subtraction so a near-MaxInt64 offset never wraps.
	if q.offset < a.Size && int64(len(data)) < a.Size-q.offset && len(data) > 0 {
		next, _ := url.Parse(resourceURI(q.remote, q.path))
		next.RawQuery = url.Values{"offset": {strconv.FormatInt(q.offset+int64(len(data)), 10)}, "length": {strconv.FormatInt(q.length, 10)}}.Encode()
		meta["cloudfs/truncated"], meta["cloudfs/next_uri"] = true, next.String()
	}
	c := &mcp.ResourceContents{URI: req.Params.URI, Meta: meta}
	if len(data) > 0 && !q.ranged && utf8.Valid(data) && !bytes.ContainsRune(data, 0) {
		c.Text, c.MIMEType = string(data), "text/plain; charset=utf-8"
	} else {
		// Explicit byte ranges and split UTF-8 code points are byte-exact.
		// A non-nil empty slice also puts blob:"" on the wire for empty files.
		c.Blob, c.MIMEType = append([]byte{}, data...), "application/octet-stream"
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{c}}, nil
}

type directoryResourceEntry struct {
	Name string `json:"name"`
	URI  string `json:"uri"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}

type directoryResource struct {
	Path      string                   `json:"path"`
	Entries   []directoryResourceEntry `json:"entries"`
	Truncated bool                     `json:"truncated"`
	NextURI   string                   `json:"next_uri,omitempty"`
}

func (s *Server) readDirectoryResource(ctx context.Context, uri string, q resourceQuery) (*mcp.ReadResourceResult, error) {
	out := directoryResource{Path: q.path, Entries: []directoryResourceEntry{}}
	entryBytes := 0
	after := q.cursor
	done := false
	for !done {
		// Fetch fixed-size windows, even when the configured response budget
		// is large. Never sort/materialize the entire cached directory.
		page, err := s.opt.FS.ReadDirPagePath(ctx, q.path, vfs.DirectoryPageOptions{After: after, Limit: min(128, s.opt.Limits.MaxEntries)})
		if err != nil {
			return nil, resourceReadError(uri, err)
		}
		for _, a := range page.Entries {
			after = a.Name
			p := path.Join(q.path, a.Name)
			if _, err := s.checkPath(p); err != nil {
				continue
			}
			m, ok := s.resourceMount(p)
			if !ok {
				continue
			}
			kind := "file"
			if a.IsDir {
				kind = "directory"
			}
			entry := directoryResourceEntry{Name: a.Name, URI: resourceURI(m.Remote, p), Kind: kind, Size: a.Size}
			// Reserve the continuation URI even on the last page, so trimming
			// to the byte budget cannot produce a larger continuation response.
			next, _ := url.Parse(resourceURI(q.remote, q.path))
			next.RawQuery = url.Values{"cursor": {base64.RawURLEncoding.EncodeToString([]byte(a.Name))}}.Encode()
			// Budget exact JSON bytes without serializing all previous entries
			// again for every candidate (quadratic work on large pages).
			envelope, _ := json.Marshal(directoryResource{Path: q.path, Entries: []directoryResourceEntry{}, Truncated: true, NextURI: next.String()})
			encoded, _ := json.Marshal(entry)
			candidateBytes := len(envelope) + entryBytes + len(encoded) + len(out.Entries)
			if len(out.Entries) >= s.opt.Limits.MaxEntries || candidateBytes > s.opt.Limits.MaxBytes {
				if len(out.Entries) == 0 {
					return nil, errors.New("directory entry exceeds resource byte limit; use list_directory")
				}
				done = true
				break
			}
			out.Entries = append(out.Entries, entry)
			entryBytes += len(encoded)
			out.NextURI, out.Truncated = next.String(), true
		}
		if !done && !page.HasMore {
			out.NextURI, out.Truncated = "", false
			done = true
		}
	}
	body, _ := json.Marshal(out)
	if len(body) > s.opt.Limits.MaxBytes {
		return nil, fmt.Errorf("directory response exceeds resource byte limit (%d)", s.opt.Limits.MaxBytes)
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "application/json", Text: string(body)}}}, nil
}
