// Package webdav implements the Provider interface over WebDAV. It backs NAS
// mounts, self-hosted servers and the OpenList bridge that fronts the Chinese
// drives while their native drivers mature (docs/DESIGN.md §4.1).
package webdav

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// Provider is a WebDAV backend. Paths are the identity: a WebDAV server has no
// stable per-file id, so Entry.ID holds the server-relative path.
type Provider struct {
	name   string
	base   *url.URL
	client *httpx.Client
	user   string
	pass   string
	caps   provider.Caps
	// chunked is true when the server advertises no upload session support,
	// which is every plain WebDAV server: uploads are a single PUT.
	chunked bool
}

// Options configures a Provider.
type Options struct {
	// Name is the remote name used in metadata and cache keys.
	Name string
	// BaseURL is the collection root, e.g. https://nas.local/dav.
	BaseURL string
	// User and Pass are optional basic-auth credentials.
	User, Pass string
	// Client is the shared HTTP client. Required.
	Client *httpx.Client
	// PartSize is the streaming chunk size for reads. WebDAV has no multipart
	// upload, so writes are a single PUT regardless.
	PartSize int64
}

// New builds a WebDAV provider.
func New(opt Options) (*Provider, error) {
	if opt.BaseURL == "" {
		return nil, errors.New("webdav: url is required")
	}
	if opt.Client == nil {
		return nil, errors.New("webdav: Client is required")
	}
	u, err := url.Parse(strings.TrimSuffix(opt.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("webdav: bad url %q: %w", opt.BaseURL, err)
	}
	partSize := opt.PartSize
	if partSize <= 0 {
		partSize = 8 << 20
	}
	p := &Provider{
		name: opt.Name, base: u, client: opt.Client, user: opt.User, pass: opt.Pass,
		caps: provider.Caps{
			// A plain WebDAV server exposes no content hash, so the VFS falls
			// back to size and mtime as the change fingerprint.
			HashTypes:      nil,
			RapidUpload:    nil,
			RangeRead:      true,
			StreamList:     true,
			PartSize:       partSize,
			MaxParts:       1,
			UploadParallel: 1,
			ServerMove:     true,
			ServerRename:   true,
			ServerCopy:     true,
			Delta:          false,
			LinkTTL:        0,
			// The URL needs the same credentials this process holds, so it is
			// not usable by an unrelated process.
			LinkShareable:   false,
			QPS:             provider.QPS{Meta: 8, Download: 8, Upload: 4},
			MaxConnsPerHost: 8,
			Tier:            provider.TierOfficial,
		},
	}
	return p, nil
}

// Name returns the remote name.
func (p *Provider) Name() string { return p.name }

// Capabilities returns the capability matrix.
func (p *Provider) Capabilities() provider.Caps { return p.caps }

// RootID is the id of the collection root.
const RootID = "/"

// urlFor builds the absolute URL for a server-relative path.
func (p *Provider) urlFor(id string) string {
	clean := path.Clean("/" + strings.TrimPrefix(id, "/"))
	u := *p.base
	u.Path = strings.TrimSuffix(u.Path, "/") + clean
	if clean == "/" {
		u.Path = strings.TrimSuffix(p.base.Path, "/") + "/"
	}
	return u.String()
}

func (p *Provider) header(extra map[string]string) http.Header {
	h := http.Header{}
	if p.user != "" {
		h.Set("Authorization", "Basic "+basicAuth(p.user, p.pass))
	}
	for k, v := range extra {
		h.Set(k, v)
	}
	return h
}

func basicAuth(user, pass string) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	src := []byte(user + ":" + pass)
	var out []byte
	for i := 0; i < len(src); i += 3 {
		var b [3]byte
		n := copy(b[:], src[i:])
		out = append(out,
			table[b[0]>>2],
			table[(b[0]&0x03)<<4|b[1]>>4])
		if n > 1 {
			out = append(out, table[(b[1]&0x0f)<<2|b[2]>>6])
		} else {
			out = append(out, '=')
		}
		if n > 2 {
			out = append(out, table[b[2]&0x3f])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}

// multistatus is the PROPFIND response envelope.
type multistatus struct {
	XMLName   xml.Name  `xml:"DAV: multistatus"`
	Responses []davResp `xml:"DAV: response"`
}

type davResp struct {
	Href     string     `xml:"DAV: href"`
	Propstat []propstat `xml:"DAV: propstat"`
}

type propstat struct {
	Status string `xml:"DAV: status"`
	Prop   prop   `xml:"DAV: prop"`
}

type prop struct {
	DisplayName   string       `xml:"DAV: displayname"`
	ContentLength string       `xml:"DAV: getcontentlength"`
	LastModified  string       `xml:"DAV: getlastmodified"`
	ETag          string       `xml:"DAV: getetag"`
	ResourceType  resourceType `xml:"DAV: resourcetype"`
}

type resourceType struct {
	Collection *struct{} `xml:"DAV: collection"`
}

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:">
  <d:prop>
    <d:displayname/>
    <d:getcontentlength/>
    <d:getlastmodified/>
    <d:getetag/>
    <d:resourcetype/>
  </d:prop>
</d:propfind>`

// List is the slice-returning compatibility path. VFS uses ListStream so a
// complete collection does not need to fit in memory.
func (p *Provider) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
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

// entryFrom converts one PROPFIND response element.
func (p *Provider) entryFrom(r davResp) (provider.Entry, bool, error) {
	var pr prop
	found := false
	for _, ps := range r.Propstat {
		if strings.Contains(ps.Status, " 200 ") {
			pr = ps.Prop
			found = true
			break
		}
	}
	if !found {
		return provider.Entry{}, false, nil
	}
	// Parse before decoding: unescaping first turns an encoded '?' or '#'
	// into URL syntax and decodes a literal '%2F' twice into a separator.
	u, err := url.Parse(r.Href)
	if err != nil || u.Path == "" || !strings.HasPrefix(u.Path, "/") {
		return provider.Entry{}, false, errors.New("webdav: invalid resource href")
	}
	href := u.Path
	// Strip the base path so ids are relative to the configured root.
	basePath := strings.TrimSuffix(p.base.Path, "/")
	if basePath != "" && href != basePath && !strings.HasPrefix(href, basePath+"/") {
		return provider.Entry{}, false, errors.New("webdav: resource href is outside the configured root")
	}
	rel := strings.TrimPrefix(href, basePath)
	rel = path.Clean("/" + strings.TrimSuffix(rel, "/"))

	e := provider.Entry{ID: rel, Name: path.Base(rel)}
	if pr.DisplayName != "" {
		e.Name = pr.DisplayName
	}
	if rel == "/" {
		e.Name = ""
	}
	if pr.ResourceType.Collection != nil {
		e.Kind = provider.KindDir
	} else {
		e.Kind = provider.KindFile
		if pr.ContentLength != "" {
			n, err := strconv.ParseInt(strings.TrimSpace(pr.ContentLength), 10, 64)
			if err != nil {
				return provider.Entry{}, false, fmt.Errorf("webdav: bad content length %q for %s", pr.ContentLength, rel)
			}
			e.Size = n
		}
	}
	if pr.LastModified != "" {
		if t, err := http.ParseTime(pr.LastModified); err == nil {
			e.ModTime = t
		}
	}
	e.Version = strings.Trim(pr.ETag, `"`)
	if e.Version == "" {
		// No ETag: fall back to a size+mtime fingerprint so the cache still
		// notices changes.
		e.Version = fmt.Sprintf("%d-%d", e.Size, e.ModTime.Unix())
	}
	return e, true, nil
}

// Stat returns one entry.
func (p *Provider) Stat(ctx context.Context, id string) (provider.Entry, error) {
	var ms multistatus
	err := p.client.XML(ctx, httpx.Request{
		Method: "PROPFIND",
		URL:    p.urlFor(id),
		Class:  ratelimit.Meta,
		Header: p.header(map[string]string{"Depth": "0", "Content-Type": "application/xml"}),
		Body:   strings.NewReader(propfindBody),
		GetBody: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(propfindBody)), nil
		},
		ExpectStatus: []int{http.StatusMultiStatus, http.StatusOK},
	}, &ms)
	if err != nil {
		return provider.Entry{}, err
	}
	for _, r := range ms.Responses {
		e, ok, err := p.entryFrom(r)
		if err != nil {
			return provider.Entry{}, err
		}
		if ok {
			return e, nil
		}
	}
	return provider.Entry{}, provider.ErrNotFound
}

