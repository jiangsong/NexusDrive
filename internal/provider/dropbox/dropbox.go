// Package dropbox implements the official Dropbox HTTP API v2 provider.
// Dropbox paths are used as provider IDs because they make parent identity
// deterministic across paginated listings; the API's stable object id remains
// available internally but is not suitable for constructing destination paths.
package dropbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
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
	RootID              = "/"
	defaultAPIBase      = "https://api.dropboxapi.com"
	defaultContentBase  = "https://content.dropboxapi.com"
	defaultOAuthURL     = "https://api.dropboxapi.com/oauth2/token"
	defaultPartSize     = 8 << 20
	defaultSinglePutMax = 64 << 20
	maxRequestBody      = 150 << 20
	maxDropboxFile      = (int64(1) << 41) - (int64(1) << 22)
	maxJSONResponse     = 16 << 20
)

type Options struct {
	Name         string
	APIBase      string
	ContentBase  string
	OAuthURL     string
	AccessToken  string
	RefreshToken string
	ClientID     string
	ClientSecret string
	PartSize     int64
	Client       *httpx.Client
	Now          func() time.Time
}

type Provider struct {
	name, apiBase, contentBase, oauthURL string
	refreshToken, clientID, clientSecret string
	partSize                             int64
	client                               *httpx.Client
	caps                                 provider.Caps
	now                                  func() time.Time

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
	refreshMu   sync.Mutex
}

func New(opt Options) (*Provider, error) {
	if opt.Client == nil {
		return nil, errors.New("dropbox: Client is required")
	}
	apiBase, err := cleanOrigin(opt.APIBase, defaultAPIBase, "api_base")
	if err != nil {
		return nil, err
	}
	contentBase, err := cleanOrigin(opt.ContentBase, defaultContentBase, "content_base")
	if err != nil {
		return nil, err
	}
	oauthURL := strings.TrimSpace(opt.OAuthURL)
	if oauthURL == "" {
		oauthURL = defaultOAuthURL
	}
	if err := safeAbsoluteURL(oauthURL); err != nil {
		return nil, fmt.Errorf("dropbox: invalid oauth_url: %w", err)
	}
	if u, _ := url.Parse(oauthURL); u.RawQuery != "" {
		return nil, errors.New("dropbox: oauth_url cannot contain a query")
	}
	for name, value := range map[string]string{
		"access_token": opt.AccessToken, "refresh_token": opt.RefreshToken,
		"client_id": opt.ClientID, "client_secret": opt.ClientSecret,
	} {
		if len(value) > 16<<10 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("dropbox: %s contains an unsafe character or is too large", name)
		}
	}
	if opt.AccessToken == "" && opt.RefreshToken == "" {
		return nil, errors.New("dropbox: access_token or refresh_token is required")
	}
	if opt.RefreshToken != "" && opt.ClientID == "" {
		return nil, errors.New("dropbox: client_id is required with refresh_token")
	}
	partSize := opt.PartSize
	if partSize == 0 {
		partSize = defaultPartSize
	}
	if partSize < 1 || partSize > maxRequestBody {
		return nil, errors.New("dropbox: part_size must be between 1 byte and 150 MiB")
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	p := &Provider{
		name: opt.Name, apiBase: apiBase, contentBase: contentBase, oauthURL: oauthURL,
		accessToken: opt.AccessToken, refreshToken: opt.RefreshToken,
		clientID: opt.ClientID, clientSecret: opt.ClientSecret,
		partSize: partSize, client: opt.Client, now: now,
	}
	p.caps = provider.Caps{
		// Naming: what the drive refuses in a name, so a pool never places
		// a replica the drive would then reject.
		Naming:    provider.Naming{CaseInsensitive: true, MaxNameBytes: 255, ForbiddenRunes: "\\", NoTrailingDotSpace: true},
		RangeRead: true, StreamList: true,
		PartSize: partSize, MaxParts: int((maxDropboxFile + partSize - 1) / partSize),
		UploadParallel: 1, SinglePutMax: defaultSinglePutMax,
		ServerMove: true, ServerRename: true, ServerCopy: true, Delta: true,
		LinkTTL: 4 * time.Hour, LinkShareable: true,
		QPS: provider.QPS{Meta: 12, Download: 12, Upload: 6}, MaxConnsPerHost: 12,
		Tier: provider.TierOfficial,
	}
	return p, nil
}

