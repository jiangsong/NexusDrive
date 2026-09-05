// Package pan123 implements the Provider interface over the official 123 Pan
// (123云盘) Open API at open-api.123pan.com.
//
// Auth is a clientID/clientSecret exchange for a 30-day access token; uploads
// are MD5 rapid-upload first, then a server-directed slice upload whose slice
// size the server chooses. 123 publishes no delta feed, so Caps.Delta is false
// and this package does not implement provider.ChangeLister
// (docs/DESIGN.md §4.1).
//
// The API is inconsistent about identifier casing — /api/v2/file/list speaks
// fileId while /api/v1/file/detail speaks fileID, and the upload endpoints use
// fileID/parentFileID — so every request struct below spells the field exactly
// the way its own endpoint documents it. Do not "tidy" them into one style.
package pan123

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// DefaultBaseURL is the Open API host.
const DefaultBaseURL = "https://open-api.123pan.com"

// RootID is the id of the drive root.
const RootID = "0"

// PartSize is the slice size the driver assumes before the server states one.
// /upload/v2/file/create returns the authoritative sliceSize and BeginUpload
// puts that in the session, so this value only shapes the capability matrix.
const PartSize = 16 << 20

// Platform is the fixed value of the mandatory Platform header.
const Platform = "open_platform"

// API paths. The upload endpoints live under /upload, not /api — including
// mkdir, which is /upload/v1/file/mkdir and not the /api/v1 path one would
// expect.
const (
	pathToken          = "/api/v1/access_token"
	pathList           = "/api/v2/file/list"
	pathDetail         = "/api/v1/file/detail"
	pathDownloadInfo   = "/api/v1/file/download_info"
	pathMkdir          = "/upload/v1/file/mkdir"
	pathName           = "/api/v1/file/name"
	pathMove           = "/api/v1/file/move"
	pathTrash          = "/api/v1/file/trash"
	pathUploadCreate   = "/upload/v2/file/create"
	pathUploadSlice    = "/upload/v2/file/slice"
	pathUploadComplete = "/upload/v2/file/upload_complete"
)

// Options configures a Provider.
type Options struct {
	// Name is the remote name used in metadata and cache keys.
	Name string
	// Client is the shared HTTP client. Required.
	Client *httpx.Client
	// BaseURL overrides DefaultBaseURL (tests point it at httptest).
	BaseURL string
	// ClientID and ClientSecret are the Open API application credentials.
	// Required unless AccessToken is supplied.
	ClientID, ClientSecret string
	// AccessToken seeds the first request. It is refreshed automatically when
	// ClientID/ClientSecret are known.
	AccessToken string
	// RootID overrides the root directory id (default "0").
	RootID string
	// LinkTTL is how long a download URL is assumed valid.
	LinkTTL time.Duration
	// PageSize is the list page size (default and maximum 100).
	PageSize int
	// PollInterval is the wait between upload_complete polls (default 1 s, the
	// interval the API documents).
	PollInterval time.Duration
	// PollAttempts caps how long CompleteUpload waits for the server to
	// assemble the slices (default 60, matching the documented 1 s cadence).
	PollAttempts int
	// Now is injectable for tests.
	Now func() time.Time
}

// Provider is the 123 Pan backend.
type Provider struct {
	name         string
	base         string
	client       *httpx.Client
	caps         provider.Caps
	rootID       string
	pageSize     int
	pollInterval time.Duration
	pollAttempts int
	now          func() time.Time

	clientID     string
	clientSecret string

	mu          sync.Mutex
	accessToken string
	expiry      time.Time

	// links caches download URLs until shortly before they lapse: the block
	// cache asks for many ranges of one file in a row and download_info is
	// rate limited like every other endpoint.
	linkMu sync.Mutex
	links  map[string]provider.Link
}

