// Package gdrive implements Google Drive through the Drive API v3.
//
// Two Drive behaviours do not map onto a POSIX tree and are handled here
// rather than left for an upper layer to discover:
//
//   - A folder may hold several children with the same name. The metadata
//     store keys children by name, so a listing carrying a duplicate cannot be
//     published. The driver detects it and fails the directory with a message
//     naming the collision instead of silently dropping or reordering a file.
//   - Google Workspace documents (Docs, Sheets, …) and shortcuts have no byte
//     stream; they can only be exported, and their size is unknown until the
//     export runs. They are skipped by List and Changes, and Stat reports
//     ErrUnsupported for them.
//
// Reads are pinned to a revision (Entry.Version is headRevisionId) so a
// concurrent remote edit cannot deliver newer bytes under an older cache key.
package gdrive

import (
	"bytes"
	"context"
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
	// RootID is Drive's alias for the drive root. The actual id is resolved
	// lazily so parent references coming back from the API can be normalised.
	RootID = "root"

	defaultAPIBase   = "https://www.googleapis.com"
	defaultTokenURL  = "https://oauth2.googleapis.com/token"
	folderMime       = "application/vnd.google-apps.folder"
	workspacePrefix  = "application/vnd.google-apps."
	uploadChunkUnit  = 256 << 10
	defaultPartSize  = 8 << 20
	maxPartSize      = 256 << 20
	singlePutMax     = 5 << 20
	maxDriveFileSize = 5 << 40
	maxJSONResponse  = 16 << 20
	maxIDLength      = 16 << 10
	listPageSize     = 1000
	maxListPages     = 10_000

	// fileFields is requested on every call that returns a file resource.
	// Drive omits anything not asked for, and an entry assembled from a
	// partial resource would carry a wrong size or an empty version.
	fileFields = "id,name,mimeType,size,md5Checksum,modifiedTime,version,headRevisionId,parents,trashed"
)

// Options configures a Drive provider.
type Options struct {
	Name, APIBase, TokenURL                           string
	AccessToken, RefreshToken, ClientID, ClientSecret string
	// DriveID selects a shared drive. Empty uses the user's My Drive.
	DriveID  string
	PartSize int64
	Client   *httpx.Client
	Now      func() time.Time
}

// Provider is a Google Drive backend.
type Provider struct {
	provider.TokenPersistence

	name, apiBase, tokenURL              string
	driveID                              string
	refreshToken, clientID, clientSecret string
	partSize                             int64
	client                               *httpx.Client
	caps                                 provider.Caps
	now                                  func() time.Time

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
	rootID      string
	refreshMu   sync.Mutex
}

// New builds a Drive provider from already validated options.
func New(opt Options) (*Provider, error) {
	if opt.Client == nil {
		return nil, errors.New("gdrive: Client is required")
	}
	base, err := cleanBase(opt.APIBase)
	if err != nil {
		return nil, err
	}
	tokenURL := strings.TrimSpace(opt.TokenURL)
	if tokenURL == "" {
		tokenURL = defaultTokenURL
	}
	if err := safeHTTPURL(tokenURL, false); err != nil {
		return nil, fmt.Errorf("gdrive: invalid token_url: %w", err)
	}
	for field, value := range map[string]string{
		"access_token": opt.AccessToken, "refresh_token": opt.RefreshToken,
		"client_id": opt.ClientID, "client_secret": opt.ClientSecret, "drive_id": opt.DriveID,
	} {
		if len(value) > maxIDLength || strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("gdrive: %s contains an unsafe character or is too large", field)
		}
	}
	if opt.AccessToken == "" && opt.RefreshToken == "" {
		return nil, errors.New("gdrive: access_token or refresh_token is required")
	}
	if opt.RefreshToken != "" && opt.ClientID == "" {
		return nil, errors.New("gdrive: client_id is required with refresh_token")
	}
	if opt.DriveID != "" && safeID(opt.DriveID) != nil {
		return nil, errors.New("gdrive: invalid drive_id")
	}
	partSize := opt.PartSize
	if partSize == 0 {
		partSize = defaultPartSize
	}
	if partSize <= 0 || partSize > maxPartSize || partSize%uploadChunkUnit != 0 {
		return nil, errors.New("gdrive: part_size must be a positive multiple of 256 KiB and at most 256 MiB")
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	p := &Provider{
		name: opt.Name, apiBase: base, tokenURL: tokenURL, driveID: opt.DriveID,
		accessToken: opt.AccessToken, refreshToken: opt.RefreshToken,
		clientID: opt.ClientID, clientSecret: opt.ClientSecret,
		partSize: partSize, client: opt.Client, now: now,
	}
	p.caps = provider.Caps{
		// Naming: what the drive refuses in a name, so a pool never places
		// a replica the drive would then reject.
		Naming:    provider.Naming{},
		HashTypes: []provider.HashType{provider.HashMD5},
		RangeRead: true, StreamList: true,
		PartSize: partSize, MaxParts: int((maxDriveFileSize + partSize - 1) / partSize),
		UploadParallel: 1, SinglePutMax: singlePutMax,
		ServerMove: true, ServerRename: true, ServerCopy: true, Delta: true,
		// Drive serves private content only to an authenticated request, so
		// there is no link a third party could follow.
		LinkShareable:   false,
		Share:           true,
		QPS:             provider.QPS{Meta: 10, Download: 10, Upload: 4},
		MaxConnsPerHost: 12,
		Tier:            provider.TierOfficial,
	}
	return p, nil
}

