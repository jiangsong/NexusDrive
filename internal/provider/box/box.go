// Package box implements Box through its Content API v2.
//
// Box keeps files and folders in separate id namespaces: item "12345" can be
// both a file and a folder, and the endpoint differs by type. Provider ids are
// opaque to the layers above, so this driver carries the type in the id
// ("f:12345" / "d:12345") rather than guessing an endpoint or spending a
// request to find out which one a bare id names.
//
// Uploads are content-addressed by Box: a chunked commit is accepted only with
// the SHA-1 of the whole file, which is why the driver advertises HashSHA1 and
// refuses to open a session without it.
package box

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

const (
	// RootID is Box's "All Files" folder, which is always id 0.
	RootID = "d:0"

	defaultAPIBase    = "https://api.box.com/2.0"
	defaultUploadBase = "https://upload.box.com/api/2.0"
	defaultTokenURL   = "https://api.box.com/oauth2/token"

	// Box refuses an upload session below 20 MB and refuses a simple upload
	// above 50 MB, so the boundary between the two paths is fixed by the API,
	// not chosen here.
	sessionMinSize  = 20 << 20
	singlePutMax    = sessionMinSize
	maxBoxFileSize  = 150 << 30
	maxJSONResponse = 16 << 20
	maxIDLength     = 64
	maxNameLength   = 255
	listPageSize    = 1000
	maxListPages    = 10_000

	itemFields = "id,type,name,size,sha1,etag,modified_at,content_modified_at,parent,file_version,item_status"
)

// Options configures a Box provider.
type Options struct {
	Name, APIBase, UploadBase, TokenURL               string
	AccessToken, RefreshToken, ClientID, ClientSecret string
	Client                                            *httpx.Client
	Now                                               func() time.Time
}

// Provider is a Box backend.
type Provider struct {
	provider.TokenPersistence

	name, apiBase, uploadBase, tokenURL  string
	refreshToken, clientID, clientSecret string
	client                               *httpx.Client
	caps                                 provider.Caps
	now                                  func() time.Time

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
	refreshMu   sync.Mutex
}

// New builds a Box provider.
func New(opt Options) (*Provider, error) {
	if opt.Client == nil {
		return nil, errors.New("box: Client is required")
	}
	apiBase, err := cleanBase(opt.APIBase, defaultAPIBase)
	if err != nil {
		return nil, fmt.Errorf("box: invalid api_base: %w", err)
	}
	uploadBase, err := cleanBase(opt.UploadBase, defaultUploadBase)
	if err != nil {
		return nil, fmt.Errorf("box: invalid upload_base: %w", err)
	}
	tokenURL := strings.TrimSpace(opt.TokenURL)
	if tokenURL == "" {
		tokenURL = defaultTokenURL
	}
	if err := safeHTTPURL(tokenURL, false); err != nil {
		return nil, fmt.Errorf("box: invalid token_url: %w", err)
	}
	for field, value := range map[string]string{
		"access_token": opt.AccessToken, "refresh_token": opt.RefreshToken,
		"client_id": opt.ClientID, "client_secret": opt.ClientSecret,
	} {
		if len(value) > 16<<10 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("box: %s contains an unsafe character or is too large", field)
		}
	}
	if opt.AccessToken == "" && opt.RefreshToken == "" {
		return nil, errors.New("box: access_token or refresh_token is required")
	}
	if opt.RefreshToken != "" && (opt.ClientID == "" || opt.ClientSecret == "") {
		return nil, errors.New("box: client_id and client_secret are required with refresh_token")
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	p := &Provider{
		name: opt.Name, apiBase: apiBase, uploadBase: uploadBase, tokenURL: tokenURL,
		accessToken: opt.AccessToken, refreshToken: opt.RefreshToken,
		clientID: opt.ClientID, clientSecret: opt.ClientSecret,
		client: opt.Client, now: now,
	}
	p.caps = provider.Caps{
		// Naming: what the drive refuses in a name, so a pool never places
		// a replica the drive would then reject.
		Naming:    provider.Naming{CaseInsensitive: true, MaxNameBytes: 255, ForbiddenRunes: "\\", NoTrailingDotSpace: true},
		HashTypes: []provider.HashType{provider.HashSHA1},
		RangeRead: true, StreamList: true,
		// Box dictates the part size when it opens a session; this is only the
		// hint used before one exists.
		PartSize: 8 << 20, MaxParts: 10000, UploadParallel: 1,
		SinglePutMax: singlePutMax,
		ServerMove:   true, ServerRename: true, ServerCopy: true,
		// Box's event stream is an account-wide feed with its own semantics
		// rather than a per-drive delta; until that is modelled, directories
		// stay TTL-driven.
		Delta:           false,
		LinkShareable:   false,
		QPS:             provider.QPS{Meta: 8, Download: 8, Upload: 4},
		MaxConnsPerHost: 8,
		Tier:            provider.TierOfficial,
	}
	return p, nil
}

func cleanBase(raw, fallback string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = fallback
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("must be an HTTP(S) URL without credentials, query, or fragment")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	if u.Path != "" && path.Clean(u.Path) != u.Path {
		return "", errors.New("path must be canonical")
	}
	return u.String(), nil
}

func safeHTTPURL(raw string, allowQuery bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.Fragment != "" || (!allowQuery && u.RawQuery != "") {
		return errors.New("must be an HTTP(S) URL without credentials or fragment")
	}
	return nil
}