func cleanOrigin(raw, fallback, field string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = fallback
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("dropbox: %s must be an http(s) origin without path, credentials, query, or fragment", field)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func safeAbsoluteURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return errors.New("must be an http(s) URL without credentials or fragment")
	}
	return nil
}

func (p *Provider) Name() string                { return p.name }
func (p *Provider) RootID() string              { return RootID }
func (p *Provider) Capabilities() provider.Caps { return p.caps }

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func cleanID(raw string) (string, error) {
	if raw == "" || raw == RootID {
		return RootID, nil
	}
	if !utf8.ValidString(raw) || !strings.HasPrefix(raw, "/") || strings.ContainsAny(raw, "\\\x00") || hasControl(raw) {
		return "", errors.New("dropbox: object id is not a safe absolute path")
	}
	clean := path.Clean(raw)
	if clean != raw || clean == RootID {
		return "", errors.New("dropbox: object id must be canonical and cannot contain dot segments, repeated separators, or a trailing slash")
	}
	return clean, nil
}

func safeName(name string) bool {
	return name != "" && name != "." && name != ".." && utf8.ValidString(name) &&
		!strings.ContainsAny(name, "/\\\x00") && !hasControl(name)
}

func childID(parent, name string) (string, error) {
	parent, err := cleanID(parent)
	if err != nil {
		return "", err
	}
	if !safeName(name) {
		return "", fmt.Errorf("dropbox: unsafe child name %q", name)
	}
	return path.Join(parent, name), nil
}

func apiPath(id string) string {
	if id == RootID {
		return ""
	}
	return id
}

type metadata struct {
	Tag            string `json:".tag"`
	ID             string `json:"id"`
	Name           string `json:"name"`
	PathLower      string `json:"path_lower"`
	PathDisplay    string `json:"path_display"`
	ClientModified string `json:"client_modified"`
	ServerModified string `json:"server_modified"`
	Rev            string `json:"rev"`
	Size           int64  `json:"size"`
	IsDownloadable *bool  `json:"is_downloadable"`
}

func entryFrom(m metadata) (provider.Entry, error) {
	if m.Tag != "file" && m.Tag != "folder" {
		return provider.Entry{}, fmt.Errorf("dropbox: unsupported metadata tag %q", m.Tag)
	}
	id, err := cleanID(m.PathLower)
	if err != nil || id == RootID {
		return provider.Entry{}, fmt.Errorf("dropbox: server returned an invalid path_lower %q", m.PathLower)
	}
	if !safeName(m.Name) {
		return provider.Entry{}, fmt.Errorf("dropbox: server returned an unsafe name %q", m.Name)
	}
	if !strings.EqualFold(path.Base(id), m.Name) {
		return provider.Entry{}, fmt.Errorf("dropbox: server returned name %q for mismatched path %q", m.Name, id)
	}
	e := provider.Entry{ID: id, ParentID: parentID(id), Name: m.Name, Kind: provider.KindDir}
	if m.Tag == "file" {
		if m.Size < 0 {
			return provider.Entry{}, errors.New("dropbox: server returned a negative file size")
		}
		if m.Rev == "" {
			return provider.Entry{}, errors.New("dropbox: server returned a file without a revision")
		}
		e.Kind, e.Size, e.Version = provider.KindFile, m.Size, m.Rev
		if m.ServerModified != "" {
			t, err := time.Parse(time.RFC3339, m.ServerModified)
			if err != nil {
				return provider.Entry{}, fmt.Errorf("dropbox: invalid server_modified for %q: %w", id, err)
			}
			e.ModTime = t
		}
		if m.IsDownloadable != nil && !*m.IsDownloadable {
			return provider.Entry{}, fmt.Errorf("%w: dropbox file %q is not downloadable", provider.ErrUnsupported, id)
		}
	}
	return e, nil
}

type listResult struct {
	Entries []metadata `json:"entries"`
	Cursor  string     `json:"cursor"`
	HasMore bool       `json:"has_more"`
}

func parentID(id string) string {
	parent := path.Dir(id)
	if parent == "." || parent == "" {
		return RootID
	}
	return parent
}