func cleanBase(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultAPIBase
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("gdrive: api_base must be an HTTP(S) URL without credentials, query, or fragment")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	if u.Path != "" && path.Clean(u.Path) != u.Path {
		return "", errors.New("gdrive: api_base path must be canonical")
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

// safeID rejects anything that could not be a Drive resource id. Drive ids are
// URL-safe base64-ish tokens; a value carrying a quote would also let a caller
// break out of the `q` search expression built below.
func safeID(id string) error {
	if id == "" || len(id) > maxIDLength || !utf8.ValidString(id) {
		return errors.New("unsafe or empty resource id")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '~':
		default:
			return fmt.Errorf("resource id contains %q", r)
		}
	}
	return nil
}

func safeName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 1024 ||
		!utf8.ValidString(name) || strings.ContainsAny(name, "/\x00") {
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

func (p *Provider) apiURL(suffix string) string    { return p.apiBase + "/drive/v3" + suffix }
func (p *Provider) uploadURL(suffix string) string { return p.apiBase + "/upload/drive/v3" + suffix }

// driveScope adds the parameters every request needs to see shared drives.
func (p *Provider) driveScope(q url.Values) url.Values {
	q.Set("supportsAllDrives", "true")
	if p.driveID != "" {
		q.Set("driveId", p.driveID)
		q.Set("corpora", "drive")
		q.Set("includeItemsFromAllDrives", "true")
	}
	return q
}

type driveFile struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	MimeType       string   `json:"mimeType"`
	Size           string   `json:"size"`
	MD5            string   `json:"md5Checksum"`
	ModifiedTime   string   `json:"modifiedTime"`
	Version        string   `json:"version"`
	HeadRevisionID string   `json:"headRevisionId"`
	Parents        []string `json:"parents"`
	Trashed        bool     `json:"trashed"`
}

func (f driveFile) isFolder() bool { return f.MimeType == folderMime }

// representable reports whether the item has a byte stream cloudfs can serve.
// Workspace documents and shortcuts do not.
func (f driveFile) representable() bool {
	return f.isFolder() || !strings.HasPrefix(f.MimeType, workspacePrefix)
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

func (p *Provider) entryFrom(f driveFile, expectedParent string) (provider.Entry, error) {
	if err := safeID(f.ID); err != nil {
		return provider.Entry{}, fmt.Errorf("gdrive: server returned invalid file id: %w", err)
	}
	if !safeName(f.Name) {
		return provider.Entry{}, fmt.Errorf("gdrive: server returned unsafe name %q", f.Name)
	}
	if !f.representable() {
		return provider.Entry{}, fmt.Errorf("%w: gdrive item %q has mime type %q and no byte stream", provider.ErrUnsupported, f.ID, f.MimeType)
	}
	e := provider.Entry{ID: f.ID, Name: f.Name, Kind: provider.KindFile}
	if f.isFolder() {
		e.Kind = provider.KindDir
	} else {
		size, err := strconv.ParseInt(strings.TrimSpace(orZero(f.Size)), 10, 64)
		if err != nil || size < 0 {
			return provider.Entry{}, fmt.Errorf("gdrive: file %q has an invalid size %q", f.ID, f.Size)
		}
		e.Size = size
	}
	parent := ""
	if len(f.Parents) > 0 {
		if err := safeID(f.Parents[0]); err != nil {
			return provider.Entry{}, fmt.Errorf("gdrive: file %q has an invalid parent id: %w", f.ID, err)
		}
		parent = p.normalizeParent(f.Parents[0])
	}
	if expectedParent != "" {
		if parent != "" && parent != expectedParent {
			return provider.Entry{}, fmt.Errorf("gdrive: file %q escaped requested parent %q", f.ID, expectedParent)
		}
		parent = expectedParent
	}
	if parent == "" {
		return provider.Entry{}, fmt.Errorf("gdrive: file %q has no parent id", f.ID)
	}
	e.ParentID = parent
	if f.ModifiedTime != "" {
		tm, err := time.Parse(time.RFC3339Nano, f.ModifiedTime)
		if err != nil {
			return provider.Entry{}, fmt.Errorf("gdrive: invalid modifiedTime: %w", err)
		}
		e.ModTime = tm
	}
	if sum := strings.ToLower(f.MD5); len(sum) == 32 {
		if _, err := hex.DecodeString(sum); err == nil {
			e.Hashes = provider.Hashes{provider.HashMD5: sum}
		}
	}
	// headRevisionId names the exact bytes and is what ReadRange downloads, so
	// prefer it. `version` also advances on metadata-only edits, which merely
	// invalidates the cache more often than strictly necessary.
	e.Version = f.HeadRevisionID
	if e.Version == "" {
		e.Version = f.Version
	}
	if len(e.Version) > maxIDLength || strings.ContainsAny(e.Version, "\r\n\x00") {
		return provider.Entry{}, fmt.Errorf("gdrive: file %q has an unsafe version", f.ID)
	}
	provider.EnsureVersion(&e)
	return e, nil
}

func orZero(v string) string {
	if strings.TrimSpace(v) == "" {
		return "0"
	}
	return v
}

// rootActual resolves and caches the concrete id of the drive root.
func (p *Provider) rootActual(ctx context.Context) (string, error) {
	p.mu.Lock()
	id := p.rootID
	p.mu.Unlock()
	if id != "" {
		return id, nil
	}
	target := "root"
	if p.driveID != "" {
		target = p.driveID
	}
	q := p.driveScope(url.Values{"fields": {"id,mimeType"}})
	q.Del("driveId")
	q.Del("corpora")
	q.Del("includeItemsFromAllDrives")
	var f driveFile
	if err := p.apiJSON(ctx, http.MethodGet, p.apiURL("/files/"+url.PathEscape(target))+"?"+q.Encode(), nil, &f, ratelimit.Meta, true); err != nil {
		return "", err
	}
	if err := safeID(f.ID); err != nil {
		return "", fmt.Errorf("gdrive: invalid root id: %w", err)
	}
	if !f.isFolder() {
		return "", errors.New("gdrive: drive root is not a folder")
	}
	p.mu.Lock()
	if p.rootID == "" {
		p.rootID = f.ID
	}
	id = p.rootID
	p.mu.Unlock()
	return id, nil
}

type filePage struct {
	Files         []driveFile `json:"files"`
	NextPageToken string      `json:"nextPageToken"`
}

func (p *Provider) listQuery(dirID, cursor string) (string, error) {
	target := dirID
	if dirID == RootID {
		target = "root"
	} else if err := safeID(dirID); err != nil {
		return "", fmt.Errorf("gdrive: invalid directory id: %w", err)
	}
	q := p.driveScope(url.Values{
		"q":      {"'" + target + "' in parents and trashed = false"},
		"fields": {"nextPageToken,files(" + fileFields + ")"},
		// A stable order keeps successive enumerations of the same directory
		// comparable, which is what makes a duplicate-name report reproducible.
		// UNVERIFIED: that orderBy stays consistent across page tokens while the
		// directory is being modified is not stated by the API reference.
		"orderBy":  {"createdTime,name"},
		"pageSize": {strconv.Itoa(listPageSize)},
		"spaces":   {"drive"},
	})
	if cursor != "" {
		if err := safeCursor(cursor); err != nil {
			return "", err
		}
		q.Set("pageToken", cursor)
	}
	return p.apiURL("/files") + "?" + q.Encode(), nil
}

func safeCursor(cursor string) error {
	if len(cursor) > 64<<10 || !utf8.ValidString(cursor) || strings.ContainsAny(cursor, "\r\n\x00") {
		return errors.New("gdrive: unsafe page token")
	}
	return nil
}

// List returns one page of a directory. Workspace documents and shortcuts are
// skipped: they have no byte stream to mount.
func (p *Provider) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	requestURL, err := p.listQuery(dirID, cursor)
	if err != nil {
		return nil, "", err
	}
	if dirID == RootID {
		if _, err := p.rootActual(ctx); err != nil {
			return nil, "", err
		}
	}
	var page filePage
	if err := p.apiJSON(ctx, http.MethodGet, requestURL, nil, &page, ratelimit.Meta, true); err != nil {
		return nil, "", err
	}
	entries := make([]provider.Entry, 0, len(page.Files))
	seen := make(map[string]struct{}, len(page.Files))
	for _, f := range page.Files {
		if !f.representable() {
			continue
		}
		e, err := p.entryFrom(f, dirID)
		if err != nil {
			return nil, "", err
		}
		if _, dup := seen[e.Name]; dup {
			return nil, "", duplicateNameError(dirID, e.Name)
		}
		seen[e.Name] = struct{}{}
		entries = append(entries, e)
	}
	if page.NextPageToken != "" {
		if err := safeCursor(page.NextPageToken); err != nil {
			return nil, "", err
		}
		return entries, page.NextPageToken, nil
	}
	return entries, "", nil
}

// duplicateNameError reports a collision the metadata store cannot represent.
// Dropping one of the two files would hide user data, and renaming one would
// invent a name that does not exist on the backend, so the directory fails
// with an actionable message instead.
func duplicateNameError(dirID, name string) error {
	return fmt.Errorf("gdrive: directory %q holds more than one item named %q; Google Drive permits this but a filesystem cannot. Rename one of them in Drive", dirID, name)
}

// ListStream enumerates a whole directory without retaining its entries. The
// set of names it does retain is what lets a cross-page duplicate be reported
// rather than reaching the metadata store as a poisoned listing.
func (p *Provider) ListStream(ctx context.Context, dirID string, visit func(provider.Entry) error) error {
	if visit == nil {
		return errors.New("gdrive: list visitor is nil")
	}
	seen := make(map[string]struct{})
	cursors := map[string]struct{}{"": {}}
	cursor := ""
	for pages := 0; pages < maxListPages; pages++ {
		entries, next, err := p.List(ctx, dirID, cursor)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if _, dup := seen[e.Name]; dup {
				return duplicateNameError(dirID, e.Name)
			}
			seen[e.Name] = struct{}{}
			if err := visit(e); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		if _, repeated := cursors[next]; repeated {
			return errors.New("gdrive: repeated directory page token")
		}
		cursors[next] = struct{}{}
		cursor = next
	}
	return errors.New("gdrive: directory listing exceeds 10000 pages")
}

// Quota implements provider.Quotaer from the About resource. Google
// reports "limit" only for accounts with one; an unlimited account has no
// limit and is reported as unknown, which a pool treats as "do not prefer
// by space".
func (p *Provider) Quota(ctx context.Context) (provider.Quota, error) {
	var about struct {
		StorageQuota struct {
			Limit string `json:"limit"`
			Usage string `json:"usage"`
		} `json:"storageQuota"`
	}
	if err := p.apiJSON(ctx, http.MethodGet, p.apiURL("/about")+"?fields=storageQuota", nil, &about, ratelimit.Meta, true); err != nil {
		return provider.Quota{}, err
	}
	var q provider.Quota
	q.Total, _ = strconv.ParseInt(about.StorageQuota.Limit, 10, 64)
	q.Used, _ = strconv.ParseInt(about.StorageQuota.Usage, 10, 64)
	return q, nil
}

func (p *Provider) getFile(ctx context.Context, id string) (driveFile, error) {
	target := id
	if id == RootID {
		target = "root"
		if p.driveID != "" {
			target = p.driveID
		}
	} else if err := safeID(id); err != nil {
		return driveFile{}, fmt.Errorf("gdrive: invalid file id: %w", err)
	}
	q := url.Values{"fields": {fileFields}, "supportsAllDrives": {"true"}}
	var f driveFile
	err := p.apiJSON(ctx, http.MethodGet, p.apiURL("/files/"+url.PathEscape(target))+"?"+q.Encode(), nil, &f, ratelimit.Meta, true)
	return f, err
}

func (p *Provider) Stat(ctx context.Context, id string) (provider.Entry, error) {
	f, err := p.getFile(ctx, id)
	if err != nil {
		return provider.Entry{}, err
	}
	if id == RootID {
		if !f.isFolder() {
			return provider.Entry{}, errors.New("gdrive: drive root is not a folder")
		}
		p.mu.Lock()
		if p.rootID == "" && safeID(f.ID) == nil {
			p.rootID = f.ID
		}
		p.mu.Unlock()
		e := provider.Entry{ID: RootID, Kind: provider.KindDir, Version: f.Version}
		provider.EnsureVersion(&e)
		return e, nil
	}
	if f.Trashed {
		return provider.Entry{}, fmt.Errorf("%w: gdrive item is in the trash", provider.ErrNotFound)
	}
	return p.entryFrom(f, "")
}

// ReadRange downloads a byte window. When the caller names a version the read
// is pinned to that revision, so a concurrent remote edit cannot deliver newer
// bytes that the block cache would then store under the older key.
func (p *Provider) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	if id == RootID || off < 0 || n < 0 {
		return nil, errors.New("gdrive: invalid file range")
	}
	if err := safeID(id); err != nil {
		return nil, fmt.Errorf("gdrive: invalid file id: %w", err)
	}
	if version != "" && safeID(version) == nil {
		body, err := p.download(ctx, p.revisionURL(id, version), off, n)
		if err == nil {
			return body, nil
		}
		// A revision can be purged once the file has newer content. Fall back
		// to the head, but only after confirming the head is still the version
		// the caller asked for.
		if !errors.Is(err, provider.ErrNotFound) && !errors.Is(err, provider.ErrLinkExpired) {
			return nil, err
		}
		e, statErr := p.Stat(ctx, id)
		if statErr != nil {
			return nil, statErr
		}
		if e.Version != version {
			return nil, fmt.Errorf("%w: gdrive revision %q is gone and the file has changed", provider.ErrConflict, version)
		}
	} else if version != "" {
		// The version is not a revision id (it came from EnsureVersion), so it
		// cannot pin the download. Confirm the head still matches first.
		e, err := p.Stat(ctx, id)
		if err != nil {
			return nil, err
		}
		if e.Version != version {
			return nil, fmt.Errorf("%w: gdrive file version changed", provider.ErrConflict)
		}
	}
	return p.download(ctx, p.mediaURL(id), off, n)
}