// rawID validates a Box resource id, which is always a decimal string.
func rawID(id string) error {
	if id == "" || len(id) > maxIDLength {
		return errors.New("box: empty or oversized resource id")
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return fmt.Errorf("box: resource id contains %q", r)
		}
	}
	return nil
}

func encodeID(kind provider.Kind, id string) string {
	if kind == provider.KindDir {
		return "d:" + id
	}
	return "f:" + id
}

// decodeID splits a provider id back into its Box type and number.
func decodeID(id string) (provider.Kind, string, error) {
	prefix, raw, ok := strings.Cut(id, ":")
	if !ok {
		return 0, "", fmt.Errorf("box: id %q has no type prefix", id)
	}
	if err := rawID(raw); err != nil {
		return 0, "", err
	}
	switch prefix {
	case "d":
		return provider.KindDir, raw, nil
	case "f":
		return provider.KindFile, raw, nil
	default:
		return 0, "", fmt.Errorf("box: id %q has an unknown type prefix", id)
	}
}

func safeName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > maxNameLength ||
		!utf8.ValidString(name) || strings.ContainsAny(name, "/\\\x00") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (p *Provider) Name() string                { return p.name }
func (p *Provider) RootID() string              { return RootID }
func (p *Provider) Capabilities() provider.Caps { return p.caps }

func (p *Provider) apiURL(suffix string) string    { return p.apiBase + suffix }
func (p *Provider) uploadURL(suffix string) string { return p.uploadBase + suffix }

type boxItem struct {
	Type              string `json:"type"`
	ID                string `json:"id"`
	Name              string `json:"name"`
	Size              int64  `json:"size"`
	SHA1              string `json:"sha1"`
	ETag              string `json:"etag"`
	ModifiedAt        string `json:"modified_at"`
	ContentModifiedAt string `json:"content_modified_at"`
	ItemStatus        string `json:"item_status"`
	Parent            *struct {
		ID string `json:"id"`
	} `json:"parent"`
	FileVersion *struct {
		ID   string `json:"id"`
		SHA1 string `json:"sha1"`
	} `json:"file_version"`
}

// representable reports whether the item has a byte stream. Box also stores
// web links, which are bookmarks with no content.
func (i boxItem) representable() bool { return i.Type == "file" || i.Type == "folder" }

func (p *Provider) entryFrom(item boxItem, expectedParent string) (provider.Entry, error) {
	if !item.representable() {
		return provider.Entry{}, fmt.Errorf("%w: box item %q is a %s and has no byte stream", provider.ErrUnsupported, item.ID, item.Type)
	}
	if err := rawID(item.ID); err != nil {
		return provider.Entry{}, fmt.Errorf("box: server returned an invalid item id: %w", err)
	}
	if !safeName(item.Name) {
		return provider.Entry{}, fmt.Errorf("box: server returned unsafe name %q", item.Name)
	}
	if item.Size < 0 {
		return provider.Entry{}, errors.New("box: server returned a negative size")
	}
	e := provider.Entry{Name: item.Name, Kind: provider.KindFile, Size: item.Size}
	if item.Type == "folder" {
		e.Kind, e.Size = provider.KindDir, 0
	}
	e.ID = encodeID(e.Kind, item.ID)
	parent := ""
	if item.Parent != nil && item.Parent.ID != "" {
		if err := rawID(item.Parent.ID); err != nil {
			return provider.Entry{}, fmt.Errorf("box: item %q has an invalid parent id: %w", item.ID, err)
		}
		parent = encodeID(provider.KindDir, item.Parent.ID)
	}
	if expectedParent != "" {
		if parent != "" && parent != expectedParent {
			return provider.Entry{}, fmt.Errorf("box: item %q escaped requested parent %q", item.ID, expectedParent)
		}
		parent = expectedParent
	}
	if parent == "" && e.ID != RootID {
		return provider.Entry{}, fmt.Errorf("box: item %q has no parent id", item.ID)
	}
	e.ParentID = parent
	stamp := item.ContentModifiedAt
	if stamp == "" {
		stamp = item.ModifiedAt
	}
	if stamp != "" {
		tm, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			return provider.Entry{}, fmt.Errorf("box: invalid modified_at: %w", err)
		}
		e.ModTime = tm
	}
	if sum := strings.ToLower(item.SHA1); len(sum) == 40 {
		if _, err := hex.DecodeString(sum); err == nil {
			e.Hashes = provider.Hashes{provider.HashSHA1: sum}
		}
	}
	// A file version id names the exact bytes and is what ReadRange pins to.
	// The etag advances on metadata edits too, so it is only the fallback.
	if e.Kind == provider.KindFile && item.FileVersion != nil {
		e.Version = item.FileVersion.ID
	}
	if e.Version == "" {
		e.Version = item.ETag
	}
	if len(e.Version) > maxIDLength || strings.ContainsAny(e.Version, "\r\n\x00") {
		return provider.Entry{}, fmt.Errorf("box: item %q has an unsafe version", item.ID)
	}
	provider.EnsureVersion(&e)
	return e, nil
}

type itemCollection struct {
	Entries    []boxItem `json:"entries"`
	NextMarker string    `json:"next_marker"`
	TotalCount int       `json:"total_count"`
}