// Changes implements Dropbox's account-wide recursive change cursor. The
// first call establishes a latest-cursor baseline without replaying the whole
// account and reports a cursor reset so persisted directory listings are made
// stale before that baseline is adopted.
func (p *Provider) Changes(ctx context.Context, cursor string) ([]provider.Change, string, error) {
	if cursor == "" {
		latest, err := p.latestCursor(ctx)
		if err != nil {
			return nil, "", err
		}
		return nil, "", &provider.CursorResetError{Cursor: latest}
	}
	if len(cursor) > 16<<10 || strings.ContainsAny(cursor, "\r\n\x00") {
		return nil, "", errors.New("dropbox: invalid change cursor")
	}
	var out listResult
	err := p.apiJSON(ctx, "/2/files/list_folder/continue", map[string]string{"cursor": cursor}, &out, ratelimit.Meta, true)
	if err != nil {
		if isCursorReset(err) {
			latest, latestErr := p.latestCursor(ctx)
			if latestErr != nil {
				return nil, "", fmt.Errorf("dropbox: replace invalid change cursor: %w", latestErr)
			}
			return nil, "", &provider.CursorResetError{Cursor: latest}
		}
		return nil, "", err
	}
	if out.Cursor == "" {
		return nil, "", errors.New("dropbox: change response returned an empty cursor")
	}
	changes := make([]provider.Change, 0, len(out.Entries))
	for _, m := range out.Entries {
		if m.Tag == "deleted" {
			id, err := cleanID(m.PathLower)
			if err != nil || id == RootID || !safeName(m.Name) || !strings.EqualFold(path.Base(id), m.Name) {
				return nil, "", fmt.Errorf("dropbox: invalid deleted metadata path %q", m.PathLower)
			}
			changes = append(changes, provider.Change{Op: provider.ChangeDelete, ID: id, ParentID: parentID(id)})
			continue
		}
		e, err := entryFrom(m)
		if err != nil {
			return nil, "", err
		}
		entry := e
		changes = append(changes, provider.Change{Op: provider.ChangeUpsert, ID: e.ID, ParentID: e.ParentID, Entry: &entry})
	}
	return changes, out.Cursor, nil
}

func (p *Provider) latestCursor(ctx context.Context) (string, error) {
	var out struct {
		Cursor string `json:"cursor"`
	}
	err := p.apiJSON(ctx, "/2/files/list_folder/get_latest_cursor", map[string]any{
		"path": "", "recursive": true, "include_deleted": true,
		"include_non_downloadable_files": false,
	}, &out, ratelimit.Meta, true)
	if err != nil {
		return "", err
	}
	if out.Cursor == "" || len(out.Cursor) > 16<<10 || strings.ContainsAny(out.Cursor, "\r\n\x00") {
		return "", errors.New("dropbox: latest cursor response is invalid")
	}
	return out.Cursor, nil
}

func (p *Provider) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	dirID, err := cleanID(dirID)
	if err != nil {
		return nil, "", err
	}
	var out listResult
	if cursor == "" {
		err = p.apiJSON(ctx, "/2/files/list_folder", map[string]any{
			"path": apiPath(dirID), "recursive": false, "include_deleted": false,
			"include_non_downloadable_files": false, "limit": 2000,
		}, &out, ratelimit.Meta, true)
	} else {
		if len(cursor) > 16<<10 || strings.ContainsAny(cursor, "\r\n\x00") {
			return nil, "", errors.New("dropbox: invalid list cursor")
		}
		err = p.apiJSON(ctx, "/2/files/list_folder/continue", map[string]string{"cursor": cursor}, &out, ratelimit.Meta, true)
	}
	if err != nil {
		return nil, "", err
	}
	entries := make([]provider.Entry, 0, len(out.Entries))
	for _, m := range out.Entries {
		e, err := entryFrom(m)
		if err != nil {
			return nil, "", err
		}
		if !strings.EqualFold(e.ParentID, dirID) {
			return nil, "", fmt.Errorf("dropbox: list_folder returned %q outside requested directory %q", e.ID, dirID)
		}
		// Preserve the exact canonical directory id supplied by the caller.
		e.ParentID = dirID
		entries = append(entries, e)
	}
	if out.HasMore {
		if out.Cursor == "" {
			return nil, "", errors.New("dropbox: paginated listing returned an empty cursor")
		}
		return entries, out.Cursor, nil
	}
	return entries, "", nil
}