func (p *Provider) mediaURL(id string) string {
	q := url.Values{"alt": {"media"}, "supportsAllDrives": {"true"}}
	return p.apiURL("/files/"+url.PathEscape(id)) + "?" + q.Encode()
}

// UNVERIFIED: the revision media endpoint is documented for binary files, but
// which revisions stay downloadable (Drive keeps only the head forever unless
// keepForever is set) has not been observed on a real account. If a revision
// is purged the read falls back to the head after re-checking the version.
func (p *Provider) revisionURL(id, revision string) string {
	q := url.Values{"alt": {"media"}}
	return p.apiURL("/files/"+url.PathEscape(id)+"/revisions/"+url.PathEscape(revision)) + "?" + q.Encode()
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

// DownloadURL has no implementation on Drive: private content is served only
// to an authenticated request, so there is no URL to hand to a third party.
func (p *Provider) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	return provider.Link{}, fmt.Errorf("%w: gdrive serves content only to authenticated requests", provider.ErrUnsupported)
}

func (p *Provider) validParentName(parentID, name string) error {
	if parentID != RootID {
		if err := safeID(parentID); err != nil {
			return fmt.Errorf("gdrive: invalid parent id: %w", err)
		}
	}
	if !safeName(name) {
		return fmt.Errorf("gdrive: unsafe child name %q", name)
	}
	return nil
}

