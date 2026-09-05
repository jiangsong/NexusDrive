// Package onedrive implements Microsoft OneDrive through Microsoft Graph v1.0.
package onedrive

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	RootID              = "root"
	defaultGraphBase    = "https://graph.microsoft.com/v1.0"
	defaultPartSize     = 10 << 20
	fragmentUnit        = 320 << 10
	maxFragmentSize     = (60 << 20) - fragmentUnit
	defaultSinglePutMax = 64 << 20
	maxOneDriveFile     = 250_000_000_000
	maxJSONResponse     = 16 << 20
	maxIDLength         = 16 << 10
	linkAdvertisedTTL   = 15 * time.Minute
	linkCacheTTL        = 10 * time.Minute
	maxLinkCacheEntries = 4096
)

type Options struct {
	Name, GraphBase, DriveID, TokenURL                string
	AccessToken, RefreshToken, ClientID, ClientSecret string
	Scope                                             string
	PartSize                                          int64
	Client                                            *httpx.Client
	Now                                               func() time.Time
}

type Provider struct {
	provider.TokenPersistence

	name, graphBase, drivePath, tokenURL        string
	refreshToken, clientID, clientSecret, scope string
	partSize                                    int64
	client                                      *httpx.Client
	caps                                        provider.Caps
	now                                         func() time.Time

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
	rootID      string
	links       map[string]downloadLink
	refreshMu   sync.Mutex
}

type downloadLink struct {
	version   string
	url       string
	expiresAt time.Time
}

func New(opt Options) (*Provider, error) {
	if opt.Client == nil {
		return nil, errors.New("onedrive: Client is required")
	}
	base, err := cleanGraphBase(opt.GraphBase)
	if err != nil {
		return nil, err
	}
	drivePath := "/me/drive"
	if opt.DriveID != "" {
		if err := safeOpaqueID(opt.DriveID); err != nil {
			return nil, fmt.Errorf("onedrive: invalid drive_id: %w", err)
		}
		drivePath = "/drives/" + pathSegment(opt.DriveID)
	}
	tokenURL := strings.TrimSpace(opt.TokenURL)
	if tokenURL == "" {
		tokenURL = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
	}
	if err := safeHTTPURL(tokenURL, false); err != nil {
		return nil, fmt.Errorf("onedrive: invalid token_url: %w", err)
	}
	u, _ := url.Parse(tokenURL)
	if u.RawQuery != "" {
		return nil, errors.New("onedrive: token_url cannot contain a query")
	}
	for name, value := range map[string]string{
		"access_token": opt.AccessToken, "refresh_token": opt.RefreshToken,
		"client_id": opt.ClientID, "client_secret": opt.ClientSecret,
	} {
		if len(value) > maxIDLength || strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("onedrive: %s contains an unsafe character or is too large", name)
		}
	}
	if opt.AccessToken == "" && opt.RefreshToken == "" {
		return nil, errors.New("onedrive: access_token or refresh_token is required")
	}
	if opt.RefreshToken != "" && opt.ClientID == "" {
		return nil, errors.New("onedrive: client_id is required with refresh_token")
	}
	partSize := opt.PartSize
	if partSize == 0 {
		partSize = defaultPartSize
	}
	if partSize <= 0 || partSize > maxFragmentSize || partSize%fragmentUnit != 0 {
		return nil, errors.New("onedrive: part_size must be a positive multiple of 320 KiB and smaller than 60 MiB")
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	p := &Provider{
		name: opt.Name, graphBase: base, drivePath: drivePath, tokenURL: tokenURL,
		accessToken: opt.AccessToken, refreshToken: opt.RefreshToken,
		clientID: opt.ClientID, clientSecret: opt.ClientSecret, scope: opt.Scope,
		partSize: partSize, client: opt.Client, now: now, links: make(map[string]downloadLink),
	}
	p.caps = provider.Caps{
		HashTypes: []provider.HashType{provider.HashSHA1}, RangeRead: true, StreamList: true,
		PartSize: partSize, MaxParts: int((maxOneDriveFile + partSize - 1) / partSize), UploadParallel: 1,
		SinglePutMax: defaultSinglePutMax,
		ServerMove:   true, ServerRename: true, ServerCopy: false, Delta: true,
		LinkTTL: linkAdvertisedTTL, LinkShareable: true,
		QPS: provider.QPS{Meta: 12, Download: 12, Upload: 6}, MaxConnsPerHost: 12,
		Tier: provider.TierOfficial,
	}
	return p, nil
}

func cleanGraphBase(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultGraphBase
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/") {
		return "", errors.New("onedrive: graph_base must be an HTTP(S) URL without credentials, query, or fragment")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	if u.Path == "" {
		return "", errors.New("onedrive: graph_base must include the API version path")
	}
	if path.Clean(u.Path) != u.Path {
		return "", errors.New("onedrive: graph_base path must be canonical")
	}
	return u.String(), nil
}