func (p *Provider) ListStream(ctx context.Context, dirID string, visit func(provider.Entry) error) error {
	if visit == nil {
		return errors.New("dropbox: list visitor is nil")
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
			return errors.New("dropbox: repeated directory cursor")
		}
		seen[next] = struct{}{}
		cursor = next
	}
	return errors.New("dropbox: directory listing exceeds 10000 pages")
}

func (p *Provider) Stat(ctx context.Context, id string) (provider.Entry, error) {
	id, err := cleanID(id)
	if err != nil {
		return provider.Entry{}, err
	}
	if id == RootID {
		return provider.Entry{ID: RootID, Name: "", Kind: provider.KindDir}, nil
	}
	var out metadata
	err = p.apiJSON(ctx, "/2/files/get_metadata", map[string]any{
		"path": apiPath(id), "include_deleted": false,
	}, &out, ratelimit.Meta, true)
	if err != nil {
		return provider.Entry{}, err
	}
	return entryFrom(out)
}

func (p *Provider) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	id, err := cleanID(id)
	if err != nil || id == RootID {
		if err == nil {
			err = errors.New("dropbox: cannot download the root directory")
		}
		return nil, err
	}
	if off < 0 || n < 0 {
		return nil, errors.New("dropbox: negative range")
	}
	arg, err := headerJSON(map[string]string{"path": apiPath(id)})
	if err != nil {
		return nil, err
	}
	var resp *httpx.Response
	err = p.authCall(ctx, func(token string) error {
		h := p.authHeader(token)
		h.Set("Dropbox-API-Arg", arg)
		h.Set("Range", httpx.RangeHeader(off, n))
		var callErr error
		resp, callErr = p.client.Do(ctx, httpx.Request{
			Method: http.MethodPost, URL: p.contentBase + "/2/files/download",
			Class: ratelimit.Download, Header: h,
			ExpectStatus: []int{http.StatusOK, http.StatusPartialContent}, Stream: true,
		})
		return callErr
	})
	if err != nil {
		return nil, err
	}
	var m metadata
	if err := decodeHeaderJSON(resp.Header.Get("Dropbox-API-Result"), &m); err != nil {
		resp.Body.Close()
		return nil, fmt.Errorf("dropbox: invalid download metadata: %w", err)
	}
	e, err := entryFrom(m)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	if !strings.EqualFold(e.ID, id) || e.Kind != provider.KindFile {
		resp.Body.Close()
		return nil, errors.New("dropbox: download response metadata does not match the requested file")
	}
	if version != "" && e.Version != version {
		resp.Body.Close()
		return nil, fmt.Errorf("%w: dropbox revision changed from %q to %q", provider.ErrConflict, version, e.Version)
	}
	return httpx.RangeBody(resp, off, n)
}

func (p *Provider) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	id, err := cleanID(id)
	if err != nil || id == RootID {
		if err == nil {
			err = errors.New("dropbox: cannot link the root directory")
		}
		return provider.Link{}, err
	}
	var out struct {
		Metadata metadata `json:"metadata"`
		Link     string   `json:"link"`
	}
	if err := p.apiJSON(ctx, "/2/files/get_temporary_link", map[string]string{"path": apiPath(id)}, &out, ratelimit.Download, true); err != nil {
		return provider.Link{}, err
	}
	e, err := entryFrom(out.Metadata)
	if err != nil || e.Kind != provider.KindFile || !strings.EqualFold(e.ID, id) {
		if err == nil {
			err = errors.New("temporary-link metadata does not match the requested file")
		}
		return provider.Link{}, fmt.Errorf("dropbox: %w", err)
	}
	if err := safeAbsoluteURL(out.Link); err != nil {
		return provider.Link{}, fmt.Errorf("dropbox: invalid temporary link: %w", err)
	}
	return provider.Link{URL: out.Link, ExpiresAt: p.now().Add(4 * time.Hour)}, nil
}

func (p *Provider) PutFile(ctx context.Context, parentID, name string, r io.Reader, size int64, _ provider.Hashes) (provider.Entry, error) {
	target, err := childID(parentID, name)
	if err != nil {
		return provider.Entry{}, err
	}
	if size < 0 || size > defaultSinglePutMax {
		return provider.Entry{}, fmt.Errorf("dropbox: single upload size %d is outside 0..%d", size, defaultSinglePutMax)
	}
	body, err := readExact(r, size)
	if err != nil {
		return provider.Entry{}, err
	}
	var out metadata
	arg := commitArg(target)
	if err := p.contentJSON(ctx, "/2/files/upload", arg, body, &out); err != nil {
		return provider.Entry{}, err
	}
	return entryFrom(out)
}