// resolveParent turns the root alias into the id Drive expects in a request
// body, since "root" is only accepted in a path or a query expression.
func (p *Provider) resolveParent(ctx context.Context, parentID string) (string, error) {
	if parentID != RootID {
		return parentID, nil
	}
	return p.rootActual(ctx)
}

// PutFile stores a small file in one multipart/related request.
func (p *Provider) PutFile(ctx context.Context, parentID, name string, r io.Reader, size int64, _ provider.Hashes) (provider.Entry, error) {
	if err := p.validParentName(parentID, name); err != nil {
		return provider.Entry{}, err
	}
	if size < 0 || size > singlePutMax {
		return provider.Entry{}, errors.New("gdrive: single upload size is outside the supported range")
	}
	content, err := readExact(r, size, singlePutMax)
	if err != nil {
		return provider.Entry{}, err
	}
	parent, err := p.resolveParent(ctx, parentID)
	if err != nil {
		return provider.Entry{}, err
	}
	// Drive would happily create a second child with the same name. Replace
	// the existing one instead so the caller's "put this name" means what it
	// says on every backend.
	existing, err := p.childByName(ctx, parentID, name)
	if err != nil {
		return provider.Entry{}, err
	}
	body, contentType, err := multipartBody(map[string]any{"name": name, "parents": []string{parent}}, content)
	if err != nil {
		return provider.Entry{}, err
	}
	method, target := http.MethodPost, p.uploadURL("/files")
	if existing != "" {
		// An update must not repeat parents; Drive rejects the combination.
		body, contentType, err = multipartBody(map[string]any{"name": name}, content)
		if err != nil {
			return provider.Entry{}, err
		}
		method, target = http.MethodPatch, p.uploadURL("/files/"+url.PathEscape(existing))
	}
	q := p.driveScope(url.Values{"uploadType": {"multipart"}, "fields": {fileFields}})
	q.Del("driveId")
	q.Del("corpora")
	q.Del("includeItemsFromAllDrives")
	var f driveFile
	if err := p.apiBytes(ctx, method, target+"?"+q.Encode(), body, contentType, &f, ratelimit.Upload); err != nil {
		return provider.Entry{}, err
	}
	return p.entryFrom(f, parentID)
}

// childByName returns the id of the single non-trashed child with that name,
// or "" when there is none. More than one is the duplicate-name case and is
// reported rather than resolved by guessing.
func (p *Provider) childByName(ctx context.Context, parentID, name string) (string, error) {
	target := parentID
	if parentID == RootID {
		target = "root"
	}
	q := p.driveScope(url.Values{
		"q":        {"'" + target + "' in parents and name = " + quoteLiteral(name) + " and trashed = false"},
		"fields":   {"files(id,mimeType)"},
		"pageSize": {"2"},
		"spaces":   {"drive"},
	})
	var page filePage
	if err := p.apiJSON(ctx, http.MethodGet, p.apiURL("/files")+"?"+q.Encode(), nil, &page, ratelimit.Meta, true); err != nil {
		return "", err
	}
	if len(page.Files) == 0 {
		return "", nil
	}
	if len(page.Files) > 1 {
		return "", duplicateNameError(parentID, name)
	}
	f := page.Files[0]
	if err := safeID(f.ID); err != nil {
		return "", fmt.Errorf("gdrive: server returned invalid file id: %w", err)
	}
	if f.isFolder() {
		return "", fmt.Errorf("%w: gdrive %q is a folder", provider.ErrExists, name)
	}
	return f.ID, nil
}

// quoteLiteral renders a Drive query string literal. Only ' and \ are special.
func quoteLiteral(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		if r == '\'' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

func multipartBody(metadata map[string]any, content []byte) ([]byte, string, error) {
	meta, err := json.Marshal(metadata)
	if err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	metaPart, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	if err != nil {
		return nil, "", err
	}
	if _, err := metaPart.Write(meta); err != nil {
		return nil, "", err
	}
	dataPart, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}})
	if err != nil {
		return nil, "", err
	}
	if _, err := dataPart.Write(content); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "multipart/related; boundary=" + w.Boundary(), nil
}