// New builds a Provider from explicit options.
func New(opt Options) (*Provider, error) {
	if opt.Client == nil {
		return nil, errors.New("pan123: Client is required")
	}
	if opt.AccessToken == "" {
		if opt.ClientID == "" {
			return nil, errors.New("pan123: remote is missing the required 'client_id' key")
		}
		if opt.ClientSecret == "" {
			return nil, errors.New("pan123: remote is missing the required 'client_secret' key")
		}
	}
	ttl := opt.LinkTTL
	if ttl <= 0 {
		// UNVERIFIED: 123 documents no lifetime for download_info URLs. Half
		// an hour is short enough that a stale link is refreshed long before a
		// CDN 403, and DownloadURL is cheap.
		ttl = 30 * time.Minute
	}
	page := opt.PageSize
	if page <= 0 || page > 100 {
		page = 100
	}
	poll := opt.PollInterval
	if poll <= 0 {
		poll = time.Second
	}
	attempts := opt.PollAttempts
	if attempts <= 0 {
		attempts = 60
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	root := opt.RootID
	if root == "" {
		root = RootID
	}
	return &Provider{
		name:         opt.Name,
		base:         strings.TrimSuffix(orDefault(opt.BaseURL, DefaultBaseURL), "/"),
		client:       opt.Client,
		rootID:       root,
		pageSize:     page,
		pollInterval: poll,
		pollAttempts: attempts,
		now:          now,
		clientID:     opt.ClientID,
		clientSecret: opt.ClientSecret,
		accessToken:  opt.AccessToken,
		links:        map[string]provider.Link{},
		caps: provider.Caps{
			HashTypes:   []provider.HashType{provider.HashMD5},
			RapidUpload: []provider.HashType{provider.HashMD5},
			RangeRead:   true,
			PartSize:    PartSize,
			// UNVERIFIED: 123 documents a 10 GiB single-file limit but no
			// slice-count ceiling. 640 is 10 GiB at the 16 MiB slice size the
			// server hands out today; the session's PartSize comes from the
			// server anyway, so this only bounds what the VFS will attempt.
			MaxParts:       640,
			UploadParallel: 3,
			ServerMove:     true,
			ServerRename:   true,
			// The Open API has no copy endpoint; a copy degrades to
			// download + upload one layer up.
			ServerCopy:    false,
			Delta:         false,
			LinkTTL:       ttl,
			LinkShareable: true,
			// The published per-endpoint QPS is 15 for list and 20 for
			// mkdir/move, but only 2 for upload create; start below all of
			// them and let AIMD find the ceiling.
			QPS:             provider.QPS{Meta: 8, Download: 5, Upload: 2},
			MaxConnsPerHost: 8,
			Tier:            provider.TierOfficial,
		},
	}, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Name returns the remote name.
func (p *Provider) Name() string { return p.name }

// Capabilities returns the capability matrix.
func (p *Provider) Capabilities() provider.Caps { return p.caps }

// RootFileID returns the id the driver treats as the root.
func (p *Provider) RootFileID() string { return p.rootID }

// ---------------------------------------------------------------- errors ---

// Documented envelope codes. 123 answers HTTP 200 for these; the number lives
// in the body's "code" field.
const (
	// CodeOK marks success.
	CodeOK = 0
	// CodeInternal is the generic server-side failure.
	CodeInternal = 1
	// CodeUnauthorized means the access token is missing, invalid or expired.
	CodeUnauthorized = 401
	// CodeTooManyRequests means the per-endpoint QPS was exceeded.
	CodeTooManyRequests = 429
	// CodeNotFound is returned by download_info for a missing file.
	CodeNotFound = 5066
	// CodeQuotaExhausted means the daily self-download traffic is used up.
	CodeQuotaExhausted = 5113
)

// APIError is the response envelope of a failed call.
type APIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	TraceID string `json:"x-traceID"`
	// Op names the endpoint, for the error string only.
	Op string `json:"-"`
}

// Error implements error.
func (e *APIError) Error() string {
	s := fmt.Sprintf("pan123: %s failed: code %d (%s)", e.Op, e.Code, e.Message)
	if e.TraceID != "" {
		// 123 support asks for the trace id, so never drop it.
		s += " [trace " + e.TraceID + "]"
	}
	return s
}

// Unwrap maps the envelope code onto a provider sentinel.
//
// UNVERIFIED: 123 publishes only the handful of codes named above, and a name
// collision has no code at all — it arrives as a message such as "不能重名".
// The message sniffing below is therefore a best effort; a miss degrades to a
// plain error, never to a wrong write.
func (e *APIError) Unwrap() error {
	switch e.Code {
	case CodeUnauthorized:
		return provider.ErrAuth
	case CodeTooManyRequests:
		return provider.ErrRateLimited
	case CodeNotFound:
		return provider.ErrNotFound
	case CodeQuotaExhausted:
		// The daily self-download allowance is used up. It resets, so this is
		// a throttle rather than a permanent failure, and ErrRateLimited puts
		// the AIMD limiter in charge of backing off.
		return provider.ErrRateLimited
	}
	// The message is inspected before the generic code 1 is called transient:
	// a name collision arrives as code 1 with only the text to go on, and
	// retrying it would loop forever.
	switch {
	case strings.Contains(e.Message, "重名"), strings.Contains(e.Message, "已存在"):
		return provider.ErrExists
	case strings.Contains(e.Message, "不存在"):
		return provider.ErrNotFound
	case strings.Contains(e.Message, "频繁"):
		return provider.ErrRateLimited
	// Risk control is distinct from a rate limit: the circuit breaker stops
	// touching the account entirely, which is what keeps a flagged account
	// from being pushed into a ban. Without this branch nothing this driver
	// returns could ever trip it.
	case strings.Contains(e.Message, "风控"),
		strings.Contains(e.Message, "异常"),
		strings.Contains(e.Message, "封禁"),
		strings.Contains(e.Message, "验证"):
		return provider.ErrRiskControl
	}
	if e.Code == CodeInternal {
		return provider.ErrTransient
	}
	return nil
}

// envelope is the shape of every Open API response.
type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	TraceID string          `json:"x-traceID"`
	Data    json.RawMessage `json:"data"`
}