func (p *Provider) BeginUpload(ctx context.Context, parentID, name string, size int64, _ provider.Hashes) (provider.UploadSession, error) {
	target, err := childID(parentID, name)
	if err != nil {
		return provider.UploadSession{}, err
	}
	if size < 0 || size > maxDropboxFile {
		return provider.UploadSession{}, fmt.Errorf("dropbox: upload size %d is outside the supported range", size)
	}
	var out struct {
		SessionID string `json:"session_id"`
	}
	if err := p.contentJSON(ctx, "/2/files/upload_session/start", map[string]bool{"close": false}, nil, &out); err != nil {
		return provider.UploadSession{}, err
	}
	if out.SessionID == "" || len(out.SessionID) > 16<<10 || strings.ContainsAny(out.SessionID, "\r\n\x00") {
		return provider.UploadSession{}, errors.New("dropbox: upload session start returned an invalid session_id")
	}
	return provider.UploadSession{
		ID: out.SessionID, PartSize: p.partSize,
		Opaque: map[string]string{
			"session_id": out.SessionID, "target": target,
			"size": strconv.FormatInt(size, 10), "part_size": strconv.FormatInt(p.partSize, 10),
		},
	}, nil
}

func sessionState(s provider.UploadSession) (sessionID, target string, size, partSize int64, err error) {
	sessionID = s.Opaque["session_id"]
	if sessionID == "" {
		sessionID = s.ID
	}
	target = s.Opaque["target"]
	if sessionID == "" || len(sessionID) > 16<<10 || strings.ContainsAny(sessionID, "\r\n\x00") {
		err = errors.New("dropbox: upload session has an invalid session_id")
		return
	}
	if target, err = cleanID(target); err != nil || target == RootID {
		if err == nil {
			err = errors.New("dropbox: upload session has no target")
		}
		return
	}
	size, err = strconv.ParseInt(s.Opaque["size"], 10, 64)
	if err != nil || size < 0 || size > maxDropboxFile {
		err = errors.New("dropbox: upload session has an invalid size")
		return
	}
	partSize = s.PartSize
	if partSize == 0 {
		partSize, err = strconv.ParseInt(s.Opaque["part_size"], 10, 64)
	}
	if err != nil || partSize < 1 || partSize > maxRequestBody {
		err = errors.New("dropbox: upload session has an invalid part size")
	}
	return
}

func (p *Provider) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	sessionID, _, size, partSize, err := sessionState(s)
	if err != nil {
		return provider.PartToken{}, err
	}
	if idx < 0 || int64(idx) > maxDropboxFile/partSize {
		return provider.PartToken{}, errors.New("dropbox: invalid upload part index")
	}
	offset := int64(idx) * partSize
	want := partSize
	if remain := size - offset; remain < want {
		want = remain
	}
	if offset < 0 || offset >= size || want < 0 || n != want {
		return provider.PartToken{}, fmt.Errorf("dropbox: part %d has size %d, want %d", idx, n, want)
	}
	body, err := readExact(r, n)
	if err != nil {
		return provider.PartToken{}, fmt.Errorf("dropbox: read part %d: %w", idx, err)
	}
	arg := map[string]any{"cursor": map[string]any{"session_id": sessionID, "offset": offset}, "close": false}
	err = p.contentJSON(ctx, "/2/files/upload_session/append_v2", arg, body, nil)
	if err != nil {
		// A response can be lost after Dropbox accepted the bytes. Its offset
		// error is then an idempotency acknowledgement for this exact part.
		if correct, ok := correctOffset(err); ok && correct == offset+n {
			return provider.PartToken{Index: idx, ETag: strconv.FormatInt(n, 10)}, nil
		}
		return provider.PartToken{}, err
	}
	return provider.PartToken{Index: idx, ETag: strconv.FormatInt(n, 10)}, nil
}