// BeginUpload opens a resumable session. The session URI is what makes an
// interrupted upload resumable after a restart, so it is what gets persisted.
func (p *Provider) BeginUpload(ctx context.Context, parentID, name string, size int64, _ provider.Hashes) (provider.UploadSession, error) {
	if err := p.validParentName(parentID, name); err != nil {
		return provider.UploadSession{}, err
	}
	if size <= 0 {
		return provider.UploadSession{}, errors.New("gdrive: zero-byte files must use PutFile")
	}
	if size > maxDriveFileSize {
		return provider.UploadSession{}, errors.New("gdrive: upload exceeds the 5 TiB limit")
	}
	parent, err := p.resolveParent(ctx, parentID)
	if err != nil {
		return provider.UploadSession{}, err
	}
	existing, err := p.childByName(ctx, parentID, name)
	if err != nil {
		return provider.UploadSession{}, err
	}
	metadata := map[string]any{"name": name, "parents": []string{parent}}
	method, target := http.MethodPost, p.uploadURL("/files")
	if existing != "" {
		metadata = map[string]any{"name": name}
		method, target = http.MethodPatch, p.uploadURL("/files/"+url.PathEscape(existing))
	}
	q := url.Values{"uploadType": {"resumable"}, "fields": {fileFields}, "supportsAllDrives": {"true"}}
	body, err := json.Marshal(metadata)
	if err != nil {
		return provider.UploadSession{}, err
	}
	var location string
	err = p.authCall(ctx, func(token string) error {
		h := make(http.Header)
		h.Set("Authorization", "Bearer "+token)
		h.Set("Content-Type", "application/json; charset=UTF-8")
		h.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))
		h.Set("X-Upload-Content-Type", "application/octet-stream")
		resp, err := p.client.Do(ctx, httpx.Request{
			Method: method, URL: target + "?" + q.Encode(), Class: ratelimit.Upload, Header: h,
			Body: bytes.NewReader(body), ExpectStatus: []int{http.StatusOK}, Stream: true,
		})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxJSONResponse))
		location = resp.Header.Get("Location")
		return nil
	})
	if err != nil {
		return provider.UploadSession{}, err
	}
	if len(location) > 64<<10 || safeHTTPURL(location, true) != nil {
		return provider.UploadSession{}, errors.New("gdrive: resumable upload did not return a usable session URI")
	}
	return provider.UploadSession{ID: location, PartSize: p.partSize, Opaque: map[string]string{
		"session_uri": location, "parent_id": parentID, "name": name,
		"size": strconv.FormatInt(size, 10), "part_size": strconv.FormatInt(p.partSize, 10),
	}}, nil
}

func sessionState(s provider.UploadSession) (sessionURI, parentID, name string, size, partSize int64, err error) {
	sessionURI = s.Opaque["session_uri"]
	if sessionURI == "" {
		sessionURI = s.ID
	}
	if len(sessionURI) > 64<<10 || safeHTTPURL(sessionURI, true) != nil {
		err = errors.New("gdrive: upload session has an invalid session URI")
		return
	}
	parentID, name = s.Opaque["parent_id"], s.Opaque["name"]
	if parentID != RootID {
		if idErr := safeID(parentID); idErr != nil {
			err = errors.New("gdrive: upload session has an invalid parent id")
			return
		}
	}
	if !safeName(name) {
		err = errors.New("gdrive: upload session has an invalid name")
		return
	}
	size, err = strconv.ParseInt(s.Opaque["size"], 10, 64)
	if err != nil || size <= 0 || size > maxDriveFileSize {
		err = errors.New("gdrive: upload session has an invalid size")
		return
	}
	partSize = s.PartSize
	if partSize == 0 {
		partSize, err = strconv.ParseInt(s.Opaque["part_size"], 10, 64)
	}
	if err != nil || partSize <= 0 || partSize > maxPartSize || partSize%uploadChunkUnit != 0 {
		err = errors.New("gdrive: upload session has an invalid part size")
	}
	return
}

// UploadPart sends one chunk. Drive answers 308 while more is expected and
// 200/201 with the file resource on the final chunk, so the committed id is
// carried out on the last part's token.
func (p *Provider) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	sessionURI, parentID, _, size, partSize, err := sessionState(s)
	if err != nil {
		return provider.PartToken{}, err
	}
	if idx < 0 || int64(idx) > maxDriveFileSize/partSize {
		return provider.PartToken{}, errors.New("gdrive: invalid part index")
	}
	offset := int64(idx) * partSize
	want := partSize
	if remain := size - offset; remain < want {
		want = remain
	}
	if offset < 0 || offset >= size || n != want {
		return provider.PartToken{}, fmt.Errorf("gdrive: part %d has size %d, want %d", idx, n, want)
	}
	content, err := readExact(r, n, maxPartSize)
	if err != nil {
		return provider.PartToken{}, err
	}
	h := make(http.Header)
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(n, 10))
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+n-1, size))
	// A resumable session URI already carries its own credential, and the
	// session is not retried on an ambiguous failure: a replayed chunk at a
	// stale offset would corrupt the object.
	// UNVERIFIED: that PUTs to the session URI need no Authorization header
	// follows the published protocol but has not been confirmed against a real
	// account; if a live run returns 401 here, add the bearer token.
	resp, err := p.client.Do(ctx, httpx.Request{
		Method: http.MethodPut, URL: sessionURI, Class: ratelimit.Upload, Header: h,
		Body: bytes.NewReader(content), Stream: true,
		ExpectStatus: []int{http.StatusOK, http.StatusCreated, resumeIncomplete},
	})
	if err != nil {
		return provider.PartToken{}, mapUploadError(err)
	}
	defer resp.Body.Close()
	final := offset+n == size
	token := provider.PartToken{Index: idx, ETag: strconv.FormatInt(n, 10)}
	if !final {
		if resp.Status != resumeIncomplete {
			return provider.PartToken{}, fmt.Errorf("gdrive: chunk %d was answered with HTTP %d before the upload was complete", idx, resp.Status)
		}
		if !rangeEndsAt(resp.Header.Get("Range"), offset+n) {
			return provider.PartToken{}, fmt.Errorf("gdrive: chunk %d was not acknowledged up to offset %d", idx, offset+n)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxJSONResponse))
		return token, nil
	}
	if resp.Status == resumeIncomplete {
		return provider.PartToken{}, errors.New("gdrive: final chunk did not complete the upload")
	}
	var f driveFile
	if err := decodeJSON(resp.Body, &f); err != nil {
		return provider.PartToken{}, err
	}
	e, err := p.entryFrom(f, parentID)
	if err != nil {
		return provider.PartToken{}, err
	}
	if e.Kind != provider.KindFile || e.Size != size {
		return provider.PartToken{}, errors.New("gdrive: final chunk returned mismatched file metadata")
	}
	token.ETag += ":" + base64.RawURLEncoding.EncodeToString([]byte(e.ID))
	return token, nil
}