// List returns one page of a folder. Web links are skipped: they carry no
// bytes and could not be opened.
func (p *Provider) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	kind, raw, err := decodeID(dirID)
	if err != nil {
		return nil, "", err
	}
	if kind != provider.KindDir {
		return nil, "", fmt.Errorf("%w: box id %q is a file, not a folder", provider.ErrUnsupported, dirID)
	}
	q := url.Values{
		"fields":    {itemFields},
		"limit":     {strconv.Itoa(listPageSize)},
		"usemarker": {"true"},
	}
	if cursor != "" {
		if err := safeCursor(cursor); err != nil {
			return nil, "", err
		}
		q.Set("marker", cursor)
	}
	var page itemCollection
	if err := p.apiJSON(ctx, http.MethodGet, p.apiURL("/folders/"+raw+"/items")+"?"+q.Encode(), nil, &page, ratelimit.Meta, true); err != nil {
		return nil, "", err
	}
	entries := make([]provider.Entry, 0, len(page.Entries))
	for _, item := range page.Entries {
		if !item.representable() {
			continue
		}
		e, err := p.entryFrom(item, dirID)
		if err != nil {
			return nil, "", err
		}
		entries = append(entries, e)
	}
	if page.NextMarker != "" {
		if err := safeCursor(page.NextMarker); err != nil {
			return nil, "", err
		}
		return entries, page.NextMarker, nil
	}
	return entries, "", nil
}

func safeCursor(cursor string) error {
	if len(cursor) > 64<<10 || !utf8.ValidString(cursor) || strings.ContainsAny(cursor, "\r\n\x00") {
		return errors.New("box: unsafe pagination marker")
	}
	return nil
}

// ListStream enumerates a whole folder without retaining its entries.
func (p *Provider) ListStream(ctx context.Context, dirID string, visit func(provider.Entry) error) error {
	if visit == nil {
		return errors.New("box: list visitor is nil")
	}
	cursors := map[string]struct{}{"": {}}
	cursor := ""
	for pages := 0; pages < maxListPages; pages++ {
		entries, next, err := p.List(ctx, dirID, cursor)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := visit(e); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		if _, repeated := cursors[next]; repeated {
			return errors.New("box: repeated pagination marker")
		}
		cursors[next] = struct{}{}
		cursor = next
	}
	return errors.New("box: folder listing exceeds 10000 pages")
}

func (p *Provider) resourcePath(id string) (provider.Kind, string, error) {
	kind, raw, err := decodeID(id)
	if err != nil {
		return 0, "", err
	}
	if kind == provider.KindDir {
		return kind, "/folders/" + raw, nil
	}
	return kind, "/files/" + raw, nil
}

func (p *Provider) Stat(ctx context.Context, id string) (provider.Entry, error) {
	kind, resource, err := p.resourcePath(id)
	if err != nil {
		return provider.Entry{}, err
	}
	q := url.Values{"fields": {itemFields}}
	var item boxItem
	if err := p.apiJSON(ctx, http.MethodGet, p.apiURL(resource)+"?"+q.Encode(), nil, &item, ratelimit.Meta, true); err != nil {
		return provider.Entry{}, err
	}
	if item.ItemStatus == "trashed" || item.ItemStatus == "deleted" {
		return provider.Entry{}, fmt.Errorf("%w: box item is in the trash", provider.ErrNotFound)
	}
	if id == RootID {
		e := provider.Entry{ID: RootID, Kind: provider.KindDir, Version: item.ETag}
		provider.EnsureVersion(&e)
		return e, nil
	}
	e, err := p.entryFrom(item, "")
	if err != nil {
		return provider.Entry{}, err
	}
	if e.Kind != kind {
		return provider.Entry{}, fmt.Errorf("box: id %q claims to be a %v but the server returned a %v", id, kind, e.Kind)
	}
	return e, nil
}

// ReadRange downloads a byte window, pinned to a file version when the caller
// names one so a concurrent remote edit cannot land under an older cache key.
//
// UNVERIFIED: the `version` query parameter on the content endpoint is
// documented but has not been exercised against a real account; a version that
// has been purged is expected to answer 404, which falls back to the current
// content only after confirming it is still the requested version.
func (p *Provider) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	kind, raw, err := decodeID(id)
	if err != nil {
		return nil, err
	}
	if kind != provider.KindFile || off < 0 || n < 0 {
		return nil, errors.New("box: invalid file range")
	}
	if version != "" && rawID(version) == nil {
		q := url.Values{"version": {version}}
		body, err := p.download(ctx, p.apiURL("/files/"+raw+"/content")+"?"+q.Encode(), off, n)
		if err == nil {
			return body, nil
		}
		if !errors.Is(err, provider.ErrNotFound) && !errors.Is(err, provider.ErrLinkExpired) {
			return nil, err
		}
		e, statErr := p.Stat(ctx, id)
		if statErr != nil {
			return nil, statErr
		}
		if e.Version != version {
			return nil, fmt.Errorf("%w: box version %q is gone and the file has changed", provider.ErrConflict, version)
		}
	} else if version != "" {
		e, err := p.Stat(ctx, id)
		if err != nil {
			return nil, err
		}
		if e.Version != version {
			return nil, fmt.Errorf("%w: box file version changed", provider.ErrConflict)
		}
	}
	return p.download(ctx, p.apiURL("/files/"+raw+"/content"), off, n)
}