// ReadRange fetches a byte range.
func (p *Provider) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	resp, err := p.client.Do(ctx, httpx.Request{
		Method:       http.MethodGet,
		URL:          p.urlFor(id),
		Class:        ratelimit.Download,
		Header:       p.header(map[string]string{"Range": httpx.RangeHeader(off, n)}),
		ExpectStatus: []int{http.StatusPartialContent, http.StatusOK},
		Stream:       true,
	})
	if err != nil {
		return nil, err
	}
	return httpx.RangeBody(resp, off, n)
}

// DownloadURL returns the file URL. WebDAV links need this process's
// credentials, so Caps.LinkShareable is false and callers should not hand this
// to another program.
func (p *Provider) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	return provider.Link{URL: p.urlFor(id)}, nil
}

// BeginUpload starts an upload. WebDAV has no multipart protocol and no hash
// handshake, so this is always a single PUT of one part.
func (p *Provider) BeginUpload(ctx context.Context, parentID, name string, size int64, h provider.Hashes) (provider.UploadSession, error) {
	target := path.Join(path.Clean("/"+strings.TrimPrefix(parentID, "/")), name)
	return provider.UploadSession{
		ID:       target,
		PartSize: size + 1, // one part covers the whole file
		Opaque:   map[string]string{"target": target},
	}, nil
}