// ----------------------------------------------------------------- token ---

func (p *Provider) token(ctx context.Context) (string, error) {
	p.mu.Lock()
	tok, exp := p.accessToken, p.expiry
	p.mu.Unlock()
	if tok != "" && (exp.IsZero() || p.now().Add(time.Minute).Before(exp)) {
		return tok, nil
	}
	return p.refresh(ctx)
}

// refresh exchanges the client credentials for an access token.
//
// 123 keeps at most three live tokens per client_id and evicts the oldest, so
// the driver refreshes only when the current token is about to expire rather
// than on every start.
func (p *Provider) refresh(ctx context.Context) (string, error) {
	p.mu.Lock()
	id, secret := p.clientID, p.clientSecret
	p.mu.Unlock()
	if id == "" || secret == "" {
		return "", fmt.Errorf("%w: pan123: access_token expired and no client_id/client_secret configured", provider.ErrAuth)
	}
	var env envelope
	err := p.client.JSON(ctx, httpx.Request{
		Method: http.MethodPost,
		URL:    p.base + pathToken,
		Class:  ratelimit.Meta,
		// The token endpoint takes the Platform header but no Authorization.
		Header: http.Header{"Platform": []string{Platform}},
		JSON:   map[string]string{"clientID": id, "clientSecret": secret},
	}, &env)
	if err != nil {
		return "", fmt.Errorf("%w: pan123: access_token request failed: %w", provider.ErrAuth, err)
	}
	if env.Code != CodeOK {
		return "", fmt.Errorf("%w: %w", provider.ErrAuth,
			&APIError{Code: env.Code, Message: env.Message, TraceID: env.TraceID, Op: "access_token"})
	}
	var data struct {
		AccessToken string `json:"accessToken"`
		ExpiredAt   string `json:"expiredAt"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return "", fmt.Errorf("pan123: decode access_token: %w", err)
	}
	if data.AccessToken == "" {
		return "", fmt.Errorf("%w: pan123: access_token response carried no token", provider.ErrAuth)
	}
	p.mu.Lock()
	p.accessToken = data.AccessToken
	// expiredAt is RFC3339 with an offset ("2025-03-23T15:48:37+08:00"), unlike
	// the space-separated local times the file endpoints return.
	if t, err := time.Parse(time.RFC3339, data.ExpiredAt); err == nil {
		p.expiry = t
	}
	p.mu.Unlock()
	return data.AccessToken, nil
}

// AccessToken returns the token currently in use.
func (p *Provider) AccessToken() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accessToken
}

// call performs one authenticated request and unmarshals the envelope's data
// into out. Code 401 triggers a single refresh-and-retry.
func (p *Provider) call(ctx context.Context, op, method, apiPath string, class ratelimit.Class, body any, query url.Values, out any) error {
	err := p.callOnce(ctx, op, method, apiPath, class, body, query, out)
	if err != nil && errors.Is(err, provider.ErrAuth) {
		if _, rerr := p.refresh(ctx); rerr != nil {
			// Keep the original failure in the chain: it names the code the
			// server actually reported, which the refresh error does not.
			return fmt.Errorf("%w (token refresh failed: %v)", err, rerr)
		}
		return p.callOnce(ctx, op, method, apiPath, class, body, query, out)
	}
	return err
}

func (p *Provider) callOnce(ctx context.Context, op, method, apiPath string, class ratelimit.Class, body any, query url.Values, out any) error {
	tok, err := p.token(ctx)
	if err != nil {
		return err
	}
	u := p.base + apiPath
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var env envelope
	req := httpx.Request{
		Method: method,
		URL:    u,
		Class:  class,
		Header: p.headers(tok),
		JSON:   body,
	}
	if err := p.client.JSON(ctx, req, &env); err != nil {
		return err
	}
	return decodeEnvelope(op, env, out)
}

func (p *Provider) headers(tok string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	// Platform is mandatory on every call; without it the gateway rejects the
	// request before it reaches the API.
	h.Set("Platform", Platform)
	return h
}

func decodeEnvelope(op string, env envelope, out any) error {
	if env.Code != CodeOK {
		return &APIError{Code: env.Code, Message: env.Message, TraceID: env.TraceID, Op: op}
	}
	if out == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("pan123: decode %s data: %w", op, err)
	}
	return nil
}

// ------------------------------------------------------------------ list ---

// listItem is one entry of /api/v2/file/list. Note the lowercase "Id" spelling
// this endpoint uses.
type listItem struct {
	FileID       json.Number `json:"fileId"`
	Filename     string      `json:"filename"`
	ParentFileID json.Number `json:"parentFileId"`
	Type         int         `json:"type"` // 0 file, 1 folder
	Size         int64       `json:"size"`
	Etag         string      `json:"etag"`
	Status       int         `json:"status"`
	Trashed      int         `json:"trashed"`
	CreateAt     string      `json:"createAt"`
	UpdateAt     string      `json:"updateAt"`
}

// beijing is the zone 123 reports its timestamps in. They arrive as
// "2024-04-30 11:58:36" with no offset, and the service runs on UTC+8.
var beijing = time.FixedZone("UTC+8", 8*60*60)

const listTimeLayout = "2006-01-02 15:04:05"

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.ParseInLocation(listTimeLayout, s, beijing); err == nil {
		return t
	}
	// The legacy v1 endpoints return "2025-02-24 17:57:01 +0800 CST".
	if t, err := time.Parse("2006-01-02 15:04:05 -0700 MST", s); err == nil {
		return t
	}
	return time.Time{}
}

func (it listItem) entry() provider.Entry {
	e := provider.Entry{
		ID:       it.FileID.String(),
		ParentID: it.ParentFileID.String(),
		Name:     it.Filename,
		Kind:     provider.KindFile,
		Size:     it.Size,
		ModTime:  parseTime(pick(it.UpdateAt, it.CreateAt)),
	}
	if it.Type == 1 {
		e.Kind = provider.KindDir
		e.Size = 0
	}
	etag := strings.ToLower(it.Etag)
	if e.Kind == provider.KindFile && etag != "" {
		e.Hashes = provider.Hashes{provider.HashMD5: etag}
		e.Version = etag
	} else {
		e.Version = fmt.Sprintf("%d-%d", it.Size, e.ModTime.Unix())
	}
	return e
}

func pick(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// List returns one page of a directory. The cursor is the API's lastFileId;
// the server reports -1 once the last page has been handed out.
//
// Recycle-bin entries come back from this endpoint whatever the request says,
// so trashed items are dropped here — the documented "trashed" filter does not
// work on v2.
func (p *Provider) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	if dirID == "" {
		dirID = p.rootID
	}
	q := url.Values{}
	q.Set("parentFileId", dirID)
	q.Set("limit", strconv.Itoa(p.pageSize))
	if cursor != "" {
		q.Set("lastFileId", cursor)
	}
	var data struct {
		LastFileID json.Number `json:"lastFileId"`
		FileList   []listItem  `json:"fileList"`
	}
	if err := p.call(ctx, "file/list", http.MethodGet, pathList, ratelimit.Meta, nil, q, &data); err != nil {
		return nil, "", err
	}
	entries := make([]provider.Entry, 0, len(data.FileList))
	for _, it := range data.FileList {
		if it.Trashed != 0 {
			continue
		}
		e := it.entry()
		if e.ParentID == "" || e.ParentID == "0" && dirID != p.rootID {
			e.ParentID = dirID
		}
		entries = append(entries, e)
	}
	next := data.LastFileID.String()
	if next == "" || next == "-1" || next == "0" {
		next = ""
	}
	return entries, next, nil
}

// Stat returns one entry. /api/v1/file/detail is the capital-ID endpoint: both
// its query parameter and its response spell the field fileID.
func (p *Provider) Stat(ctx context.Context, id string) (provider.Entry, error) {
	if id == "" {
		id = p.rootID
	}
	if id == p.rootID {
		return provider.Entry{ID: p.rootID, Kind: provider.KindDir, Version: p.rootID}, nil
	}
	q := url.Values{}
	q.Set("fileID", id)
	var data struct {
		FileID       json.Number `json:"fileID"`
		Filename     string      `json:"filename"`
		Type         int         `json:"type"`
		Size         int64       `json:"size"`
		Etag         string      `json:"etag"`
		Status       int         `json:"status"`
		ParentFileID json.Number `json:"parentFileID"`
		CreateAt     string      `json:"createAt"`
		Trashed      int         `json:"trashed"`
	}
	if err := p.call(ctx, "file/detail", http.MethodGet, pathDetail, ratelimit.Meta, nil, q, &data); err != nil {
		return provider.Entry{}, err
	}
	if data.FileID.String() == "" || data.FileID.String() == "0" {
		return provider.Entry{}, fmt.Errorf("%w: pan123: no file %s", provider.ErrNotFound, id)
	}
	it := listItem{
		FileID: data.FileID, Filename: data.Filename, ParentFileID: data.ParentFileID,
		Type: data.Type, Size: data.Size, Etag: data.Etag, Status: data.Status,
		Trashed: data.Trashed, CreateAt: data.CreateAt,
	}
	return it.entry(), nil
}

// DownloadURL returns a direct link. download_info is one of the lowercase-Id
// endpoints.
func (p *Provider) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	p.linkMu.Lock()
	if l, ok := p.links[id]; ok && p.now().Add(time.Minute).Before(l.ExpiresAt) {
		p.linkMu.Unlock()
		return l, nil
	}
	p.linkMu.Unlock()

	q := url.Values{}
	q.Set("fileId", id)
	var data struct {
		DownloadURL string `json:"downloadUrl"`
	}
	if err := p.call(ctx, "download_info", http.MethodGet, pathDownloadInfo, ratelimit.Download, nil, q, &data); err != nil {
		return provider.Link{}, err
	}
	if data.DownloadURL == "" {
		return provider.Link{}, fmt.Errorf("%w: pan123: download_info returned no url for %s", provider.ErrNotFound, id)
	}
	link := provider.Link{URL: data.DownloadURL, ExpiresAt: p.now().Add(p.caps.LinkTTL)}
	p.linkMu.Lock()
	p.links[id] = link
	p.linkMu.Unlock()
	return link, nil
}

// ReadRange fetches a byte range from the CDN, refreshing the link once if the
// CDN reports it expired.
func (p *Provider) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	link, err := p.DownloadURL(ctx, id)
	if err != nil {
		return nil, err
	}
	body, err := p.getRange(ctx, link.URL, off, n)
	if err != nil && errors.Is(err, provider.ErrLinkExpired) {
		p.linkMu.Lock()
		delete(p.links, id)
		p.linkMu.Unlock()
		fresh, err2 := p.DownloadURL(ctx, id)
		if err2 != nil {
			return nil, err2
		}
		return p.getRange(ctx, fresh.URL, off, n)
	}
	return body, err
}

func (p *Provider) getRange(ctx context.Context, rawURL string, off, n int64) (io.ReadCloser, error) {
	h := http.Header{}
	h.Set("Range", httpx.RangeHeader(off, n))
	resp, err := p.client.Do(ctx, httpx.Request{
		Method:       http.MethodGet,
		URL:          rawURL,
		Class:        ratelimit.Download,
		Header:       h,
		ExpectStatus: []int{http.StatusPartialContent, http.StatusOK},
		Stream:       true,
	})
	if err != nil {
		return nil, err
	}
	if resp.Status == http.StatusOK && off > 0 {
		if _, err := io.CopyN(io.Discard, resp.Body, off); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("pan123: server ignored Range and the file is shorter than offset %d: %w", off, err)
		}
	}
	if n > 0 {
		return readCloser{Reader: io.LimitReader(resp.Body, n), Closer: resp.Body}, nil
	}
	return resp.Body, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// ---------------------------------------------------------------- upload ---

// BeginUpload creates the upload. When 123 already holds a file with the same
// MD5 and size it answers reuse=true with the finished file id (秒传) and no
// byte is transferred.
func (p *Provider) BeginUpload(ctx context.Context, parentID, name string, size int64, h provider.Hashes) (provider.UploadSession, error) {
	if parentID == "" {
		parentID = p.rootID
	}
	parent, err := strconv.ParseInt(parentID, 10, 64)
	if err != nil {
		return provider.UploadSession{}, fmt.Errorf("pan123: bad parent id %q: %w", parentID, err)
	}
	etag := strings.ToLower(h[provider.HashMD5])
	req := map[string]any{
		"parentFileID": parent,
		"filename":     name,
		"etag":         etag,
		"size":         size,
		// duplicate 2 overwrites a same-named file. The upload layer has
		// already compared versions and picked a conflict name when it had to,
		// so an overwrite here is intended rather than a lost update.
		"duplicate":  2,
		"containDir": false,
	}
	var data struct {
		FileID      json.Number `json:"fileID"`
		Reuse       bool        `json:"reuse"`
		PreuploadID string      `json:"preuploadID"`
		SliceSize   int64       `json:"sliceSize"`
		Servers     []string    `json:"servers"`
	}
	if err := p.call(ctx, "upload/create", http.MethodPost, pathUploadCreate, ratelimit.Upload, req, nil, &data); err != nil {
		return provider.UploadSession{}, err
	}

	sliceSize := data.SliceSize
	if sliceSize <= 0 {
		sliceSize = PartSize
	}
	server := ""
	if len(data.Servers) > 0 {
		server = strings.TrimSuffix(data.Servers[0], "/")
	}
	s := provider.UploadSession{
		ID:       data.PreuploadID,
		PartSize: sliceSize,
		Opaque: map[string]string{
			"preupload_id": data.PreuploadID,
			"server":       server,
			"size":         strconv.FormatInt(size, 10),
			"parent_id":    parentID,
			"name":         name,
			"etag":         etag,
		},
	}

	// reuse=true with fileID 0 has been observed in the field: the server
	// signalled a rapid hit it then could not honour. Only a real id means the
	// file is there, otherwise fall through to slice upload.
	if data.Reuse && data.FileID.String() != "" && data.FileID.String() != "0" {
		e := provider.Entry{
			ID: data.FileID.String(), ParentID: parentID, Name: name,
			Kind: provider.KindFile, Size: size, ModTime: p.now(),
		}
		if etag != "" {
			e.Hashes = provider.Hashes{provider.HashMD5: etag}
			e.Version = etag
		}
		s.RapidDone = true
		s.Entry = &e
		return s, nil
	}
	if data.PreuploadID == "" {
		return provider.UploadSession{}, fmt.Errorf("pan123: upload/create returned neither a reused file nor a preuploadID for %q", name)
	}
	return s, nil
}

// UploadPart sends one slice. sliceNo is 1-based and the binary form field is
// named "slice" — not "file", which is what the single-step upload endpoint
// uses.
//
// The slice's own MD5 travels with it, so it is hashed before the body is
// sent: a seekable reader (what the upload layer passes) is hashed and rewound,
// anything else is buffered.
func (p *Provider) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	sum, body, err := hashAndRewind(r, n)
	if err != nil {
		return provider.PartToken{}, err
	}
	preuploadID := pick(s.Opaque["preupload_id"], s.ID)

	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	fields := [][2]string{
		{"preuploadID", preuploadID},
		{"sliceNo", strconv.Itoa(idx + 1)},
		{"sliceMD5", sum},
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return provider.PartToken{}, err
		}
	}
	fw, err := mw.CreateFormFile("slice", fmt.Sprintf("%s.part%d", s.Opaque["name"], idx+1))
	if err != nil {
		return provider.PartToken{}, err
	}
	copied, err := io.Copy(fw, io.LimitReader(body, n))
	if err != nil {
		return provider.PartToken{}, err
	}
	// A short read means the staged blob is smaller than the journal believes.
	// Uploading it anyway would commit a truncated file that looks successful.
	if copied != n {
		return provider.PartToken{}, fmt.Errorf("pan123: slice %d is short: read %d of %d bytes", idx+1, copied, n)
	}
	if err := mw.Close(); err != nil {
		return provider.PartToken{}, err
	}

	// The slice goes to the upload server the create call named, which is a
	// different host from the API.
	base := s.Opaque["server"]
	if base == "" {
		base = p.base
	}
	payload := buf.Bytes()
	contentType := mw.FormDataContentType()
	do := func() error {
		tok, err := p.token(ctx)
		if err != nil {
			return err
		}
		hdr := p.headers(tok)
		hdr.Set("Content-Type", contentType)
		var env envelope
		if err := p.client.JSON(ctx, httpx.Request{
			Method: http.MethodPost,
			URL:    base + pathUploadSlice,
			Class:  ratelimit.Upload,
			Header: hdr,
			Body:   bytes.NewReader(payload),
			GetBody: func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(payload)), nil
			},
		}, &env); err != nil {
			return err
		}
		return decodeEnvelope("upload/slice", env, nil)
	}
	err = do()
	if err != nil && errors.Is(err, provider.ErrAuth) {
		if _, rerr := p.refresh(ctx); rerr != nil {
			return provider.PartToken{}, fmt.Errorf("%w (token refresh failed: %v)", err, rerr)
		}
		err = do()
	}
	if err != nil {
		return provider.PartToken{}, err
	}
	// 123 returns no per-slice token; the slice MD5 is the only thing that
	// identifies the part, and CompleteUpload does not replay it.
	return provider.PartToken{Index: idx, ETag: sum}, nil
}

// hashAndRewind computes the MD5 of the next n bytes and returns a reader
// positioned back at the start of them.
func hashAndRewind(r io.Reader, n int64) (string, io.Reader, error) {
	h := md5.New()
	if s, ok := r.(io.Seeker); ok {
		start, err := s.Seek(0, io.SeekCurrent)
		if err == nil {
			if _, err := io.Copy(h, io.LimitReader(r, n)); err != nil {
				return "", nil, err
			}
			if _, err := s.Seek(start, io.SeekStart); err != nil {
				return "", nil, err
			}
			return hex.EncodeToString(h.Sum(nil)), r, nil
		}
	}
	buf := &bytes.Buffer{}
	if _, err := io.Copy(io.MultiWriter(h, buf), io.LimitReader(r, n)); err != nil {
		return "", nil, err
	}
	return hex.EncodeToString(h.Sum(nil)), buf, nil
}

// CompleteUpload finalises the upload. The server assembles the slices
// asynchronously, so completed=false means "ask again in a second"; success is
// completed=true together with a non-zero fileID.
func (p *Provider) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	preuploadID := pick(s.Opaque["preupload_id"], s.ID)
	req := map[string]any{"preuploadID": preuploadID}
	for attempt := 0; attempt < p.pollAttempts; attempt++ {
		var data struct {
			Completed bool        `json:"completed"`
			FileID    json.Number `json:"fileID"`
		}
		if err := p.call(ctx, "upload_complete", http.MethodPost, pathUploadComplete, ratelimit.Upload, req, nil, &data); err != nil {
			return provider.Entry{}, err
		}
		if data.Completed && data.FileID.String() != "" && data.FileID.String() != "0" {
			size, _ := strconv.ParseInt(s.Opaque["size"], 10, 64)
			e := provider.Entry{
				ID:       data.FileID.String(),
				ParentID: s.Opaque["parent_id"],
				Name:     s.Opaque["name"],
				Kind:     provider.KindFile,
				Size:     size,
				ModTime:  p.now(),
			}
			if etag := s.Opaque["etag"]; etag != "" {
				e.Hashes = provider.Hashes{provider.HashMD5: etag}
				e.Version = etag
			} else {
				e.Version = fmt.Sprintf("%d-%d", size, e.ModTime.Unix())
			}
			return e, nil
		}
		select {
		case <-ctx.Done():
			return provider.Entry{}, ctx.Err()
		case <-time.After(p.pollInterval):
		}
	}
	return provider.Entry{}, fmt.Errorf("%w: pan123: upload %s was still assembling after %d polls",
		provider.ErrTransient, preuploadID, p.pollAttempts)
}

// ------------------------------------------------------------- namespace ---

// Mkdir creates a directory. The endpoint lives under /upload, and its request
// spells the parent "parentID" while the response returns "dirID".
func (p *Provider) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	if parentID == "" {
		parentID = p.rootID
	}
	parent, err := strconv.ParseInt(parentID, 10, 64)
	if err != nil {
		return provider.Entry{}, fmt.Errorf("pan123: bad parent id %q: %w", parentID, err)
	}
	var data struct {
		DirID json.Number `json:"dirID"`
	}
	if err := p.call(ctx, "file/mkdir", http.MethodPost, pathMkdir, ratelimit.Meta,
		map[string]any{"name": name, "parentID": parent}, nil, &data); err != nil {
		return provider.Entry{}, err
	}
	if data.DirID.String() == "" {
		return provider.Entry{}, fmt.Errorf("pan123: mkdir returned no dirID for %q", name)
	}
	return provider.Entry{
		ID: data.DirID.String(), ParentID: parentID, Name: name,
		Kind: provider.KindDir, ModTime: p.now(), Version: data.DirID.String(),
	}, nil
}

// Rename renames one entry. This endpoint is a PUT — the batch variant
// (POST /api/v1/file/rename) exists but its documented response contradicts
// its own example, so the single-file call is the one the driver trusts.
func (p *Provider) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	fileID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return provider.Entry{}, fmt.Errorf("pan123: bad file id %q: %w", id, err)
	}
	if err := p.call(ctx, "file/name", http.MethodPut, pathName, ratelimit.Meta,
		map[string]any{"fileId": fileID, "fileName": newName}, nil, nil); err != nil {
		return provider.Entry{}, err
	}
	e, err := p.Stat(ctx, id)
	if err != nil {
		// The rename succeeded; report what we know rather than failing the
		// operation because the follow-up read was throttled. EnsureVersion
		// keeps the entry usable as a cache key.
		e := provider.Entry{ID: id, Name: newName, ModTime: p.now()}
		provider.EnsureVersion(&e)
		return e, nil
	}
	return e, nil
}

// Move relocates one entry. The endpoint is a batch call capped at 100 ids.
func (p *Provider) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	fileID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return provider.Entry{}, fmt.Errorf("pan123: bad file id %q: %w", id, err)
	}
	parent, err := strconv.ParseInt(newParentID, 10, 64)
	if err != nil {
		return provider.Entry{}, fmt.Errorf("pan123: bad parent id %q: %w", newParentID, err)
	}
	if err := p.call(ctx, "file/move", http.MethodPost, pathMove, ratelimit.Meta,
		map[string]any{"fileIDs": []int64{fileID}, "toParentFileID": parent}, nil, nil); err != nil {
		return provider.Entry{}, err
	}
	e, err := p.Stat(ctx, id)
	if err != nil {
		e := provider.Entry{ID: id, ParentID: newParentID, ModTime: p.now()}
		provider.EnsureVersion(&e)
		return e, nil
	}
	e.ParentID = newParentID
	return e, nil
}

// Delete moves an entry to the recycle bin.
func (p *Provider) Delete(ctx context.Context, id string) error {
	fileID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return fmt.Errorf("pan123: bad file id %q: %w", id, err)
	}
	return p.call(ctx, "file/trash", http.MethodPost, pathTrash, ratelimit.Meta,
		map[string]any{"fileIDs": []int64{fileID}}, nil, nil)
}

var (
	_ provider.Provider    = (*Provider)(nil)
	_ provider.Transporter = (*Provider)(nil)
)

// Factory builds a 123 Pan provider from its config block.
func Factory(name string, cfg map[string]any) (provider.Provider, error) {
	get := func(key string) string {
		v, ok := cfg[key]
		if !ok {
			return ""
		}
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}
	if get("access_token") == "" {
		if get("client_id") == "" {
			return nil, fmt.Errorf("pan123: remote %q is missing the required 'client_id' key", name)
		}
		if get("client_secret") == "" {
			return nil, fmt.Errorf("pan123: remote %q is missing the required 'client_secret' key", name)
		}
	}
	// Prefer the daemon's client: it already applies the proxy rules, the rate
	// limiter and the circuit breaker for this remote.
	const userAgent = "cloudfs/0.1"
	client, shared := httpx.FromConfig(cfg, httpx.Options{
		Remote: name, Account: get("client_id"), UserAgent: userAgent,
	})
	if shared && client.UserAgent == "" {
		client.UserAgent = userAgent
	}
	return New(Options{
		Name:         name,
		Client:       client,
		BaseURL:      get("base_url"),
		ClientID:     get("client_id"),
		ClientSecret: get("client_secret"),
		AccessToken:  get("access_token"),
		RootID:       get("root_id"),
	})
}

// SetTransport lets the daemon replace the HTTP client after construction.
func (p *Provider) SetTransport(client any) {
	if c, ok := client.(*httpx.Client); ok && c != nil {
		p.client = c
	}
}

func init() {
	provider.Register("pan123", Factory)
}