func (p *Provider) download(ctx context.Context, rawURL string, off, n int64) (io.ReadCloser, error) {
	var body io.ReadCloser
	err := p.authCall(ctx, func(token string) error {
		h := make(http.Header)
		h.Set("Authorization", "Bearer "+token)
		h.Set("Range", httpx.RangeHeader(off, n))
		resp, err := p.client.Do(ctx, httpx.Request{
			Method: http.MethodGet, URL: rawURL, Class: ratelimit.Download, Header: h,
			ExpectStatus: []int{http.StatusOK, http.StatusPartialContent}, Stream: true,
		})
		if err != nil {
			return err
		}
		ranged, err := httpx.RangeBody(resp, off, n)
		if err != nil {
			return err
		}
		body = ranged
		return nil
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// DownloadURL is unsupported: Box answers the content endpoint with a redirect
// to a URL that is only valid for the authenticated caller, and the shared
// HTTP client follows redirects rather than surfacing the target.
func (p *Provider) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	return provider.Link{}, fmt.Errorf("%w: box does not expose a shareable content link", provider.ErrUnsupported)
}

func (p *Provider) validParentName(parentID, name string) (string, error) {
	kind, raw, err := decodeID(parentID)
	if err != nil {
		return "", err
	}
	if kind != provider.KindDir {
		return "", fmt.Errorf("box: parent %q is not a folder", parentID)
	}
	if !safeName(name) {
		return "", fmt.Errorf("box: unsafe child name %q", name)
	}
	return raw, nil
}

// PutFile stores a small file in one multipart/form-data request. A name that
// already exists comes back as a 409 naming the conflicting id, which is then
// uploaded as a new version rather than becoming a second file.
func (p *Provider) PutFile(ctx context.Context, parentID, name string, r io.Reader, size int64, _ provider.Hashes) (provider.Entry, error) {
	parent, err := p.validParentName(parentID, name)
	if err != nil {
		return provider.Entry{}, err
	}
	if size < 0 || size > singlePutMax {
		return provider.Entry{}, errors.New("box: single upload size is outside the supported range")
	}
	content, err := readExact(r, size, singlePutMax)
	if err != nil {
		return provider.Entry{}, err
	}
	body, contentType, err := uploadForm(map[string]any{
		"name": name, "parent": map[string]string{"id": parent},
	}, name, content)
	if err != nil {
		return provider.Entry{}, err
	}
	var out itemCollection
	err = p.uploadBytes(ctx, p.uploadURL("/files/content"), body, contentType, &out)
	var conflict *conflictError
	if errors.As(err, &conflict) {
		versionBody, versionType, buildErr := uploadForm(map[string]any{"name": name}, name, content)
		if buildErr != nil {
			return provider.Entry{}, buildErr
		}
		err = p.uploadBytes(ctx, p.uploadURL("/files/"+conflict.id+"/content"), versionBody, versionType, &out)
	}
	if err != nil {
		return provider.Entry{}, err
	}
	if len(out.Entries) != 1 {
		return provider.Entry{}, fmt.Errorf("box: upload returned %d entries, want 1", len(out.Entries))
	}
	return p.entryFrom(out.Entries[0], parentID)
}

func uploadForm(attributes map[string]any, filename string, content []byte) ([]byte, string, error) {
	encoded, err := json.Marshal(attributes)
	if err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("attributes", string(encoded)); err != nil {
		return nil, "", err
	}
	header := textproto.MIMEHeader{}
	// The filename is quoted rather than interpolated: a name holding a quote
	// would otherwise rewrite the part header.
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%s`, strconv.Quote(filename)))
	header.Set("Content-Type", "application/octet-stream")
	part, err := w.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(content); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// conflictError carries the id Box reports when a name is already taken.
type conflictError struct{ id string }

func (e *conflictError) Error() string { return "box: name already exists as item " + e.id }
func (e *conflictError) Unwrap() error { return provider.ErrExists }

type uploadSessionResponse struct {
	ID               string `json:"id"`
	PartSize         int64  `json:"part_size"`
	TotalParts       int    `json:"total_parts"`
	SessionEndpoints struct {
		UploadPart string `json:"upload_part"`
		Commit     string `json:"commit"`
		Abort      string `json:"abort"`
	} `json:"session_endpoints"`
}

// BeginUpload opens a chunked upload session. Box commits a session only
// against the SHA-1 of the whole file, so the hash is required up front and
// travels in the session state rather than being recomputed at commit time.
func (p *Provider) BeginUpload(ctx context.Context, parentID, name string, size int64, h provider.Hashes) (provider.UploadSession, error) {
	parent, err := p.validParentName(parentID, name)
	if err != nil {
		return provider.UploadSession{}, err
	}
	if size < sessionMinSize {
		return provider.UploadSession{}, fmt.Errorf("box: files below %d bytes must use PutFile", sessionMinSize)
	}
	if size > maxBoxFileSize {
		return provider.UploadSession{}, errors.New("box: upload exceeds the maximum file size")
	}
	whole := strings.ToLower(h[provider.HashSHA1])
	if _, decodeErr := hex.DecodeString(whole); len(whole) != 40 || decodeErr != nil {
		return provider.UploadSession{}, errors.New("box: a chunked upload needs the SHA-1 of the whole file")
	}
	target, body := p.uploadURL("/files/upload_sessions"), map[string]any{
		"folder_id": parent, "file_size": size, "file_name": name,
	}
	existing, err := p.childFileByName(ctx, parentID, name)
	if err != nil {
		return provider.UploadSession{}, err
	}
	if existing != "" {
		target = p.uploadURL("/files/" + existing + "/upload_sessions")
		body = map[string]any{"file_size": size, "file_name": name}
	}
	var out uploadSessionResponse
	if err := p.apiJSON(ctx, http.MethodPost, target, body, &out, ratelimit.Upload, false); err != nil {
		return provider.UploadSession{}, err
	}
	if err := safeSessionID(out.ID); err != nil {
		return provider.UploadSession{}, err
	}
	if out.PartSize <= 0 || out.PartSize > 1<<30 {
		return provider.UploadSession{}, fmt.Errorf("box: session reported an unusable part size %d", out.PartSize)
	}
	if want := int((size + out.PartSize - 1) / out.PartSize); out.TotalParts != 0 && out.TotalParts != want {
		return provider.UploadSession{}, fmt.Errorf("box: session expects %d parts, the file needs %d", out.TotalParts, want)
	}
	return provider.UploadSession{ID: out.ID, PartSize: out.PartSize, Opaque: map[string]string{
		"session_id": out.ID, "parent_id": parentID, "name": name,
		"size": strconv.FormatInt(size, 10), "part_size": strconv.FormatInt(out.PartSize, 10),
		"sha1": whole,
	}}, nil
}

func safeSessionID(id string) error {
	if id == "" || len(id) > 256 || !utf8.ValidString(id) {
		return errors.New("box: upload session id is empty or oversized")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf("box: upload session id contains %q", r)
		}
	}
	return nil
}

func sessionState(s provider.UploadSession) (sessionID, parentID, name, sha1hex string, size, partSize int64, err error) {
	sessionID = s.Opaque["session_id"]
	if sessionID == "" {
		sessionID = s.ID
	}
	if err = safeSessionID(sessionID); err != nil {
		return
	}
	parentID, name = s.Opaque["parent_id"], s.Opaque["name"]
	if _, _, idErr := decodeID(parentID); idErr != nil {
		err = errors.New("box: upload session has an invalid parent id")
		return
	}
	if !safeName(name) {
		err = errors.New("box: upload session has an invalid name")
		return
	}
	sha1hex = strings.ToLower(s.Opaque["sha1"])
	if _, decodeErr := hex.DecodeString(sha1hex); len(sha1hex) != 40 || decodeErr != nil {
		err = errors.New("box: upload session has no usable whole-file SHA-1")
		return
	}
	size, err = strconv.ParseInt(s.Opaque["size"], 10, 64)
	if err != nil || size < sessionMinSize || size > maxBoxFileSize {
		err = errors.New("box: upload session has an invalid size")
		return
	}
	partSize = s.PartSize
	if partSize == 0 {
		partSize, err = strconv.ParseInt(s.Opaque["part_size"], 10, 64)
	}
	if err != nil || partSize <= 0 || partSize > 1<<30 {
		err = errors.New("box: upload session has an invalid part size")
	}
	return
}

// uploadedPart is Box's acknowledgement of one chunk. The commit replays these
// verbatim, so the whole record is carried on the part token.
type uploadedPart struct {
	PartID string `json:"part_id"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	SHA1   string `json:"sha1"`
}

func (p *Provider) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	sessionID, _, _, _, size, partSize, err := sessionState(s)
	if err != nil {
		return provider.PartToken{}, err
	}
	if idx < 0 || int64(idx) > size/partSize+1 {
		return provider.PartToken{}, errors.New("box: invalid part index")
	}
	offset := int64(idx) * partSize
	want := partSize
	if remain := size - offset; remain < want {
		want = remain
	}
	if offset < 0 || offset >= size || n != want {
		return provider.PartToken{}, fmt.Errorf("box: part %d has size %d, want %d", idx, n, want)
	}
	content, err := readExact(r, n, 1<<30)
	if err != nil {
		return provider.PartToken{}, err
	}
	sum := sha1.Sum(content)
	var out struct {
		Part uploadedPart `json:"part"`
	}
	err = p.authCall(ctx, func(token string) error {
		h := make(http.Header)
		h.Set("Authorization", "Bearer "+token)
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Length", strconv.FormatInt(n, 10))
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+n-1, size))
		h.Set("Digest", "sha="+base64.StdEncoding.EncodeToString(sum[:]))
		return p.doJSON(ctx, httpx.Request{
			Method: http.MethodPut, URL: p.uploadURL("/files/upload_sessions/" + sessionID),
			Class: ratelimit.Upload, Header: h, Body: bytes.NewReader(content),
			ExpectStatus: []int{http.StatusOK, http.StatusCreated},
		}, &out)
	})
	if err != nil {
		return provider.PartToken{}, mapUploadError(err)
	}
	if out.Part.Offset != offset || out.Part.Size != n {
		return provider.PartToken{}, fmt.Errorf("box: part %d was acknowledged at offset %d size %d, want %d/%d",
			idx, out.Part.Offset, out.Part.Size, offset, n)
	}
	if strings.ToLower(out.Part.SHA1) != "" && !sameDigest(out.Part.SHA1, sum[:]) {
		return provider.PartToken{}, fmt.Errorf("box: part %d was stored with a different digest", idx)
	}
	encoded, err := json.Marshal(out.Part)
	if err != nil {
		return provider.PartToken{}, err
	}
	return provider.PartToken{Index: idx, ETag: base64.RawURLEncoding.EncodeToString(encoded)}, nil
}