// UploadPart writes the file. Only part 0 is valid because WebDAV PUT replaces
// the whole resource.
func (p *Provider) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	if idx != 0 {
		return provider.PartToken{}, fmt.Errorf("%w: webdav uploads are a single PUT, got part %d", provider.ErrUnsupported, idx)
	}
	target := s.Opaque["target"]
	if target == "" {
		target = s.ID
	}
	resp, err := p.client.Do(ctx, httpx.Request{
		Method: http.MethodPut,
		URL:    p.urlFor(target),
		Class:  ratelimit.Upload,
		Header: p.header(map[string]string{"Content-Length": strconv.FormatInt(n, 10)}),
		Body:   io.LimitReader(r, n),
		// A PUT body is a stream we cannot rewind, so it must not be retried
		// blindly; the journal retries the whole upload instead.
		ExpectStatus: []int{http.StatusCreated, http.StatusNoContent, http.StatusOK},
	})
	if err != nil {
		return provider.PartToken{}, err
	}
	return provider.PartToken{Index: 0, ETag: strings.Trim(resp.Header.Get("ETag"), `"`)}, nil
}

// CompleteUpload stats the uploaded file to return its final entry.
func (p *Provider) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	target := s.Opaque["target"]
	if target == "" {
		target = s.ID
	}
	return p.Stat(ctx, target)
}

// Mkdir creates a collection.
func (p *Provider) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	target := path.Join(path.Clean("/"+strings.TrimPrefix(parentID, "/")), name)
	_, err := p.client.Do(ctx, httpx.Request{
		Method:       "MKCOL",
		URL:          p.urlFor(target),
		Class:        ratelimit.Meta,
		Header:       p.header(nil),
		ExpectStatus: []int{http.StatusCreated, http.StatusOK},
	})
	if err != nil {
		var se *httpx.StatusError
		if errors.As(err, &se) && se.Code == http.StatusMethodNotAllowed {
			// MKCOL on an existing collection is 405.
			return provider.Entry{}, provider.ErrExists
		}
		return provider.Entry{}, err
	}
	return p.Stat(ctx, target)
}