func (p *Provider) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	sessionID, target, size, partSize, err := sessionState(s)
	if err != nil {
		return provider.Entry{}, err
	}
	ordered := append([]provider.PartToken(nil), parts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })
	expected := int((size + partSize - 1) / partSize)
	if len(ordered) != expected {
		return provider.Entry{}, fmt.Errorf("dropbox: upload has %d completed parts, want %d", len(ordered), expected)
	}
	for i, part := range ordered {
		if part.Index != i {
			return provider.Entry{}, errors.New("dropbox: upload parts must be unique and contiguous from zero")
		}
		want := partSize
		if remain := size - int64(i)*partSize; remain < want {
			want = remain
		}
		got, parseErr := strconv.ParseInt(part.ETag, 10, 64)
		if parseErr != nil || got != want {
			return provider.Entry{}, fmt.Errorf("dropbox: part %d token does not match its expected size", i)
		}
	}
	arg := map[string]any{
		"cursor": map[string]any{"session_id": sessionID, "offset": size},
		"commit": commitArg(target),
	}
	var out metadata
	if err = p.contentJSON(ctx, "/2/files/upload_session/finish", arg, nil, &out); err != nil {
		// A same-sized pre-existing target cannot prove that an ambiguous commit
		// succeeded, so never turn this failure into success using Stat alone.
		return provider.Entry{}, err
	}
	return entryFrom(out)
}

func commitArg(target string) map[string]any {
	return map[string]any{
		"path": apiPath(target), "mode": "overwrite", "autorename": false,
		"mute": true, "strict_conflict": false,
	}
}

func (p *Provider) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	target, err := childID(parentID, name)
	if err != nil {
		return provider.Entry{}, err
	}
	var out struct {
		Metadata metadata `json:"metadata"`
	}
	if err := p.apiJSON(ctx, "/2/files/create_folder_v2", map[string]any{"path": apiPath(target), "autorename": false}, &out, ratelimit.Meta, false); err != nil {
		return provider.Entry{}, err
	}
	return entryFrom(out.Metadata)
}

func (p *Provider) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	id, err := cleanID(id)
	if err != nil || id == RootID {
		if err == nil {
			err = errors.New("dropbox: cannot rename the root directory")
		}
		return provider.Entry{}, err
	}
	target, err := childID(path.Dir(id), newName)
	if err != nil {
		return provider.Entry{}, err
	}
	return p.relocate(ctx, "/2/files/move_v2", id, target)
}

func (p *Provider) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	id, err := cleanID(id)
	if err != nil || id == RootID {
		if err == nil {
			err = errors.New("dropbox: cannot move the root directory")
		}
		return provider.Entry{}, err
	}
	target, err := childID(newParentID, path.Base(id))
	if err != nil {
		return provider.Entry{}, err
	}
	if strings.HasPrefix(strings.ToLower(target)+"/", strings.ToLower(id)+"/") {
		return provider.Entry{}, errors.New("dropbox: cannot move a directory into itself")
	}
	return p.relocate(ctx, "/2/files/move_v2", id, target)
}

func (p *Provider) Copy(ctx context.Context, id, newParentID, newName string) (provider.Entry, error) {
	id, err := cleanID(id)
	if err != nil || id == RootID {
		if err == nil {
			err = errors.New("dropbox: cannot copy the root directory")
		}
		return provider.Entry{}, err
	}
	if newName == "" {
		newName = path.Base(id)
	}
	target, err := childID(newParentID, newName)
	if err != nil {
		return provider.Entry{}, err
	}
	return p.relocate(ctx, "/2/files/copy_v2", id, target)
}

func (p *Provider) relocate(ctx context.Context, endpoint, from, to string) (provider.Entry, error) {
	var out struct {
		Metadata metadata `json:"metadata"`
	}
	if err := p.apiJSON(ctx, endpoint, map[string]any{
		"from_path": apiPath(from), "to_path": apiPath(to), "autorename": false,
	}, &out, ratelimit.Meta, false); err != nil {
		return provider.Entry{}, err
	}
	return entryFrom(out.Metadata)
}

func (p *Provider) Delete(ctx context.Context, id string) error {
	id, err := cleanID(id)
	if err != nil {
		return err
	}
	if id == RootID {
		return errors.New("dropbox: refusing to delete the provider root")
	}
	return p.apiJSON(ctx, "/2/files/delete_v2", map[string]string{"path": apiPath(id)}, nil, ratelimit.Meta, false)
}

// pathSpaceUsage reports the account's space. It takes no argument, so the
// call goes out with no body at all — Dropbox rejects a JSON content type on
// its argument-less endpoints.
const pathSpaceUsage = "/2/users/get_space_usage"