// sameDigest compares Box's acknowledgement, which may be base64 or hex.
func sameDigest(reported string, want []byte) bool {
	if strings.EqualFold(reported, hex.EncodeToString(want)) {
		return true
	}
	if raw, err := base64.StdEncoding.DecodeString(reported); err == nil {
		return bytes.Equal(raw, want)
	}
	return false
}

func (p *Provider) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	sessionID, parentID, _, sha1hex, size, partSize, err := sessionState(s)
	if err != nil {
		return provider.Entry{}, err
	}
	expected := int((size + partSize - 1) / partSize)
	if len(parts) != expected {
		return provider.Entry{}, fmt.Errorf("box: upload has %d parts, want %d", len(parts), expected)
	}
	ordered := make([]uploadedPart, expected)
	seen := make([]bool, expected)
	for _, part := range parts {
		if part.Index < 0 || part.Index >= expected || seen[part.Index] {
			return provider.Entry{}, errors.New("box: upload parts must be unique and contiguous")
		}
		seen[part.Index] = true
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(part.ETag)
		if decodeErr != nil {
			return provider.Entry{}, fmt.Errorf("box: part %d token is unreadable", part.Index)
		}
		var record uploadedPart
		if err := json.Unmarshal(decoded, &record); err != nil {
			return provider.Entry{}, fmt.Errorf("box: part %d token is unreadable", part.Index)
		}
		want := partSize
		if remain := size - int64(part.Index)*partSize; remain < want {
			want = remain
		}
		if record.Offset != int64(part.Index)*partSize || record.Size != want {
			return provider.Entry{}, fmt.Errorf("box: part %d token describes the wrong window", part.Index)
		}
		ordered[part.Index] = record
	}
	digest, err := hex.DecodeString(sha1hex)
	if err != nil {
		return provider.Entry{}, err
	}
	var out itemCollection
	err = p.authCall(ctx, func(token string) error {
		body, marshalErr := json.Marshal(map[string]any{"parts": ordered})
		if marshalErr != nil {
			return marshalErr
		}
		h := make(http.Header)
		h.Set("Authorization", "Bearer "+token)
		h.Set("Content-Type", "application/json")
		h.Set("Digest", "sha="+base64.StdEncoding.EncodeToString(digest))
		return p.doJSON(ctx, httpx.Request{
			Method: http.MethodPost, URL: p.uploadURL("/files/upload_sessions/" + sessionID + "/commit"),
			Class: ratelimit.Upload, Header: h, Body: bytes.NewReader(body),
			ExpectStatus: []int{http.StatusOK, http.StatusCreated, http.StatusAccepted},
			Stream:       true,
		}, &out)
	})
	if err != nil {
		return provider.Entry{}, mapUploadError(err)
	}
	if len(out.Entries) != 1 {
		// A 202 means Box is still assembling the file. Reporting it as
		// transient lets the uploader retry the commit, which is idempotent,
		// instead of treating a pending upload as a failure.
		return provider.Entry{}, fmt.Errorf("%w: box has not finished committing the upload session", provider.ErrTransient)
	}
	e, err := p.entryFrom(out.Entries[0], parentID)
	if err != nil {
		return provider.Entry{}, err
	}
	if e.Kind != provider.KindFile || e.Size != size {
		return provider.Entry{}, errors.New("box: committed file does not match the upload")
	}
	return e, nil
}