// Rename renames within the same collection.
func (p *Provider) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	target := path.Join(path.Dir(path.Clean("/"+strings.TrimPrefix(id, "/"))), newName)
	return p.moveTo(ctx, id, target)
}

// Move relocates an entry into another collection, keeping its name.
func (p *Provider) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	clean := path.Clean("/" + strings.TrimPrefix(id, "/"))
	target := path.Join(path.Clean("/"+strings.TrimPrefix(newParentID, "/")), path.Base(clean))
	return p.moveTo(ctx, id, target)
}

func (p *Provider) moveTo(ctx context.Context, id, target string) (provider.Entry, error) {
	_, err := p.client.Do(ctx, httpx.Request{
		Method: "MOVE",
		URL:    p.urlFor(id),
		Class:  ratelimit.Meta,
		Header: p.header(map[string]string{
			"Destination": p.urlFor(target),
			// Refuse to clobber silently: the VFS decides about conflicts.
			"Overwrite": "F",
		}),
		ExpectStatus: []int{http.StatusCreated, http.StatusNoContent, http.StatusOK},
	})
	if err != nil {
		var se *httpx.StatusError
		if errors.As(err, &se) && se.Code == http.StatusPreconditionFailed {
			return provider.Entry{}, provider.ErrExists
		}
		return provider.Entry{}, err
	}
	return p.Stat(ctx, target)
}

// Copy duplicates an entry server-side.
func (p *Provider) Copy(ctx context.Context, id, newParentID, newName string) (provider.Entry, error) {
	if newName == "" {
		newName = path.Base(path.Clean("/" + strings.TrimPrefix(id, "/")))
	}
	target := path.Join(path.Clean("/"+strings.TrimPrefix(newParentID, "/")), newName)
	_, err := p.client.Do(ctx, httpx.Request{
		Method:       "COPY",
		URL:          p.urlFor(id),
		Class:        ratelimit.Meta,
		Header:       p.header(map[string]string{"Destination": p.urlFor(target), "Overwrite": "F"}),
		ExpectStatus: []int{http.StatusCreated, http.StatusNoContent, http.StatusOK},
	})
	if err != nil {
		return provider.Entry{}, err
	}
	return p.Stat(ctx, target)
}

// Delete removes an entry, or its whole subtree for a collection.
func (p *Provider) Delete(ctx context.Context, id string) error {
	_, err := p.client.Do(ctx, httpx.Request{
		Method:       http.MethodDelete,
		URL:          p.urlFor(id),
		Class:        ratelimit.Meta,
		Header:       p.header(nil),
		ExpectStatus: []int{http.StatusNoContent, http.StatusOK, http.StatusAccepted},
	})
	return err
}

var (
	_ provider.Provider     = (*Provider)(nil)
	_ provider.ServerCopier = (*Provider)(nil)
)

// Factory builds a WebDAV provider from config. It is registered under both
// "webdav" and "openlist", since the OpenList bridge speaks plain WebDAV.
func Factory(name string, cfg map[string]any) (provider.Provider, error) {
	get := func(key string) string {
		if v, ok := cfg[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}
	base := get("url")
	if base == "" {
		return nil, fmt.Errorf("webdav: remote %q is missing the required 'url' key", name)
	}
	// Prefer the daemon's client: it already applies the proxy rules, the
	// rate limiter and the circuit breaker for this remote.
	client, _ := httpx.FromConfig(cfg, httpx.Options{Remote: name, UserAgent: "cloudfs/0.1"})
	return New(Options{
		Name: name, BaseURL: base, User: get("user"), Pass: get("pass"), Client: client,
	})
}

// SetTransport lets the daemon replace the HTTP client after construction.
func (p *Provider) SetTransport(client any) {
	if c, ok := client.(*httpx.Client); ok && c != nil {
		p.client = c
	}
}

func init() {
	provider.Register("webdav", Factory)
	provider.Register("openlist", Factory)
}

// ParseTime parses a WebDAV timestamp, exported for driver tests.
func ParseTime(s string) (time.Time, error) { return http.ParseTime(s) }