// resumeIncomplete is the status Drive uses for "chunk stored, send more".
const resumeIncomplete = 308

// rangeEndsAt checks Drive's acknowledgement header, which is inclusive:
// "bytes=0-262143" means the next byte expected is 262144.
func rangeEndsAt(header string, next int64) bool {
	value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header), "bytes="))
	_, end, ok := strings.Cut(value, "-")
	if !ok {
		return false
	}
	last, err := strconv.ParseInt(strings.TrimSpace(end), 10, 64)
	return err == nil && last == next-1
}

func (p *Provider) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	_, _, _, size, partSize, err := sessionState(s)
	if err != nil {
		return provider.Entry{}, err
	}
	expected := int((size + partSize - 1) / partSize)
	if len(parts) != expected {
		return provider.Entry{}, fmt.Errorf("gdrive: upload has %d parts, want %d", len(parts), expected)
	}
	seen := make([]bool, expected)
	finalID := ""
	for _, part := range parts {
		if part.Index < 0 || part.Index >= expected || seen[part.Index] {
			return provider.Entry{}, errors.New("gdrive: upload parts must be unique and contiguous")
		}
		seen[part.Index] = true
		fields := strings.SplitN(part.ETag, ":", 2)
		length, parseErr := strconv.ParseInt(fields[0], 10, 64)
		want := partSize
		if remain := size - int64(part.Index)*partSize; remain < want {
			want = remain
		}
		if parseErr != nil || length != want {
			return provider.Entry{}, fmt.Errorf("gdrive: part %d token has the wrong length", part.Index)
		}
		if part.Index == expected-1 && len(fields) == 2 {
			decoded, decodeErr := base64.RawURLEncoding.DecodeString(fields[1])
			if decodeErr == nil {
				finalID = string(decoded)
			}
		}
	}
	if finalID == "" || safeID(finalID) != nil {
		return provider.Entry{}, errors.New("gdrive: final part token has no committed file id")
	}
	e, err := p.Stat(ctx, finalID)
	if err != nil {
		return provider.Entry{}, err
	}
	if e.Kind != provider.KindFile || e.Size != size {
		return provider.Entry{}, errors.New("gdrive: committed file does not match the upload")
	}
	return e, nil
}

func (p *Provider) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	if err := p.validParentName(parentID, name); err != nil {
		return provider.Entry{}, err
	}
	parent, err := p.resolveParent(ctx, parentID)
	if err != nil {
		return provider.Entry{}, err
	}
	existing, err := p.childByName(ctx, parentID, name)
	if err != nil && !errors.Is(err, provider.ErrExists) {
		return provider.Entry{}, err
	}
	if existing != "" || errors.Is(err, provider.ErrExists) {
		return provider.Entry{}, fmt.Errorf("%w: gdrive %q already exists", provider.ErrExists, name)
	}
	q := url.Values{"fields": {fileFields}, "supportsAllDrives": {"true"}}
	var f driveFile
	if err := p.apiJSON(ctx, http.MethodPost, p.apiURL("/files")+"?"+q.Encode(), map[string]any{
		"name": name, "mimeType": folderMime, "parents": []string{parent},
	}, &f, ratelimit.Meta, false); err != nil {
		return provider.Entry{}, err
	}
	return p.entryFrom(f, parentID)
}

func (p *Provider) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	if id == RootID || safeID(id) != nil || !safeName(newName) {
		return provider.Entry{}, errors.New("gdrive: invalid rename")
	}
	return p.patchFile(ctx, id, nil, map[string]any{"name": newName})
}

func (p *Provider) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	if id == RootID || safeID(id) != nil {
		return provider.Entry{}, errors.New("gdrive: invalid move source")
	}
	parent, err := p.resolveParent(ctx, newParentID)
	if err != nil {
		return provider.Entry{}, err
	}
	if safeID(parent) != nil {
		return provider.Entry{}, errors.New("gdrive: invalid move destination")
	}
	// Drive moves by editing the parent set, so the current parents have to be
	// named explicitly or the file would end up linked under both.
	current, err := p.getFile(ctx, id)
	if err != nil {
		return provider.Entry{}, err
	}
	for _, existing := range current.Parents {
		if err := safeID(existing); err != nil {
			return provider.Entry{}, fmt.Errorf("gdrive: file %q has an invalid parent id: %w", id, err)
		}
	}
	extra := url.Values{"addParents": {parent}}
	if len(current.Parents) > 0 {
		extra.Set("removeParents", strings.Join(current.Parents, ","))
	}
	return p.patchFile(ctx, id, extra, map[string]any{})
}

func (p *Provider) patchFile(ctx context.Context, id string, extra url.Values, body map[string]any) (provider.Entry, error) {
	q := url.Values{"fields": {fileFields}, "supportsAllDrives": {"true"}}
	for key, values := range extra {
		q[key] = values
	}
	var f driveFile
	if err := p.apiJSON(ctx, http.MethodPatch, p.apiURL("/files/"+url.PathEscape(id))+"?"+q.Encode(), body, &f, ratelimit.Meta, false); err != nil {
		return provider.Entry{}, err
	}
	return p.entryFrom(f, "")
}

// Copy is Drive's server-side copy: no bytes travel through this process.
func (p *Provider) Copy(ctx context.Context, id, newParentID, newName string) (provider.Entry, error) {
	if id == RootID || safeID(id) != nil || !safeName(newName) {
		return provider.Entry{}, errors.New("gdrive: invalid copy request")
	}
	parent, err := p.resolveParent(ctx, newParentID)
	if err != nil {
		return provider.Entry{}, err
	}
	if safeID(parent) != nil {
		return provider.Entry{}, errors.New("gdrive: invalid copy destination")
	}
	q := url.Values{"fields": {fileFields}, "supportsAllDrives": {"true"}}
	var f driveFile
	if err := p.apiJSON(ctx, http.MethodPost, p.apiURL("/files/"+url.PathEscape(id)+"/copy")+"?"+q.Encode(), map[string]any{
		"name": newName, "parents": []string{parent},
	}, &f, ratelimit.Meta, false); err != nil {
		return provider.Entry{}, err
	}
	return p.entryFrom(f, newParentID)
}