// childFileByName finds an existing file with that name so an upload becomes a
// new version instead of a second item.
func (p *Provider) childFileByName(ctx context.Context, parentID, name string) (string, error) {
	cursor := ""
	for pages := 0; pages < maxListPages; pages++ {
		entries, next, err := p.List(ctx, parentID, cursor)
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			if e.Name != name {
				continue
			}
			if e.Kind != provider.KindFile {
				return "", fmt.Errorf("%w: box %q is a folder", provider.ErrExists, name)
			}
			_, raw, err := decodeID(e.ID)
			return raw, err
		}
		if next == "" {
			return "", nil
		}
		cursor = next
	}
	return "", errors.New("box: folder scan exceeds 10000 pages")
}

func (p *Provider) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	parent, err := p.validParentName(parentID, name)
	if err != nil {
		return provider.Entry{}, err
	}
	var item boxItem
	err = p.apiJSON(ctx, http.MethodPost, p.apiURL("/folders")+"?fields="+url.QueryEscape(itemFields), map[string]any{
		"name": name, "parent": map[string]string{"id": parent},
	}, &item, ratelimit.Meta, false)
	var conflict *conflictError
	if errors.As(err, &conflict) {
		return provider.Entry{}, fmt.Errorf("%w: box %q already exists", provider.ErrExists, name)
	}
	if err != nil {
		return provider.Entry{}, err
	}
	return p.entryFrom(item, parentID)
}

func (p *Provider) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	if id == RootID {
		return provider.Entry{}, errors.New("box: cannot rename the root folder")
	}
	if !safeName(newName) {
		return provider.Entry{}, fmt.Errorf("box: unsafe name %q", newName)
	}
	return p.updateItem(ctx, id, map[string]any{"name": newName}, "")
}