func safeHTTPURL(raw string, allowQuery bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || (!allowQuery && u.RawQuery != "") {
		return errors.New("must be an HTTP(S) URL without credentials or fragment")
	}
	return nil
}

func safeOpaqueID(id string) error {
	if id == "" || len(id) > maxIDLength || !utf8.ValidString(id) || strings.ContainsAny(id, "/\\\r\n\x00") {
		return errors.New("unsafe or empty item id")
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return errors.New("item id contains a control character")
		}
	}
	return nil
}

func safeName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 1024 || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\\x00") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func pathSegment(value string) string {
	return strings.ReplaceAll(url.PathEscape(value), ":", "%3A")
}

func (p *Provider) Name() string                  { return p.name }
func (p *Provider) RootID() string                { return RootID }
func (p *Provider) Capabilities() provider.Caps   { return p.caps }
func (p *Provider) driveURL(suffix string) string { return p.graphBase + p.drivePath + suffix }

type parentReference struct {
	ID, DriveID, Path string
}

func (r *parentReference) UnmarshalJSON(b []byte) error {
	var x struct {
		ID      string `json:"id"`
		DriveID string `json:"driveId"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(b, &x); err != nil {
		return err
	}
	r.ID, r.DriveID, r.Path = x.ID, x.DriveID, x.Path
	return nil
}

type driveItem struct {
	ID                   string          `json:"id"`
	Name                 string          `json:"name"`
	Size                 int64           `json:"size"`
	ETag                 string          `json:"eTag"`
	CTag                 string          `json:"cTag"`
	LastModifiedDateTime string          `json:"lastModifiedDateTime"`
	Parent               parentReference `json:"parentReference"`
	Folder               *struct{}       `json:"folder"`
	Package              *struct{}       `json:"package"`
	Deleted              *struct{}       `json:"deleted"`
	DownloadURL          string          `json:"@microsoft.graph.downloadUrl"`
	File                 *struct {
		Hashes struct {
			SHA1 string `json:"sha1Hash"`
		} `json:"hashes"`
	} `json:"file"`
}

func (p *Provider) normalizeParent(id string) string {
	p.mu.Lock()
	root := p.rootID
	p.mu.Unlock()
	if root != "" && id == root {
		return RootID
	}
	return id
}

func (p *Provider) normalizedParent(ref parentReference) string {
	if strings.HasSuffix(ref.Path, "/root:") {
		return RootID
	}
	return p.normalizeParent(ref.ID)
}

func (p *Provider) entryFrom(item driveItem, expectedParent string) (provider.Entry, error) {
	if err := safeOpaqueID(item.ID); err != nil {
		return provider.Entry{}, fmt.Errorf("onedrive: server returned invalid item id: %w", err)
	}
	if !safeName(item.Name) {
		return provider.Entry{}, fmt.Errorf("onedrive: server returned unsafe name %q", item.Name)
	}
	if item.Size < 0 {
		return provider.Entry{}, errors.New("onedrive: server returned a negative size")
	}
	parent := p.normalizedParent(item.Parent)
	if expectedParent != "" {
		if parent != "" && parent != expectedParent {
			return provider.Entry{}, fmt.Errorf("onedrive: item %q escaped requested parent %q", item.ID, expectedParent)
		}
		parent = expectedParent
	}
	if parent == "" {
		return provider.Entry{}, fmt.Errorf("onedrive: item %q has no parent id", item.ID)
	}
	e := provider.Entry{ID: item.ID, ParentID: parent, Name: item.Name, Size: item.Size, Kind: provider.KindFile}
	if item.Folder != nil || item.Package != nil {
		e.Kind, e.Size = provider.KindDir, 0
	} else if item.File == nil {
		return provider.Entry{}, fmt.Errorf("%w: onedrive item %q has neither file nor folder facet", provider.ErrUnsupported, item.ID)
	}
	e.Version = item.CTag
	if e.Version == "" {
		e.Version = item.ETag
	}
	if e.Kind == provider.KindFile && e.Version == "" {
		return provider.Entry{}, fmt.Errorf("onedrive: file %q has no cTag or eTag", item.ID)
	}
	if len(e.Version) > maxIDLength || strings.ContainsAny(e.Version, "\r\n\x00") {
		return provider.Entry{}, fmt.Errorf("onedrive: item %q has an unsafe version", item.ID)
	}
	if item.LastModifiedDateTime != "" {
		tm, err := time.Parse(time.RFC3339Nano, item.LastModifiedDateTime)
		if err != nil {
			return provider.Entry{}, fmt.Errorf("onedrive: invalid lastModifiedDateTime: %w", err)
		}
		e.ModTime = tm
	}
	if sum := strings.ToLower(item.FileHash()); len(sum) == 40 {
		if _, err := hex.DecodeString(sum); err == nil {
			e.Hashes = provider.Hashes{provider.HashSHA1: sum}
		}
	}
	if e.Kind == provider.KindFile {
		p.rememberDownload(e.ID, e.Version, item.DownloadURL)
	}
	return e, nil
}

func (p *Provider) rememberDownload(id, version, raw string) {
	if id == "" || version == "" || len(raw) > 64<<10 || safeHTTPURL(raw, true) != nil {
		return
	}
	now := p.now()
	p.mu.Lock()
	if len(p.links) >= maxLinkCacheEntries {
		for key, link := range p.links {
			if !now.Before(link.expiresAt) {
				delete(p.links, key)
			}
		}
		if len(p.links) >= maxLinkCacheEntries {
			clear(p.links)
		}
	}
	p.links[id] = downloadLink{version: version, url: raw, expiresAt: now.Add(linkCacheTTL)}
	p.mu.Unlock()
}

func (p *Provider) cachedDownload(id, version string) (string, bool) {
	now := p.now()
	p.mu.Lock()
	link, ok := p.links[id]
	if ok && !now.Before(link.expiresAt) {
		delete(p.links, id)
		ok = false
	} else if ok && version != "" && link.version != version {
		ok = false
	}
	p.mu.Unlock()
	return link.url, ok
}

func (p *Provider) forgetDownload(id string) {
	p.mu.Lock()
	delete(p.links, id)
	p.mu.Unlock()
}

func (i driveItem) FileHash() string {
	if i.File == nil {
		return ""
	}
	return i.File.Hashes.SHA1
}

func (p *Provider) rootActual(ctx context.Context) (string, error) {
	p.mu.Lock()
	id := p.rootID
	p.mu.Unlock()
	if id != "" {
		return id, nil
	}
	var item driveItem
	if err := p.graphJSON(ctx, http.MethodGet, p.driveURL("/root"), nil, &item, ratelimit.Meta, true); err != nil {
		return "", err
	}
	if err := safeOpaqueID(item.ID); err != nil {
		return "", fmt.Errorf("onedrive: invalid root id: %w", err)
	}
	if item.Folder == nil && item.Package == nil {
		return "", errors.New("onedrive: drive root is not a folder")
	}
	p.mu.Lock()
	if p.rootID == "" {
		p.rootID = item.ID
	}
	id = p.rootID
	p.mu.Unlock()
	return id, nil
}

type itemPage struct {
	Value     []driveItem `json:"value"`
	NextLink  string      `json:"@odata.nextLink"`
	DeltaLink string      `json:"@odata.deltaLink"`
}

func (p *Provider) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	if dirID != RootID {
		if err := safeOpaqueID(dirID); err != nil {
			return nil, "", fmt.Errorf("onedrive: invalid directory id: %w", err)
		}
	} else if _, err := p.rootActual(ctx); err != nil {
		return nil, "", err
	}
	requestURL := cursor
	if requestURL == "" {
		if dirID == RootID {
			requestURL = p.driveURL("/root/children?$top=200")
		} else {
			requestURL = p.driveURL("/items/" + pathSegment(dirID) + "/children?$top=200")
		}
	} else if err := p.safeCursor(cursor); err != nil {
		return nil, "", err
	}
	var page itemPage
	if err := p.graphJSON(ctx, http.MethodGet, requestURL, nil, &page, ratelimit.Meta, true); err != nil {
		return nil, "", err
	}
	entries := make([]provider.Entry, 0, len(page.Value))
	for _, item := range page.Value {
		e, err := p.entryFrom(item, dirID)
		if err != nil {
			return nil, "", err
		}
		entries = append(entries, e)
	}
	if page.NextLink != "" {
		if err := p.safeCursor(page.NextLink); err != nil {
			return nil, "", fmt.Errorf("onedrive: unsafe nextLink: %w", err)
		}
		return entries, page.NextLink, nil
	}
	return entries, "", nil
}

func (p *Provider) ListStream(ctx context.Context, dirID string, visit func(provider.Entry) error) error {
	if visit == nil {
		return errors.New("onedrive: list visitor is nil")
	}
	cursor := ""
	seen := map[string]struct{}{"": {}}
	for pages := 0; pages < 10_000; pages++ {
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
		if _, exists := seen[next]; exists {
			return errors.New("onedrive: repeated directory cursor")
		}
		seen[next] = struct{}{}
		cursor = next
	}
	return errors.New("onedrive: directory listing exceeds 10000 pages")
}

func (p *Provider) getItem(ctx context.Context, id string) (driveItem, error) {
	if id == RootID {
		var item driveItem
		err := p.graphJSON(ctx, http.MethodGet, p.driveURL("/root"), nil, &item, ratelimit.Meta, true)
		if err == nil {
			p.mu.Lock()
			p.rootID = item.ID
			p.mu.Unlock()
		}
		return item, err
	}
	if err := safeOpaqueID(id); err != nil {
		return driveItem{}, fmt.Errorf("onedrive: invalid item id: %w", err)
	}
	var item driveItem
	err := p.graphJSON(ctx, http.MethodGet, p.driveURL("/items/"+pathSegment(id)), nil, &item, ratelimit.Meta, true)
	return item, err
}

func (p *Provider) Stat(ctx context.Context, id string) (provider.Entry, error) {
	item, err := p.getItem(ctx, id)
	if err != nil {
		return provider.Entry{}, err
	}
	if id == RootID {
		if item.Folder == nil && item.Package == nil {
			return provider.Entry{}, errors.New("onedrive: drive root is not a folder")
		}
		return provider.Entry{ID: RootID, Name: "", Kind: provider.KindDir, Version: first(item.CTag, item.ETag)}, nil
	}
	return p.entryFrom(item, "")
}

func (p *Provider) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	if id == RootID || off < 0 || n < 0 {
		return nil, errors.New("onedrive: invalid file range")
	}
	if err := safeOpaqueID(id); err != nil {
		return nil, fmt.Errorf("onedrive: invalid item id: %w", err)
	}
	if raw, ok := p.cachedDownload(id, version); ok {
		body, err := p.readDownloadURL(ctx, raw, off, n)
		if !errors.Is(err, provider.ErrLinkExpired) {
			return body, err
		}
		p.forgetDownload(id)
	}
	item, err := p.getItem(ctx, id)
	if err != nil {
		return nil, err
	}
	e, err := p.entryFrom(item, "")
	if err != nil {
		return nil, err
	}
	if e.Kind != provider.KindFile || (version != "" && e.Version != version) {
		return nil, fmt.Errorf("%w: onedrive item version changed", provider.ErrConflict)
	}
	body, err := p.readDownloadURL(ctx, item.DownloadURL, off, n)
	if !errors.Is(err, provider.ErrLinkExpired) {
		return body, err
	}
	item, err = p.getItem(ctx, id)
	if err != nil {
		return nil, err
	}
	e, err = p.entryFrom(item, "")
	if err != nil || (version != "" && e.Version != version) {
		return nil, fmt.Errorf("%w: onedrive item changed while refreshing its download URL", provider.ErrConflict)
	}
	return p.readDownloadURL(ctx, item.DownloadURL, off, n)
}

func (p *Provider) readDownloadURL(ctx context.Context, raw string, off, n int64) (io.ReadCloser, error) {
	if err := safeHTTPURL(raw, true); err != nil {
		return nil, fmt.Errorf("onedrive: invalid preauthenticated download URL: %w", err)
	}
	h := make(http.Header)
	h.Set("Range", httpx.RangeHeader(off, n))
	resp, err := p.client.Do(ctx, httpx.Request{
		Method: http.MethodGet, URL: raw, Class: ratelimit.Download, Header: h,
		ExpectStatus: []int{http.StatusOK, http.StatusPartialContent}, Stream: true,
	})
	if err != nil {
		var se *httpx.StatusError
		if errors.As(err, &se) && (se.Code == http.StatusForbidden || se.Code == http.StatusGone) {
			return nil, provider.ErrLinkExpired
		}
		return nil, mapError(err)
	}
	return httpx.RangeBody(resp, off, n)
}

func (p *Provider) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	if id == RootID {
		return provider.Link{}, errors.New("onedrive: cannot link the drive root")
	}
	item, err := p.getItem(ctx, id)
	if err != nil {
		return provider.Link{}, err
	}
	e, err := p.entryFrom(item, "")
	if err != nil || e.Kind != provider.KindFile {
		if err == nil {
			err = provider.ErrUnsupported
		}
		return provider.Link{}, err
	}
	if err := safeHTTPURL(item.DownloadURL, true); err != nil {
		return provider.Link{}, fmt.Errorf("onedrive: invalid preauthenticated download URL: %w", err)
	}
	return provider.Link{URL: item.DownloadURL, ExpiresAt: p.now().Add(linkAdvertisedTTL)}, nil
}

func (p *Provider) PutFile(ctx context.Context, parentID, name string, r io.Reader, size int64, _ provider.Hashes) (provider.Entry, error) {
	if err := p.validParentName(parentID, name); err != nil {
		return provider.Entry{}, err
	}
	if size < 0 || size > defaultSinglePutMax {
		return provider.Entry{}, errors.New("onedrive: single upload size is outside the supported range")
	}
	body, err := readExact(r, size, defaultSinglePutMax)
	if err != nil {
		return provider.Entry{}, err
	}
	endpoint := p.driveURL("/items/" + pathSegment(parentID) + ":/" + pathSegment(name) + ":/content")
	if parentID == RootID {
		endpoint = p.driveURL("/root:/" + pathSegment(name) + ":/content")
	}
	var item driveItem
	if err := p.graphBytes(ctx, http.MethodPut, endpoint, body, &item, ratelimit.Upload); err != nil {
		return provider.Entry{}, err
	}
	return p.entryFrom(item, parentID)
}

func (p *Provider) BeginUpload(ctx context.Context, parentID, name string, size int64, _ provider.Hashes) (provider.UploadSession, error) {
	if err := p.validParentName(parentID, name); err != nil {
		return provider.UploadSession{}, err
	}
	if size <= 0 {
		return provider.UploadSession{}, errors.New("onedrive: zero-byte files must use PutFile")
	}
	if size > maxOneDriveFile {
		return provider.UploadSession{}, errors.New("onedrive: upload exceeds the 250 GB limit")
	}
	endpoint := p.driveURL("/items/" + pathSegment(parentID) + ":/" + pathSegment(name) + ":/createUploadSession")
	if parentID == RootID {
		endpoint = p.driveURL("/root:/" + pathSegment(name) + ":/createUploadSession")
	}
	var out struct {
		UploadURL          string   `json:"uploadUrl"`
		ExpirationDateTime string   `json:"expirationDateTime"`
		NextExpectedRanges []string `json:"nextExpectedRanges"`
	}
	if err := p.graphJSON(ctx, http.MethodPost, endpoint, map[string]any{
		"item": map[string]any{"@microsoft.graph.conflictBehavior": "replace", "name": name},
	}, &out, ratelimit.Upload, false); err != nil {
		return provider.UploadSession{}, err
	}
	if err := safeHTTPURL(out.UploadURL, true); err != nil || len(out.UploadURL) > 64<<10 {
		return provider.UploadSession{}, errors.New("onedrive: createUploadSession returned an invalid uploadUrl")
	}
	return provider.UploadSession{ID: out.UploadURL, PartSize: p.partSize, Opaque: map[string]string{
		"upload_url": out.UploadURL, "parent_id": parentID, "name": name,
		"size": strconv.FormatInt(size, 10), "part_size": strconv.FormatInt(p.partSize, 10),
	}}, nil
}

func (p *Provider) validParentName(parentID, name string) error {
	if parentID != RootID {
		if err := safeOpaqueID(parentID); err != nil {
			return fmt.Errorf("onedrive: invalid parent id: %w", err)
		}
	}
	if !safeName(name) {
		return fmt.Errorf("onedrive: unsafe child name %q", name)
	}
	return nil
}

func sessionState(s provider.UploadSession) (uploadURL, parentID, name string, size, partSize int64, err error) {
	uploadURL = s.Opaque["upload_url"]
	if uploadURL == "" {
		uploadURL = s.ID
	}
	if len(uploadURL) > 64<<10 || safeHTTPURL(uploadURL, true) != nil {
		err = errors.New("onedrive: upload session has an invalid upload URL")
		return
	}
	parentID, name = s.Opaque["parent_id"], s.Opaque["name"]
	if parentID != RootID {
		if idErr := safeOpaqueID(parentID); idErr != nil {
			err = errors.New("onedrive: upload session has an invalid parent id")
			return
		}
	}
	if !safeName(name) {
		err = errors.New("onedrive: upload session has an invalid name")
		return
	}
	size, err = strconv.ParseInt(s.Opaque["size"], 10, 64)
	if err != nil || size <= 0 || size > maxOneDriveFile {
		err = errors.New("onedrive: upload session has an invalid size")
		return
	}
	partSize = s.PartSize
	if partSize == 0 {
		partSize, err = strconv.ParseInt(s.Opaque["part_size"], 10, 64)
	}
	if err != nil || partSize <= 0 || partSize > maxFragmentSize || partSize%fragmentUnit != 0 {
		err = errors.New("onedrive: upload session has an invalid part size")
	}
	return
}

func (p *Provider) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	uploadURL, parentID, _, size, partSize, err := sessionState(s)
	if err != nil {
		return provider.PartToken{}, err
	}
	if idx < 0 || int64(idx) > maxOneDriveFile/partSize {
		return provider.PartToken{}, errors.New("onedrive: invalid part index")
	}
	offset := int64(idx) * partSize
	want := partSize
	if remain := size - offset; remain < want {
		want = remain
	}
	if offset < 0 || offset >= size || n != want {
		return provider.PartToken{}, fmt.Errorf("onedrive: part %d has size %d, want %d", idx, n, want)
	}
	body, err := readExact(r, n, maxFragmentSize)
	if err != nil {
		return provider.PartToken{}, err
	}
	h := make(http.Header)
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(n, 10))
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+n-1, size))
	resp, err := p.client.Do(ctx, httpx.Request{
		Method: http.MethodPut, URL: uploadURL, Class: ratelimit.Upload, Header: h,
		Body: bytes.NewReader(body), ExpectStatus: []int{http.StatusOK, http.StatusCreated, http.StatusAccepted}, Stream: true,
	})
	if err != nil {
		return provider.PartToken{}, mapUploadError(err)
	}
	defer resp.Body.Close()
	token := provider.PartToken{Index: idx, ETag: strconv.FormatInt(n, 10)}
	if offset+n == size {
		var item driveItem
		if err := decodeJSON(resp.Body, &item); err != nil {
			return provider.PartToken{}, err
		}
		e, err := p.entryFrom(item, parentID)
		if err != nil || e.Kind != provider.KindFile || e.Size != size {
			return provider.PartToken{}, errors.New("onedrive: final fragment returned mismatched file metadata")
		}
		token.ETag += ":" + base64.RawURLEncoding.EncodeToString([]byte(e.ID))
	} else {
		var state struct {
			NextExpectedRanges []string `json:"nextExpectedRanges"`
		}
		if err := decodeJSON(resp.Body, &state); err != nil || !rangeStartsAt(state.NextExpectedRanges, offset+n) {
			return provider.PartToken{}, errors.New("onedrive: fragment response did not acknowledge the expected next offset")
		}
	}
	return token, nil
}

func rangeStartsAt(ranges []string, offset int64) bool {
	prefix := strconv.FormatInt(offset, 10) + "-"
	for _, r := range ranges {
		if r == strconv.FormatInt(offset, 10) || strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

func (p *Provider) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	_, _, _, size, partSize, err := sessionState(s)
	if err != nil {
		return provider.Entry{}, err
	}
	expected := int((size + partSize - 1) / partSize)
	if len(parts) != expected {
		return provider.Entry{}, fmt.Errorf("onedrive: upload has %d parts, want %d", len(parts), expected)
	}
	seen := make([]bool, expected)
	finalID := ""
	for _, part := range parts {
		if part.Index < 0 || part.Index >= expected || seen[part.Index] {
			return provider.Entry{}, errors.New("onedrive: upload parts must be unique and contiguous")
		}
		seen[part.Index] = true
		fields := strings.SplitN(part.ETag, ":", 2)
		length, parseErr := strconv.ParseInt(fields[0], 10, 64)
		want := partSize
		if remain := size - int64(part.Index)*partSize; remain < want {
			want = remain
		}
		if parseErr != nil || length != want {
			return provider.Entry{}, fmt.Errorf("onedrive: part %d token has the wrong length", part.Index)
		}
		if part.Index == expected-1 && len(fields) == 2 {
			decoded, decodeErr := base64.RawURLEncoding.DecodeString(fields[1])
			if decodeErr == nil {
				finalID = string(decoded)
			}
		}
	}
	if finalID == "" || safeOpaqueID(finalID) != nil {
		return provider.Entry{}, errors.New("onedrive: final part token has no committed item id")
	}
	e, err := p.Stat(ctx, finalID)
	if err != nil {
		return provider.Entry{}, err
	}
	if e.Kind != provider.KindFile || e.Size != size {
		return provider.Entry{}, errors.New("onedrive: committed item does not match the upload")
	}
	return e, nil
}

func (p *Provider) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	if err := p.validParentName(parentID, name); err != nil {
		return provider.Entry{}, err
	}
	endpoint := p.driveURL("/items/" + pathSegment(parentID) + "/children")
	if parentID == RootID {
		endpoint = p.driveURL("/root/children")
	}
	var item driveItem
	if err := p.graphJSON(ctx, http.MethodPost, endpoint, map[string]any{
		"name": name, "folder": map[string]any{}, "@microsoft.graph.conflictBehavior": "fail",
	}, &item, ratelimit.Meta, false); err != nil {
		return provider.Entry{}, err
	}
	return p.entryFrom(item, parentID)
}

func (p *Provider) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	if id == RootID || safeOpaqueID(id) != nil || !safeName(newName) {
		return provider.Entry{}, errors.New("onedrive: invalid rename")
	}
	return p.patchItem(ctx, id, map[string]any{"name": newName})
}

func (p *Provider) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	if id == RootID || safeOpaqueID(id) != nil {
		return provider.Entry{}, errors.New("onedrive: invalid move source")
	}
	parent := newParentID
	if newParentID == RootID {
		var err error
		parent, err = p.rootActual(ctx)
		if err != nil {
			return provider.Entry{}, err
		}
	} else if err := safeOpaqueID(newParentID); err != nil {
		return provider.Entry{}, errors.New("onedrive: invalid move destination")
	}
	return p.patchItem(ctx, id, map[string]any{"parentReference": map[string]string{"id": parent}})
}

func (p *Provider) patchItem(ctx context.Context, id string, body map[string]any) (provider.Entry, error) {
	var item driveItem
	if err := p.graphJSON(ctx, http.MethodPatch, p.driveURL("/items/"+pathSegment(id)), body, &item, ratelimit.Meta, false); err != nil {
		return provider.Entry{}, err
	}
	return p.entryFrom(item, "")
}

func (p *Provider) Delete(ctx context.Context, id string) error {
	if id == RootID || safeOpaqueID(id) != nil {
		return errors.New("onedrive: refusing to delete an invalid item or drive root")
	}
	err := p.graphJSON(ctx, http.MethodDelete, p.driveURL("/items/"+pathSegment(id)), nil, nil, ratelimit.Meta, false)
	if err == nil {
		p.forgetDownload(id)
	}
	return err
}

func (p *Provider) Changes(ctx context.Context, cursor string) ([]provider.Change, string, error) {
	root, err := p.rootActual(ctx)
	if err != nil {
		return nil, "", err
	}
	requestURL := cursor
	baseline := cursor == ""
	if baseline {
		requestURL = p.driveURL("/root/delta?token=latest")
	} else if err := p.safeCursor(cursor); err != nil {
		return nil, "", err
	}
	var page itemPage
	if err := p.graphJSON(ctx, http.MethodGet, requestURL, nil, &page, ratelimit.Meta, true); err != nil {
		if !baseline && isCursorReset(err) {
			return p.newDeltaBaseline(ctx)
		}
		return nil, "", err
	}
	next := first(page.NextLink, page.DeltaLink)
	if next == "" || p.safeCursor(next) != nil {
		return nil, "", errors.New("onedrive: delta response has no safe continuation link")
	}
	if baseline {
		return nil, "", &provider.CursorResetError{Cursor: next}
	}
	changes := make([]provider.Change, 0, len(page.Value))
	for _, item := range page.Value {
		if item.ID == root {
			continue
		}
		if err := safeOpaqueID(item.ID); err != nil {
			return nil, "", errors.New("onedrive: delta returned an invalid item id")
		}
		parent := p.normalizedParent(item.Parent)
		if item.Deleted != nil {
			p.forgetDownload(item.ID)
			changes = append(changes, provider.Change{Op: provider.ChangeDelete, ID: item.ID, ParentID: parent})
			continue
		}
		e, err := p.entryFrom(item, "")
		if err != nil {
			return nil, "", err
		}
		entry := e
		changes = append(changes, provider.Change{Op: provider.ChangeUpsert, ID: e.ID, ParentID: e.ParentID, Entry: &entry})
	}
	return changes, next, nil
}

func (p *Provider) newDeltaBaseline(ctx context.Context) ([]provider.Change, string, error) {
	var page itemPage
	if err := p.graphJSON(ctx, http.MethodGet, p.driveURL("/root/delta?token=latest"), nil, &page, ratelimit.Meta, true); err != nil {
		return nil, "", err
	}
	next := first(page.NextLink, page.DeltaLink)
	if next == "" || p.safeCursor(next) != nil {
		return nil, "", errors.New("onedrive: replacement delta cursor is invalid")
	}
	return nil, "", &provider.CursorResetError{Cursor: next}
}

func (p *Provider) safeCursor(raw string) error {
	if len(raw) > 64<<10 {
		return errors.New("onedrive: cursor is too large")
	}
	u, err := url.Parse(raw)
	base, _ := url.Parse(p.graphBase)
	if err != nil || u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || u.Fragment != "" ||
		(u.Path != base.Path && !strings.HasPrefix(u.Path, strings.TrimSuffix(base.Path, "/")+"/")) {
		return errors.New("onedrive: cursor escaped the configured Graph API")
	}
	return nil
}

func readExact(r io.Reader, n, max int64) ([]byte, error) {
	if n < 0 || n > max || (r == nil && n != 0) {
		return nil, errors.New("onedrive: invalid request body")
	}
	if r == nil {
		return []byte{}, nil
	}
	b, err := io.ReadAll(io.LimitReader(r, n+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != n {
		return nil, fmt.Errorf("onedrive: request body has %d bytes, want %d", len(b), n)
	}
	return b, nil
}

func (p *Provider) token(ctx context.Context) (string, error) {
	if err := p.FlushTokens(); err != nil {
		return "", fmt.Errorf("onedrive: save rotated credentials: %w", err)
	}
	p.mu.Lock()
	token, expiry := p.accessToken, p.expiresAt
	p.mu.Unlock()
	if token != "" && (expiry.IsZero() || p.now().Add(time.Minute).Before(expiry)) {
		return token, nil
	}
	return p.refresh(ctx, token)
}

func (p *Provider) refresh(ctx context.Context, stale string) (string, error) {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	p.mu.Lock()
	current, expiry := p.accessToken, p.expiresAt
	rt := p.refreshToken
	p.mu.Unlock()
	if current != "" && current != stale && (expiry.IsZero() || p.now().Add(time.Minute).Before(expiry)) {
		return current, nil
	}
	if rt == "" {
		return "", fmt.Errorf("%w: onedrive has no refresh_token", provider.ErrAuth)
	}
	form := map[string]string{"grant_type": "refresh_token", "refresh_token": rt, "client_id": p.clientID}
	if p.clientSecret != "" {
		form["client_secret"] = p.clientSecret
	}
	if p.scope != "" {
		form["scope"] = p.scope
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := p.doJSON(ctx, httpx.Request{Method: http.MethodPost, URL: p.tokenURL, Class: ratelimit.Meta, Form: form}, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("%w: onedrive token refresh failed", provider.ErrAuth)
	}
	p.mu.Lock()
	p.accessToken = out.AccessToken
	rotated := ""
	if out.RefreshToken != "" {
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
			return "", fmt.Errorf("onedrive: save rotated refresh token: %w", err)
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

func (p *Provider) graphJSON(ctx context.Context, method, rawURL string, body, out any, class ratelimit.Class, retryable bool) error {
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
			// State-changing bodyless operations must not be replayed after an
			// ambiguous transport failure.
			req.Body = bytes.NewReader(nil)
		}
		return p.doJSON(ctx, req, out)
	})
}

func (p *Provider) graphBytes(ctx context.Context, method, rawURL string, body []byte, out any, class ratelimit.Class) error {
	return p.authCall(ctx, func(token string) error {
		h := make(http.Header)
		h.Set("Authorization", "Bearer "+token)
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		return p.doJSON(ctx, httpx.Request{Method: method, URL: rawURL, Header: h, Class: class, Body: bytes.NewReader(body)}, out)
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
			return errors.New("onedrive: API response exceeds 16 MiB")
		}
		return nil
	}
	return decodeJSON(resp.Body, out)
}

func decodeJSON(r io.Reader, out any) error {
	dec := json.NewDecoder(io.LimitReader(r, maxJSONResponse+1))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("onedrive: decode API response: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("onedrive: API response contains trailing JSON")
		}
		return fmt.Errorf("onedrive: oversized or malformed API response: %w", err)
	}
	return nil
}

type apiError struct {
	status int
	code   string
	kind   error
}

func (e *apiError) Error() string {
	return fmt.Sprintf("onedrive: Graph HTTP %d (%s)", e.status, e.code)
}
func (e *apiError) Unwrap() error { return e.kind }

func mapError(err error) error {
	if err == nil {
		return nil
	}
	var se *httpx.StatusError
	if !errors.As(err, &se) {
		return err
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(se.Body), &envelope)
	code := strings.ToLower(envelope.Error.Code)
	var kind error
	switch {
	case se.Code == 401 || se.Code == 403:
		kind = provider.ErrAuth
	case se.Code == 404 || strings.Contains(code, "notfound") || code == "itemnotfound":
		kind = provider.ErrNotFound
	case se.Code == 409 && (code == "namealreadyexists" || strings.Contains(code, "conflict")):
		kind = provider.ErrExists
	case se.Code == 409 || se.Code == 412:
		kind = provider.ErrConflict
	case se.Code == 429:
		kind = provider.ErrRateLimited
	case se.Code == 410 || code == "resyncrequired" || code == "syncstatenotfound":
		kind = provider.ErrCursorReset
	case se.Code >= 500:
		kind = provider.ErrTransient
	default:
		return err
	}
	return &apiError{status: se.Code, code: envelope.Error.Code, kind: kind}
}

func mapUploadError(err error) error {
	var se *httpx.StatusError
	if errors.As(err, &se) && (se.Code == http.StatusNotFound || se.Code == http.StatusGone) {
		return fmt.Errorf("%w: onedrive upload session expired", provider.ErrLinkExpired)
	}
	return mapError(err)
}

func isCursorReset(err error) bool { return errors.Is(err, provider.ErrCursorReset) }
func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

var (
	_ provider.Provider               = (*Provider)(nil)
	_ provider.StreamLister           = (*Provider)(nil)
	_ provider.SinglePutter           = (*Provider)(nil)
	_ provider.ChangeLister           = (*Provider)(nil)
	_ provider.TokenPersistenceSetter = (*Provider)(nil)
)