func (p *Provider) Delete(ctx context.Context, id string) error {
	if id == RootID || safeID(id) != nil {
		return errors.New("gdrive: refusing to delete an invalid file or the drive root")
	}
	q := url.Values{"supportsAllDrives": {"true"}}
	return p.apiJSON(ctx, http.MethodDelete, p.apiURL("/files/"+url.PathEscape(id))+"?"+q.Encode(), nil, nil, ratelimit.Meta, false)
}

type changePage struct {
	Changes []struct {
		FileID  string     `json:"fileId"`
		Removed bool       `json:"removed"`
		File    *driveFile `json:"file"`
	} `json:"changes"`
	NextPageToken     string `json:"nextPageToken"`
	NewStartPageToken string `json:"newStartPageToken"`
}

// Changes walks Drive's change feed. An empty cursor establishes a baseline
// and reports it as a reset rather than replaying the whole drive.
func (p *Provider) Changes(ctx context.Context, cursor string) ([]provider.Change, string, error) {
	root, err := p.rootActual(ctx)
	if err != nil {
		return nil, "", err
	}
	if cursor == "" {
		return p.newDeltaBaseline(ctx)
	}
	if err := safeCursor(cursor); err != nil {
		return nil, "", err
	}
	q := p.driveScope(url.Values{
		"pageToken":      {cursor},
		"fields":         {"nextPageToken,newStartPageToken,changes(fileId,removed,file(" + fileFields + "))"},
		"pageSize":       {"1000"},
		"includeRemoved": {"true"},
		"spaces":         {"drive"},
	})
	var page changePage
	if err := p.apiJSON(ctx, http.MethodGet, p.apiURL("/changes")+"?"+q.Encode(), nil, &page, ratelimit.Meta, true); err != nil {
		if isCursorReset(err) {
			return p.newDeltaBaseline(ctx)
		}
		return nil, "", err
	}
	next := first(page.NextPageToken, page.NewStartPageToken)
	if next == "" || safeCursor(next) != nil {
		return nil, "", errors.New("gdrive: change feed returned no usable continuation token")
	}
	changes := make([]provider.Change, 0, len(page.Changes))
	for _, c := range page.Changes {
		if err := safeID(c.FileID); err != nil {
			return nil, "", errors.New("gdrive: change feed returned an invalid file id")
		}
		if c.FileID == root {
			continue
		}
		if c.Removed || c.File == nil || c.File.Trashed {
			changes = append(changes, provider.Change{Op: provider.ChangeDelete, ID: c.FileID})
			continue
		}
		if !c.File.representable() {
			continue
		}
		e, err := p.entryFrom(*c.File, "")
		if err != nil {
			return nil, "", err
		}
		entry := e
		changes = append(changes, provider.Change{Op: provider.ChangeUpsert, ID: e.ID, ParentID: e.ParentID, Entry: &entry})
	}
	return changes, next, nil
}

func (p *Provider) newDeltaBaseline(ctx context.Context) ([]provider.Change, string, error) {
	q := p.driveScope(url.Values{})
	q.Del("corpora")
	q.Del("includeItemsFromAllDrives")
	var out struct {
		StartPageToken string `json:"startPageToken"`
	}
	if err := p.apiJSON(ctx, http.MethodGet, p.apiURL("/changes/startPageToken")+"?"+q.Encode(), nil, &out, ratelimit.Meta, true); err != nil {
		return nil, "", err
	}
	if out.StartPageToken == "" || safeCursor(out.StartPageToken) != nil {
		return nil, "", errors.New("gdrive: startPageToken is missing or unsafe")
	}
	return nil, "", &provider.CursorResetError{Cursor: out.StartPageToken}
}

func readExact(r io.Reader, n, max int64) ([]byte, error) {
	if n < 0 || n > max || (r == nil && n != 0) {
		return nil, errors.New("gdrive: invalid request body")
	}
	if r == nil {
		return []byte{}, nil
	}
	b, err := io.ReadAll(io.LimitReader(r, n+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != n {
		return nil, fmt.Errorf("gdrive: request body has %d bytes, want %d", len(b), n)
	}
	return b, nil
}

func (p *Provider) token(ctx context.Context) (string, error) {
	if err := p.FlushTokens(); err != nil {
		return "", fmt.Errorf("gdrive: save rotated credentials: %w", err)
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
	current, expiry, rt := p.accessToken, p.expiresAt, p.refreshToken
	p.mu.Unlock()
	if current != "" && current != stale && (expiry.IsZero() || p.now().Add(time.Minute).Before(expiry)) {
		return current, nil
	}
	if rt == "" {
		return "", fmt.Errorf("%w: gdrive has no refresh_token", provider.ErrAuth)
	}
	form := map[string]string{"grant_type": "refresh_token", "refresh_token": rt, "client_id": p.clientID}
	if p.clientSecret != "" {
		form["client_secret"] = p.clientSecret
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := p.doJSON(ctx, httpx.Request{Method: http.MethodPost, URL: p.tokenURL, Class: ratelimit.Meta, Form: form}, &out); err != nil || out.AccessToken == "" {
		return "", refreshFailure(err)
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
			return "", fmt.Errorf("gdrive: save rotated refresh token: %w", err)
		}
	}
	return out.AccessToken, nil
}

// refreshFailure turns a refused token refresh into an error someone can act
// on.
//
// invalid_grant is the one worth naming. Google issues it when the refresh
// token is no longer valid, and by far the most common cause on a drive that
// worked yesterday is an OAuth application still in Testing: there,
// authorization expires seven days after consent. The token is not corrupt
// and re-authorizing works — for another seven days — so without this the
// error sends people round that loop indefinitely, reading "token refresh
// failed" every week.
func refreshFailure(err error) error {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		var envelope struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(se.Body), &envelope)
		if strings.EqualFold(envelope.Error, "invalid_grant") {
			return fmt.Errorf("%w: gdrive refused the refresh token (invalid_grant). "+
				"If the OAuth application is still in Testing, authorization expires 7 days after consent; "+
				"publish it to Production and authorize again", provider.ErrAuth)
		}
	}
	return fmt.Errorf("%w: gdrive token refresh failed", provider.ErrAuth)
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
				req.Header.Set("Content-Type", "application/json; charset=UTF-8")
			}
		} else if !retryable && method != http.MethodGet && method != http.MethodHead {
			// A state-changing call must not be replayed after an ambiguous
			// transport failure; without a body httpx would consider it safe.
			req.Body = bytes.NewReader(nil)
		}
		return p.doJSON(ctx, req, out)
	})
}