func (p *Provider) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	if id == RootID {
		return provider.Entry{}, errors.New("box: cannot move the root folder")
	}
	kind, parent, err := decodeID(newParentID)
	if err != nil {
		return provider.Entry{}, err
	}
	if kind != provider.KindDir {
		return provider.Entry{}, fmt.Errorf("box: move destination %q is not a folder", newParentID)
	}
	return p.updateItem(ctx, id, map[string]any{"parent": map[string]string{"id": parent}}, newParentID)
}

func (p *Provider) updateItem(ctx context.Context, id string, body map[string]any, expectedParent string) (provider.Entry, error) {
	_, resource, err := p.resourcePath(id)
	if err != nil {
		return provider.Entry{}, err
	}
	var item boxItem
	if err := p.apiJSON(ctx, http.MethodPut, p.apiURL(resource)+"?fields="+url.QueryEscape(itemFields), body, &item, ratelimit.Meta, false); err != nil {
		return provider.Entry{}, err
	}
	return p.entryFrom(item, expectedParent)
}

// Copy duplicates an item on the server; no bytes pass through this process.
func (p *Provider) Copy(ctx context.Context, id, newParentID, newName string) (provider.Entry, error) {
	_, resource, err := p.resourcePath(id)
	if err != nil {
		return provider.Entry{}, err
	}
	if id == RootID {
		return provider.Entry{}, errors.New("box: cannot copy the root folder")
	}
	kind, parent, err := decodeID(newParentID)
	if err != nil {
		return provider.Entry{}, err
	}
	if kind != provider.KindDir || !safeName(newName) {
		return provider.Entry{}, errors.New("box: invalid copy destination")
	}
	var item boxItem
	if err := p.apiJSON(ctx, http.MethodPost, p.apiURL(resource+"/copy")+"?fields="+url.QueryEscape(itemFields), map[string]any{
		"parent": map[string]string{"id": parent}, "name": newName,
	}, &item, ratelimit.Meta, false); err != nil {
		return provider.Entry{}, err
	}
	return p.entryFrom(item, newParentID)
}

func (p *Provider) Delete(ctx context.Context, id string) error {
	if id == RootID {
		return errors.New("box: refusing to delete the root folder")
	}
	kind, resource, err := p.resourcePath(id)
	if err != nil {
		return err
	}
	target := p.apiURL(resource)
	if kind == provider.KindDir {
		// Without this a folder with children answers 400 rather than
		// deleting, which the tree above would report as an opaque failure.
		target += "?recursive=true"
	}
	return p.apiJSON(ctx, http.MethodDelete, target, nil, nil, ratelimit.Meta, false)
}

func readExact(r io.Reader, n, max int64) ([]byte, error) {
	if n < 0 || n > max || (r == nil && n != 0) {
		return nil, errors.New("box: invalid request body")
	}
	if r == nil {
		return []byte{}, nil
	}
	b, err := io.ReadAll(io.LimitReader(r, n+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != n {
		return nil, fmt.Errorf("box: request body has %d bytes, want %d", len(b), n)
	}
	return b, nil
}

func (p *Provider) token(ctx context.Context) (string, error) {
	if err := p.FlushTokens(); err != nil {
		return "", fmt.Errorf("box: save rotated credentials: %w", err)
	}
	p.mu.Lock()
	token, expiry := p.accessToken, p.expiresAt
	p.mu.Unlock()
	if token != "" && (expiry.IsZero() || p.now().Add(time.Minute).Before(expiry)) {
		return token, nil
	}
	return p.refresh(ctx, token)
}

// refresh exchanges the refresh token. Box rotates it on every exchange, so
// losing the new value locks the account out; the new one is persisted before
// any request uses the access token it came with.
func (p *Provider) refresh(ctx context.Context, stale string) (string, error) {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	p.mu.Lock()
	current, expiry, rt := p.accessToken, p.expiresAt, p.refreshToken
	p.mu.Unlock()
	if current != "" && current != stale && (expiry.IsZero() || p.now().Add(time.Minute).Before(expiry)) {
		return current, nil
	}
	if rt == "" {
		return "", fmt.Errorf("%w: box has no refresh_token", provider.ErrAuth)
	}
	form := map[string]string{
		"grant_type": "refresh_token", "refresh_token": rt,
		"client_id": p.clientID, "client_secret": p.clientSecret,
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := p.doJSON(ctx, httpx.Request{Method: http.MethodPost, URL: p.tokenURL, Class: ratelimit.Meta, Form: form}, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("%w: box token refresh failed", provider.ErrAuth)
	}
	p.mu.Lock()
	p.accessToken = out.AccessToken
	rotated := ""
	if out.RefreshToken != "" && out.RefreshToken != rt {
		p.refreshToken = out.RefreshToken
		rotated = out.RefreshToken
	}
	if out.ExpiresIn > 0 {
		p.expiresAt = p.now().Add(time.Duration(out.ExpiresIn) * time.Second)
	} else {
		p.expiresAt = time.Time{}
	}
	p.mu.Unlock()
	if rotated != "" {
		if err := p.SaveTokens(map[string]string{"refresh_token": rotated}); err != nil {
			return "", fmt.Errorf("box: save rotated refresh token: %w", err)
		}
	}
	return out.AccessToken, nil
}

func (p *Provider) hasRefresh() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refreshToken != ""
}

func (p *Provider) authCall(ctx context.Context, call func(string) error) error {
	token, err := p.token(ctx)
	if err != nil {
		return err
	}
	err = mapError(call(token))
	if !errors.Is(err, provider.ErrAuth) || !p.hasRefresh() {
		return err
	}
	token, err = p.refresh(ctx, token)
	if err != nil {
		return err
	}
	return mapError(call(token))
}

func (p *Provider) apiJSON(ctx context.Context, method, rawURL string, body, out any, class ratelimit.Class, retryable bool) error {
	return p.authCall(ctx, func(token string) error {
		h := make(http.Header)
		h.Set("Authorization", "Bearer "+token)
		req := httpx.Request{Method: method, URL: rawURL, Header: h, Class: class}
		if body != nil {
			if retryable {
				req.JSON = body
			} else {
				b, err := json.Marshal(body)
				if err != nil {
					return err
				}
				req.Body = bytes.NewReader(b)
				req.Header.Set("Content-Type", "application/json")
			}
		} else if !retryable && method != http.MethodGet && method != http.MethodHead {
			req.Body = bytes.NewReader(nil)
		}
		return p.doJSON(ctx, req, out)
	})
}

func (p *Provider) uploadBytes(ctx context.Context, rawURL string, body []byte, contentType string, out any) error {
	return p.authCall(ctx, func(token string) error {
		h := make(http.Header)
		h.Set("Authorization", "Bearer "+token)
		h.Set("Content-Type", contentType)
		h.Set("Content-Length", strconv.Itoa(len(body)))
		return p.doJSON(ctx, httpx.Request{
			Method: http.MethodPost, URL: rawURL, Header: h,
			Class: ratelimit.Upload, Body: bytes.NewReader(body),
		}, out)
	})
}

func (p *Provider) doJSON(ctx context.Context, req httpx.Request, out any) error {
	req.Stream = true
	resp, err := p.client.Do(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxJSONResponse+1))
		if err != nil {
			return err
		}
		if n > maxJSONResponse {
			return errors.New("box: API response exceeds 16 MiB")
		}
		return nil
	}
	if resp.Status == http.StatusNoContent || resp.Status == http.StatusAccepted {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxJSONResponse))
		return nil
	}
	return decodeJSON(resp.Body, out)
}