// Quota implements provider.Quotaer. A pool ranks its members by free space
// and puts every member that can report a figure ahead of every member that
// cannot, so a driver that stays silent here loses placement decisions rather
// than merely going unsorted — which is why this exists even though Dropbox
// accounts are rarely the tight ones in a pool.
//
// Only the individual allocation is readable: its "allocated" is the whole
// account's total, and the top-level "used" is what is consumed. Any other
// allocation shape reports unknown (Total 0), which Quota.Free turns into a
// negative number, rather than a guess — placing writes against a number that
// is not this account's is worse than placing them unsorted.
//
// UNVERIFIED: the team allocation's real field names and semantics. Dropbox
// documents team_space_allocation with per-user and team-wide figures whose
// relationship to what this mount may actually write is unclear, so it is
// deliberately reported as unknown. Verify against a real Dropbox Business
// account which of those figures bounds a single member's writes before
// teaching this method to read it.
func (p *Provider) Quota(ctx context.Context) (provider.Quota, error) {
	var usage struct {
		Used       int64 `json:"used"`
		Allocation struct {
			Tag       string `json:".tag"`
			Allocated int64  `json:"allocated"`
		} `json:"allocation"`
	}
	if err := p.apiJSON(ctx, pathSpaceUsage, nil, &usage, ratelimit.Meta, true); err != nil {
		return provider.Quota{}, err
	}
	if usage.Allocation.Tag != "individual" {
		return provider.Quota{}, nil
	}
	return provider.Quota{Total: usage.Allocation.Allocated, Used: usage.Used}, nil
}