func (p *Provider) apiBytes(ctx context.Context, method, rawURL string, body []byte, contentType string, out any, class ratelimit.Class) error {
	return p.authCall(ctx, func(token string) error {
		h := make(http.Header)
		h.Set("Authorization", "Bearer "+token)
		h.Set("Content-Type", contentType)
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
			return errors.New("gdrive: API response exceeds 16 MiB")
		}
		return nil
	}
	return decodeJSON(resp.Body, out)
}

func decodeJSON(r io.Reader, out any) error {
	dec := json.NewDecoder(io.LimitReader(r, maxJSONResponse+1))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("gdrive: decode API response: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("gdrive: API response contains trailing JSON")
		}
		return fmt.Errorf("gdrive: oversized or malformed API response: %w", err)
	}
	return nil
}

type apiError struct {
	status int
	reason string
	kind   error
}

func (e *apiError) Error() string {
	return fmt.Sprintf("gdrive: Drive API HTTP %d (%s)", e.status, e.reason)
}
func (e *apiError) Unwrap() error { return e.kind }

// mapError turns a Drive error envelope into the sentinel the retry and
// circuit-breaker layers understand. The reason string matters as much as the
// status: 403 is both "quota exceeded, back off" and "forbidden, give up".
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
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
			Status string `json:"status"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(se.Body), &envelope)
	reason := ""
	if len(envelope.Error.Errors) > 0 {
		reason = strings.ToLower(envelope.Error.Errors[0].Reason)
	}
	if reason == "" {
		reason = strings.ToLower(envelope.Error.Status)
	}
	var kind error
	switch {
	case se.Code == 401:
		kind = provider.ErrAuth
	case se.Code == 403 && reason == "storagequotaexceeded":
		// The account has no room left. This is not a rate limit and not a
		// permission problem: retrying the same Drive can never succeed, so
		// it has to be told apart from every other 403.
		//
		// UNVERIFIED: Drive is documented to answer 403 with the error reason
		// "storageQuotaExceeded" when the account is full; confirm the exact
		// reason string against a real full account.
		kind = provider.ErrQuotaExceeded
	case se.Code == 403 && isQuotaReason(reason):
		kind = provider.ErrRateLimited
	case se.Code == 403:
		kind = provider.ErrAuth
	case se.Code == 404 || reason == "notfound":
		kind = provider.ErrNotFound
	case se.Code == 409 || reason == "duplicate":
		kind = provider.ErrExists
	case se.Code == 412:
		kind = provider.ErrConflict
	case se.Code == 429:
		kind = provider.ErrRateLimited
	case se.Code == 410 || reason == "pagetokeninvalid" || reason == "invalidpagetoken":
		kind = provider.ErrCursorReset
	case se.Code >= 500:
		kind = provider.ErrTransient
	default:
		return err
	}
	return &apiError{status: se.Code, reason: reason, kind: kind}
}

// UNVERIFIED: the exact reason strings Drive returns with 403 have not been
// observed on a real account. Treating an unlisted 403 as ErrAuth is the safe
// side: it stops rather than retries into a quota ban.
func isQuotaReason(reason string) bool {
	switch reason {
	case "userratelimitexceeded", "ratelimitexceeded", "quotaexceeded",
		"sharingratelimitexceeded", "dailylimitexceeded":
		return true
	}
	return false
}

func mapUploadError(err error) error {
	var se *httpx.StatusError
	if errors.As(err, &se) && (se.Code == http.StatusNotFound || se.Code == http.StatusGone) {
		return fmt.Errorf("%w: gdrive resumable upload session expired", provider.ErrLinkExpired)
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
	_ provider.ServerCopier           = (*Provider)(nil)
	_ provider.ChangeLister           = (*Provider)(nil)
	_ provider.TokenPersistenceSetter = (*Provider)(nil)
)

// Sharing (provider.Sharer, T-55). UNVERIFIED: a public link on Drive is
// a permission {type: anyone, role: reader} on the file (POST
// /files/{id}/permissions), after which the file's webViewLink opens for
// anyone; the permission id is what DELETE
// /files/{id}/permissions/{permId} revokes, so the share id is
// "<file>/<permission>". Drive has no per-link expiry or password
// (expirationTime on a permission needs Workspace) — verify what a real
// account accepts.

func (p *Provider) CreateShare(ctx context.Context, id string, opt provider.ShareOptions) (provider.Share, error) {
	if id == RootID || safeID(id) != nil {
		return provider.Share{}, errors.New("gdrive: refusing to share an invalid file or the drive root")
	}
	body := map[string]any{"type": "anyone", "role": "reader"}
	if opt.Expires > 0 {
		body["expirationTime"] = time.Now().Add(opt.Expires).UTC().Format(time.RFC3339)
	}
	var perm struct {
		ID             string `json:"id"`
		ExpirationTime string `json:"expirationTime"`
	}
	q := url.Values{"supportsAllDrives": {"true"}}
	if err := p.apiJSON(ctx, http.MethodPost, p.apiURL("/files/"+url.PathEscape(id)+"/permissions")+"?"+q.Encode(), body, &perm, ratelimit.Meta, false); err != nil {
		return provider.Share{}, err
	}
	var file struct {
		WebViewLink string `json:"webViewLink"`
	}
	fq := url.Values{"fields": {"webViewLink"}, "supportsAllDrives": {"true"}}
	if err := p.apiJSON(ctx, http.MethodGet, p.apiURL("/files/"+url.PathEscape(id))+"?"+fq.Encode(), nil, &file, ratelimit.Meta, true); err != nil {
		return provider.Share{}, err
	}
	sh := provider.Share{ID: id + "/" + perm.ID, URL: file.WebViewLink}
	if t, err := time.Parse(time.RFC3339, perm.ExpirationTime); err == nil {
		sh.ExpiresAt = t
	}
	return sh, nil
}

func (p *Provider) RevokeShare(ctx context.Context, shareID string) error {
	file, perm, ok := strings.Cut(shareID, "/")
	if !ok || safeID(file) != nil || safeID(perm) != nil {
		return errors.New("gdrive: share id is <file>/<permission>")
	}
	q := url.Values{"supportsAllDrives": {"true"}}
	return p.apiJSON(ctx, http.MethodDelete, p.apiURL("/files/"+url.PathEscape(file)+"/permissions/"+url.PathEscape(perm))+"?"+q.Encode(), nil, nil, ratelimit.Meta, false)
}

var _ provider.Sharer = (*Provider)(nil)