func decodeJSON(r io.Reader, out any) error {
	dec := json.NewDecoder(io.LimitReader(r, maxJSONResponse+1))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("box: decode API response: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("box: API response contains trailing JSON")
		}
		return fmt.Errorf("box: oversized or malformed API response: %w", err)
	}
	return nil
}

type apiError struct {
	status int
	code   string
	kind   error
}

func (e *apiError) Error() string { return fmt.Sprintf("box: API HTTP %d (%s)", e.status, e.code) }
func (e *apiError) Unwrap() error { return e.kind }

// mapError classifies a Box error envelope. A 409 carries the id of the item
// that already holds the name, which the upload path uses to store a new
// version instead of creating a duplicate.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var se *httpx.StatusError
	if !errors.As(err, &se) {
		return err
	}
	var envelope struct {
		Code        string `json:"code"`
		Status      int    `json:"status"`
		ContextInfo struct {
			Conflicts json.RawMessage `json:"conflicts"`
		} `json:"context_info"`
	}
	_ = json.Unmarshal([]byte(se.Body), &envelope)
	code := strings.ToLower(envelope.Code)
	if se.Code == http.StatusConflict {
		if id := conflictID(envelope.ContextInfo.Conflicts); id != "" {
			return &conflictError{id: id}
		}
		return &apiError{status: se.Code, code: envelope.Code, kind: provider.ErrExists}
	}
	var kind error
	switch {
	case se.Code == 401 || code == "unauthorized":
		kind = provider.ErrAuth
	case se.Code == 403:
		kind = provider.ErrAuth
	case se.Code == 404:
		kind = provider.ErrNotFound
	case se.Code == 412:
		kind = provider.ErrConflict
	case se.Code == 429:
		kind = provider.ErrRateLimited
	case se.Code >= 500:
		kind = provider.ErrTransient
	default:
		return err
	}
	return &apiError{status: se.Code, code: envelope.Code, kind: kind}
}

// conflictID reads the conflicting item id. Box reports a single object for a
// file name clash and an array for a folder clash.
func conflictID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var single struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &single); err == nil && rawID(single.ID) == nil {
		return single.ID
	}
	var many []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &many); err == nil && len(many) == 1 && rawID(many[0].ID) == nil {
		return many[0].ID
	}
	return ""
}

func mapUploadError(err error) error {
	var se *httpx.StatusError
	if errors.As(err, &se) && (se.Code == http.StatusNotFound || se.Code == http.StatusGone) {
		return fmt.Errorf("%w: box upload session expired", provider.ErrLinkExpired)
	}
	return mapError(err)
}

var (
	_ provider.Provider               = (*Provider)(nil)
	_ provider.StreamLister           = (*Provider)(nil)
	_ provider.SinglePutter           = (*Provider)(nil)
	_ provider.ServerCopier           = (*Provider)(nil)
	_ provider.TokenPersistenceSetter = (*Provider)(nil)
)