func readExact(r io.Reader, n int64) ([]byte, error) {
	if n < 0 || n > maxRequestBody {
		return nil, errors.New("dropbox: request body size is outside the supported range")
	}
	if r == nil {
		if n == 0 {
			return []byte{}, nil
		}
		return nil, errors.New("dropbox: request body is nil")
	}
	b, err := io.ReadAll(io.LimitReader(r, n+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != n {
		return nil, fmt.Errorf("dropbox: request body has %d bytes, want %d", len(b), n)
	}
	return b, nil
}

func (p *Provider) authHeader(token string) http.Header {
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+token)
	return h
}

func (p *Provider) token(ctx context.Context) (string, error) {
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
	p.mu.Unlock()
	if current != "" && current != stale && (expiry.IsZero() || p.now().Add(time.Minute).Before(expiry)) {
		return current, nil
	}
	if p.refreshToken == "" {
		return "", fmt.Errorf("%w: dropbox access token expired and no refresh_token is configured", provider.ErrAuth)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	form := map[string]string{
		"grant_type": "refresh_token", "refresh_token": p.refreshToken,
		"client_id": p.clientID,
	}
	if p.clientSecret != "" {
		form["client_secret"] = p.clientSecret
	}
	err := p.doJSON(ctx, httpx.Request{
		Method: http.MethodPost, URL: p.oauthURL, Class: ratelimit.Meta,
		Form: form,
	}, &out)
	if err != nil || out.AccessToken == "" {
		if err == nil {
			err = errors.New("empty access_token")
		}
		return "", fmt.Errorf("%w: dropbox token refresh failed: %v", provider.ErrAuth, err)
	}
	expires := time.Time{}
	if out.ExpiresIn > 0 {
		expires = p.now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	p.mu.Lock()
	p.accessToken, p.expiresAt = out.AccessToken, expires
	p.mu.Unlock()
	return out.AccessToken, nil
}

func (p *Provider) authCall(ctx context.Context, call func(string) error) error {
	token, err := p.token(ctx)
	if err != nil {
		return err
	}
	err = mapError(call(token))
	if !errors.Is(err, provider.ErrAuth) || p.refreshToken == "" {
		return err
	}
	token, refreshErr := p.refresh(ctx, token)
	if refreshErr != nil {
		return refreshErr
	}
	return mapError(call(token))
}

func (p *Provider) apiJSON(ctx context.Context, endpoint string, arg, out any, class ratelimit.Class, retryable bool) error {
	return p.authCall(ctx, func(token string) error {
		r := httpx.Request{Method: http.MethodPost, URL: p.apiBase + endpoint, Class: class, Header: p.authHeader(token)}
		if retryable {
			r.JSON = arg
		} else {
			body, err := json.Marshal(arg)
			if err != nil {
				return err
			}
			r.Body = bytes.NewReader(body)
			r.Header.Set("Content-Type", "application/json")
		}
		return p.doJSON(ctx, r, out)
	})
}

func (p *Provider) contentJSON(ctx context.Context, endpoint string, arg any, body []byte, out any) error {
	headerArg, err := headerJSON(arg)
	if err != nil {
		return err
	}
	return p.authCall(ctx, func(token string) error {
		h := p.authHeader(token)
		h.Set("Dropbox-API-Arg", headerArg)
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		return p.doJSON(ctx, httpx.Request{
			Method: http.MethodPost, URL: p.contentBase + endpoint, Class: ratelimit.Upload,
			Header: h, Body: bytes.NewReader(body),
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
			return errors.New("dropbox: API response exceeds 16 MiB")
		}
		return nil
	}
	limited := io.LimitReader(resp.Body, maxJSONResponse+1)
	dec := json.NewDecoder(limited)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("dropbox: decode API response: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("dropbox: API response contains trailing JSON")
		}
		return fmt.Errorf("dropbox: API response is too large or malformed: %w", err)
	}
	return nil
}

func headerJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, r := range string(b) {
		switch {
		case r <= 0x7f:
			out.WriteRune(r)
		case r <= 0xffff:
			fmt.Fprintf(&out, "\\u%04x", r)
		default:
			r -= 0x10000
			fmt.Fprintf(&out, "\\u%04x\\u%04x", 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		}
	}
	return out.String(), nil
}

func decodeHeaderJSON(raw string, out any) error {
	if raw == "" || len(raw) > 64<<10 || strings.ContainsAny(raw, "\r\n\x00") {
		return errors.New("missing or unsafe Dropbox-API-Result header")
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return err
	}
	return nil
}

type apiError struct {
	status  int
	summary string
	body    string
	kind    error
}

func (e *apiError) Error() string {
	summary := e.summary
	if len(summary) > 300 {
		summary = summary[:300] + "..."
	}
	if summary == "" {
		return fmt.Sprintf("dropbox: API returned HTTP %d", e.status)
	}
	return fmt.Sprintf("dropbox: API returned HTTP %d: %s", e.status, summary)
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
		Summary string          `json:"error_summary"`
		Error   json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal([]byte(se.Body), &envelope)
	summary := strings.ToLower(envelope.Summary)
	kind := error(nil)
	switch {
	case se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden:
		kind = provider.ErrAuth
	case se.Code == http.StatusNotFound:
		kind = provider.ErrNotFound
	case se.Code == http.StatusTooManyRequests:
		kind = provider.ErrRateLimited
	case se.Code >= 500:
		kind = provider.ErrTransient
	case se.Code == http.StatusConflict &&
		(strings.Contains(summary, "not_found") || strings.Contains(summary, "not found")):
		kind = provider.ErrNotFound
	case se.Code == http.StatusConflict &&
		(strings.Contains(summary, "conflict") || strings.Contains(summary, "already_exists")):
		kind = provider.ErrExists
	case se.Code == http.StatusConflict:
		kind = provider.ErrConflict
	case se.Code == http.StatusGone:
		kind = provider.ErrLinkExpired
	}
	if kind == nil {
		return err
	}
	return &apiError{status: se.Code, summary: envelope.Summary, body: se.Body, kind: kind}
}

func correctOffset(err error) (int64, bool) {
	var ae *apiError
	if !errors.As(err, &ae) || !strings.Contains(strings.ToLower(ae.summary), "incorrect_offset") {
		return 0, false
	}
	var envelope struct {
		Error struct {
			IncorrectOffset struct {
				CorrectOffset int64 `json:"correct_offset"`
			} `json:"incorrect_offset"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(ae.body), &envelope) != nil || envelope.Error.IncorrectOffset.CorrectOffset < 0 {
		return 0, false
	}
	return envelope.Error.IncorrectOffset.CorrectOffset, true
}

func isCursorReset(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && strings.HasPrefix(strings.ToLower(ae.summary), "reset")
}

var (
	_ provider.Provider     = (*Provider)(nil)
	_ provider.StreamLister = (*Provider)(nil)
	_ provider.SinglePutter = (*Provider)(nil)
	_ provider.ServerCopier = (*Provider)(nil)
	_ provider.ChangeLister = (*Provider)(nil)
)
