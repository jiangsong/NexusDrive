// Package aliyun implements the Provider interface over the official Aliyun
// Drive (阿里云盘) Open API at openapi.alipan.com.
//
// The driver speaks only the documented Open API: OAuth2 refresh-token auth, a
// pre_hash → content_hash + proof_code rapid-upload handshake, and OSS-signed
// per-part upload URLs. Aliyun publishes no delta feed, so Caps.Delta is false
// and the package deliberately does not implement provider.ChangeLister
// (docs/DESIGN.md §4.1).
package aliyun

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// DefaultBaseURL is the Open API host. Requests are relative to it so tests
// can point the driver at an httptest.Server.
const DefaultBaseURL = "https://openapi.alipan.com"

// RootID is the id Aliyun uses for the drive root.
const RootID = "root"

// PartSize is the default upload part size. Aliyun accepts 100 KiB … 5 GiB per
// part; 4 MiB keeps a part inside one cache block and one retry cheap.
const PartSize = 4 << 20

// MaxParts is the highest part_number the Open API accepts for one upload.
const MaxParts = 10000

// preHashBytes is the prefix length Aliyun hashes as pre_hash.
const preHashBytes = 1 << 10

// API paths. Kept in one place because the Open API moved host twice already
// (aliyundrive.com → alipan.com) while the paths stayed stable.
const (
	pathToken       = "/oauth/access_token"
	pathDriveInfo   = "/adrive/v1.0/user/getDriveInfo"
	pathList        = "/adrive/v1.0/openFile/list"
	pathGet         = "/adrive/v1.0/openFile/get"
	pathDownloadURL = "/adrive/v1.0/openFile/getDownloadUrl"
	pathCreate      = "/adrive/v1.0/openFile/create"
	pathUploadURL   = "/adrive/v1.0/openFile/getUploadUrl"
	pathComplete    = "/adrive/v1.0/openFile/complete"
	pathUpdate      = "/adrive/v1.0/openFile/update"
	pathMove        = "/adrive/v1.0/openFile/move"
	pathCopy        = "/adrive/v1.0/openFile/copy"
	pathTrash       = "/adrive/v1.0/openFile/recyclebin/trash"
)

// ProofBytesFunc reads n bytes at off from the file that is about to be
// uploaded. Aliyun's rapid upload needs 8 bytes from a token-dependent offset
// (see ProofRange), which BeginUpload cannot obtain from its arguments; the
// uploader normally supplies provider.UploadContentReader through context.
// This hook is an optional fallback for direct users of the driver. Without
// either source the driver omits the proof and falls back to chunked transfer.
type ProofBytesFunc func(ctx context.Context, parentID, name string, off, n int64) ([]byte, error)

// Options configures a Provider.
type Options struct {
	// Name is the remote name used in metadata and cache keys.
	Name string
	// Client is the shared HTTP client. Required.
	Client *httpx.Client
	// BaseURL overrides DefaultBaseURL (tests point it at httptest).
	BaseURL string
	// ClientID and ClientSecret identify the Open API application.
	ClientID, ClientSecret string
	// RefreshToken is the long-lived credential. Required unless AccessToken
	// is supplied and never expires, which the Open API never does.
	RefreshToken string
	// AccessToken seeds the first request; without it the driver refreshes on
	// the first call.
	AccessToken string
	// DriveID pins the drive. Empty means ask getDriveInfo.
	DriveID string
	// RootID overrides the root file id (default "root").
	RootID string
	// LinkTTL is how long a download URL is requested for. Aliyun caps it at
	// 4 h and defaults to 15 min.
	LinkTTL time.Duration
	// ProofBytes supplies the rapid-upload proof range; optional.
	ProofBytes ProofBytesFunc
	// Now is injectable for tests.
	Now func() time.Time
}

// Provider is the Aliyun Drive backend.
type Provider struct {
	provider.TokenPersistence
	refreshMu sync.Mutex
	name      string
	base      string
	client    *httpx.Client
	caps      provider.Caps
	rootID    string
	linkTTL   time.Duration
	proof     ProofBytesFunc
	now       func() time.Time

	clientID     string
	clientSecret string

	mu           sync.Mutex
	accessToken  string
	refreshToken string
	expiry       time.Time
	driveID      string

	// partURLs caches the OSS upload URLs create handed back, keyed by
	// upload_id. UploadPart falls back to getUploadUrl when a URL is missing
	// (a resumed session) or rejected as expired.
	partMu   sync.Mutex
	partURLs map[string]map[int]string

	// links caches download URLs until shortly before they expire; the block
	// cache asks for many ranges of the same file in a row.
	linkMu sync.Mutex
	links  map[string]provider.Link
}

// New builds a Provider from explicit options. Factory wraps it for the
// registry.
func New(opt Options) (*Provider, error) {
	if opt.Client == nil {
		return nil, errors.New("aliyun: Client is required")
	}
	if opt.RefreshToken == "" && opt.AccessToken == "" {
		return nil, errors.New("aliyun: remote is missing the required 'refresh_token' key")
	}
	base := opt.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	root := opt.RootID
	if root == "" {
		root = RootID
	}
	ttl := opt.LinkTTL
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	p := &Provider{
		name:         opt.Name,
		base:         strings.TrimSuffix(base, "/"),
		client:       opt.Client,
		rootID:       root,
		linkTTL:      ttl,
		proof:        opt.ProofBytes,
		now:          now,
		clientID:     opt.ClientID,
		clientSecret: opt.ClientSecret,
		accessToken:  opt.AccessToken,
		refreshToken: opt.RefreshToken,
		driveID:      opt.DriveID,
		partURLs:     map[string]map[int]string{},
		links:        map[string]provider.Link{},
		caps: provider.Caps{
			HashTypes: []provider.HashType{provider.HashSHA1},
			// pre_sha1 short-circuits the handshake; sha1 is the content hash
			// the server matches against.
			RapidUpload:     []provider.HashType{provider.HashPreSHA1, provider.HashSHA1},
			RangeRead:       true,
			PartSize:        PartSize,
			MaxParts:        MaxParts,
			UploadParallel:  4,
			ServerMove:      true,
			ServerRename:    true,
			ServerCopy:      true,
			Delta:           false,
			LinkTTL:         ttl,
			LinkShareable:   true,
			QPS:             provider.QPS{Meta: 4, Download: 4, Upload: 2},
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

// RootFileID returns the file id the driver treats as the root.
func (p *Provider) RootFileID() string { return p.rootID }

// ---------------------------------------------------------------- errors ---

// APIError is Aliyun's error envelope: {"code":"NotFound.FileId","message":…}.
// It wraps the transport error so both the HTTP status and the provider
// sentinel remain reachable with errors.Is / errors.As.
type APIError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId"`

	// Status is the HTTP status the envelope arrived with.
	Status int `json:"-"`
	cause  error
}

// Error implements error.
func (e *APIError) Error() string {
	return fmt.Sprintf("aliyun: %s (%s, http %d)", e.Message, e.Code, e.Status)
}

// HTTPStatus implements retry.HTTPStatuser.
func (e *APIError) HTTPStatus() int { return e.Status }

// Unwrap maps the documented error codes onto provider sentinels. Codes win
// over the HTTP status because Aliyun returns 403 both for "no permission" and
// for a drive that is locked, and 400 both for a missing file and for a bad
// argument.
func (e *APIError) Unwrap() error {
	switch {
	case e.Code == "":
		// No envelope: let the transport error classify itself.
	case e.Code == "NotFound.UploadId":
		// The upload session is gone. Reporting ErrNotFound makes the upload
		// layer discard the session and start a fresh create instead of
		// retrying parts against a dead upload_id.
		return provider.ErrNotFound
	case strings.HasPrefix(e.Code, "NotFound"), e.Code == "ForbiddenFileInTheRecycleBin":
		// NotFound.FileId arrives with 404, NotFound.File with 400.
		return provider.ErrNotFound
	case strings.HasPrefix(e.Code, "AlreadyExist"):
		// UNVERIFIED: AlreadyExist.File is documented for the private web API;
		// the Open API signals a name collision with the "exist" flag in the
		// create/move response instead, which Mkdir and Move check. Mapping it
		// costs nothing if the code never appears.
		return provider.ErrExists
	case e.Code == "AccessTokenInvalid", e.Code == "AccessTokenExpired",
		e.Code == "RefreshTokenInvalid", e.Code == "RefreshTokenExpired",
		e.Code == "I400JD", // opaque token-invalid code seen in the wild
		e.Code == "PermissionDenied", e.Code == "UserNotAllowedAccessDrive",
		strings.HasPrefix(e.Code, "Unauthorized"):
		return provider.ErrAuth
	case strings.HasPrefix(e.Code, "QpsLimitExceed"):
		// Per-API QPS ceiling: back off, the account is not in trouble.
		//
		// UNVERIFIED: QpsLimitExceed is documented for the private API; the
		// Open API is documented to answer 429 TooManyRequests instead. The
		// mapping is kept because it is harmless and the code is still
		// reported by some endpoints.
		return provider.ErrRateLimited
	case e.Code == "TooManyRequests":
		// Aliyun's own guidance is that retrying a 429 without honouring the
		// x-retry-after header (milliseconds) gets the user blocked, so an
		// account-wide 429 trips the circuit breaker rather than merely
		// slowing the bucket (docs/DESIGN.md §4.2 risk-control row).
		return provider.ErrRiskControl
	}
	return e.cause
}

// IsPreHashMatched reports whether the error is Aliyun's "the first 1 KiB
// matched an existing file, send the full hash" response. It is a control-flow
// signal, not a failure.
func IsPreHashMatched(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == "PreHashMatched"
}

// asAPIError converts a transport error into an APIError when the body carries
// Aliyun's envelope; otherwise it returns err unchanged.
func asAPIError(err error) error {
	var se *httpx.StatusError
	if !errors.As(err, &se) {
		return err
	}
	ae := &APIError{Status: se.Code, cause: err}
	// A body that is not the envelope leaves Code empty, which keeps the
	// status-based classification.
	_ = json.Unmarshal([]byte(se.Body), ae)
	return ae
}

// ----------------------------------------------------------------- token ---

type tokenResp struct {
	TokenType    string `json:"token_type"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// token returns a usable access token, refreshing when it is missing or within
// a minute of expiry.
func (p *Provider) token(ctx context.Context) (string, error) {
	if err := p.FlushTokens(); err != nil {
		return "", fmt.Errorf("aliyun: save credentials: %w", err)
	}
	p.mu.Lock()
	tok, exp := p.accessToken, p.expiry
	p.mu.Unlock()
	if err := p.FlushTokens(); err != nil {
		return "", err
	}
	if tok != "" && (exp.IsZero() || p.now().Add(time.Minute).Before(exp)) {
		return tok, nil
	}
	return p.refresh(ctx)
}

// refresh exchanges the refresh token for a new access token. A failure here
// is terminal for the remote: the user has to re-authorise, so it reports
// provider.ErrAuth.
func (p *Provider) refresh(ctx context.Context) (string, error) {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	if err := p.FlushTokens(); err != nil {
		return "", err
	}
	p.mu.Lock()
	rt := p.refreshToken
	p.mu.Unlock()
	if rt == "" {
		return "", fmt.Errorf("%w: aliyun: no refresh_token configured", provider.ErrAuth)
	}
	body := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": rt,
	}
	if p.clientID != "" {
		body["client_id"] = p.clientID
		body["client_secret"] = p.clientSecret
	}
	var out tokenResp
	err := p.client.JSON(ctx, httpx.Request{
		Method: http.MethodPost,
		URL:    p.base + pathToken,
		Class:  ratelimit.Meta,
		JSON:   body,
	}, &out)
	if err != nil {
		return "", fmt.Errorf("%w: aliyun: refresh token rejected: %w", provider.ErrAuth, asAPIError(err))
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("%w: aliyun: refresh returned no access_token", provider.ErrAuth)
	}
	p.mu.Lock()
	p.accessToken = out.AccessToken
	if out.RefreshToken != "" {
		// Aliyun rotates the refresh token on every exchange; keeping the old
		// one would lock the account out after the next restart.
		p.refreshToken = out.RefreshToken
	}
	if out.ExpiresIn > 0 {
		p.expiry = p.now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	saveErr := p.SaveTokens(map[string]string{"refresh_token": out.RefreshToken})
	p.mu.Unlock()
	if saveErr != nil {
		return "", fmt.Errorf("aliyun: save refreshed credentials: %w", saveErr)
	}
	return out.AccessToken, nil
}

// RefreshToken returns the current refresh token, which rotates on every
// exchange. The caller persists it so a restart can still authenticate.
func (p *Provider) RefreshToken() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refreshToken
}

// call performs an authenticated JSON POST and decodes out. On 401 it refreshes
// the access token once and retries, which is the only retry the driver adds on
// top of httpx's transport-level policy.
func (p *Provider) call(ctx context.Context, path string, class ratelimit.Class, in, out any) error {
	tok, err := p.token(ctx)
	if err != nil {
		return err
	}
	err = p.callWith(ctx, tok, path, class, in, out)
	if err != nil && errors.Is(err, provider.ErrAuth) {
		tok, rerr := p.refresh(ctx)
		if rerr != nil {
			// Keep the original failure in the chain: it names the code the
			// server actually reported, which the refresh error does not.
			return fmt.Errorf("%w (token refresh failed: %v)", err, rerr)
		}
		return p.callWith(ctx, tok, path, class, in, out)
	}
	return err
}

func (p *Provider) callWith(ctx context.Context, tok, path string, class ratelimit.Class, in, out any) error {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tok)
	err := p.client.JSON(ctx, httpx.Request{
		Method: http.MethodPost,
		URL:    p.base + path,
		Class:  class,
		Header: h,
		JSON:   in,
	}, out)
	if err != nil {
		return asAPIError(err)
	}
	return nil
}

// drive resolves the drive id, asking getDriveInfo once when it is not pinned.
func (p *Provider) drive(ctx context.Context) (string, error) {
	p.mu.Lock()
	id := p.driveID
	p.mu.Unlock()
	if id != "" {
		return id, nil
	}
	var out struct {
		DefaultDriveID  string `json:"default_drive_id"`
		ResourceDriveID string `json:"resource_drive_id"`
		BackupDriveID   string `json:"backup_drive_id"`
	}
	if err := p.call(ctx, pathDriveInfo, ratelimit.Meta, map[string]any{}, &out); err != nil {
		return "", err
	}
	// Prefer the resource drive: it is the one the web UI calls "我的文件".
	// The backup drive holds phone backups and is usually not what a mount
	// should expose.
	id = firstNonEmpty(out.ResourceDriveID, out.DefaultDriveID, out.BackupDriveID)
	if id == "" {
		return "", errors.New("aliyun: getDriveInfo returned no drive id")
	}
	p.mu.Lock()
	p.driveID = id
	p.mu.Unlock()
	return id, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// ------------------------------------------------------------------ items ---

// item is one file or folder as the Open API reports it.
type item struct {
	DriveID         string `json:"drive_id"`
	FileID          string `json:"file_id"`
	ParentFileID    string `json:"parent_file_id"`
	Name            string `json:"name"`
	Type            string `json:"type"` // "file" | "folder"
	Size            int64  `json:"size"`
	ContentHash     string `json:"content_hash"`
	ContentHashName string `json:"content_hash_name"`
	UpdatedAt       string `json:"updated_at"`
	CreatedAt       string `json:"created_at"`
}

func (it item) entry() provider.Entry {
	e := provider.Entry{
		ID:       it.FileID,
		ParentID: it.ParentFileID,
		Name:     it.Name,
		Kind:     provider.KindFile,
		Size:     it.Size,
	}
	if it.Type == "folder" {
		e.Kind = provider.KindDir
		e.Size = 0
	}
	if t, err := time.Parse(time.RFC3339, it.UpdatedAt); err == nil {
		e.ModTime = t
	}
	if it.ContentHash != "" && strings.EqualFold(it.ContentHashName, "sha1") {
		e.Hashes = provider.Hashes{provider.HashSHA1: strings.ToLower(it.ContentHash)}
	}
	// content_hash is the strongest change token available: equal hashes mean
	// identical bytes. Folders have none, so they fall back to updated_at.
	e.Version = strings.ToLower(it.ContentHash)
	if e.Version == "" {
		e.Version = it.UpdatedAt
	}
	return e
}

// List returns one page of a folder's children. cursor is Aliyun's next_marker.
func (p *Provider) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	drive, err := p.drive(ctx)
	if err != nil {
		return nil, "", err
	}
	if dirID == "" {
		dirID = p.rootID
	}
	req := map[string]any{
		"drive_id":        drive,
		"parent_file_id":  dirID,
		"limit":           100,
		"order_by":        "name",
		"order_direction": "ASC",
	}
	if cursor != "" {
		req["marker"] = cursor
	}
	var out struct {
		Items      []item `json:"items"`
		NextMarker string `json:"next_marker"`
	}
	if err := p.call(ctx, pathList, ratelimit.Meta, req, &out); err != nil {
		return nil, "", err
	}
	entries := make([]provider.Entry, 0, len(out.Items))
	for _, it := range out.Items {
		e := it.entry()
		if e.ParentID == "" {
			e.ParentID = dirID
		}
		entries = append(entries, e)
	}
	return entries, out.NextMarker, nil
}

// Stat returns one entry by file id.
func (p *Provider) Stat(ctx context.Context, id string) (provider.Entry, error) {
	drive, err := p.drive(ctx)
	if err != nil {
		return provider.Entry{}, err
	}
	if id == "" {
		id = p.rootID
	}
	var it item
	if err := p.call(ctx, pathGet, ratelimit.Meta, map[string]any{
		"drive_id": drive, "file_id": id,
	}, &it); err != nil {
		return provider.Entry{}, err
	}
	if it.FileID == "" {
		return provider.Entry{}, provider.ErrNotFound
	}
	return it.entry(), nil
}

// DownloadURL returns a time-limited direct link. Links are cached until a
// minute before they expire because the block cache asks for many ranges of
// the same file in a row and getDownloadUrl counts against the meta QPS.
func (p *Provider) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	p.linkMu.Lock()
	if l, ok := p.links[id]; ok && p.now().Add(time.Minute).Before(l.ExpiresAt) {
		p.linkMu.Unlock()
		return l, nil
	}
	p.linkMu.Unlock()

	drive, err := p.drive(ctx)
	if err != nil {
		return provider.Link{}, err
	}
	var out struct {
		URL        string `json:"url"`
		Expiration string `json:"expiration"`
		Method     string `json:"method"`
	}
	if err := p.call(ctx, pathDownloadURL, ratelimit.Download, map[string]any{
		"drive_id": drive, "file_id": id,
		"expire_sec": int64(p.linkTTL / time.Second),
	}, &out); err != nil {
		return provider.Link{}, err
	}
	if out.URL == "" {
		return provider.Link{}, fmt.Errorf("%w: aliyun: getDownloadUrl returned no url for %s", provider.ErrNotFound, id)
	}
	link := provider.Link{URL: out.URL, ExpiresAt: p.now().Add(p.linkTTL)}
	if t, err := time.Parse(time.RFC3339, out.Expiration); err == nil {
		link.ExpiresAt = t
	}
	if len(p.caps.LinkHeaders) > 0 {
		link.Headers = p.caps.LinkHeaders
	}
	p.linkMu.Lock()
	p.links[id] = link
	p.linkMu.Unlock()
	return link, nil
}

// ReadRange fetches a byte range from the CDN. An expired link answers 403, so
// one refresh-and-retry is built in; anything else is handed up as-is.
func (p *Provider) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	link, err := p.DownloadURL(ctx, id)
	if err != nil {
		return nil, err
	}
	body, err := p.getRange(ctx, link, off, n)
	if err != nil && errors.Is(err, provider.ErrLinkExpired) {
		p.linkMu.Lock()
		delete(p.links, id)
		p.linkMu.Unlock()
		link, err2 := p.DownloadURL(ctx, id)
		if err2 != nil {
			return nil, err2
		}
		return p.getRange(ctx, link, off, n)
	}
	return body, err
}

func (p *Provider) getRange(ctx context.Context, link provider.Link, off, n int64) (io.ReadCloser, error) {
	h := http.Header{}
	h.Set("Range", httpx.RangeHeader(off, n))
	for k, v := range link.Headers {
		h.Set(k, v)
	}
	resp, err := p.client.Do(ctx, httpx.Request{
		Method:       http.MethodGet,
		URL:          link.URL,
		Class:        ratelimit.Download,
		Header:       h,
		ExpectStatus: []int{http.StatusPartialContent, http.StatusOK},
		Stream:       true,
	})
	if err != nil {
		return nil, err
	}
	// A CDN node that ignored Range sent the whole object; skip forward so the
	// caller always gets the bytes it asked for.
	if resp.Status == http.StatusOK && off > 0 {
		if _, err := io.CopyN(io.Discard, resp.Body, off); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("aliyun: server ignored Range and the object is shorter than offset %d: %w", off, err)
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

// ProofRange returns the byte range Aliyun wants a proof of for a file of the
// given size, for the supplied access token.
//
// The offset is md5(access_token) truncated to its first 16 hex digits, read as
// a big-endian uint64 and taken modulo the file size; the proof is the 8 bytes
// (fewer at EOF) starting there.
//
// The algorithm is transcribed from the getProofRange snippet in Aliyun's own
// upload guide, which the AList/OpenList driver implements identically.
//
// UNVERIFIED: the end-to-end rapid path (create with content_hash +
// proof_code returning rapid_upload=true) has never been exercised against a
// live account by this project. A wrong proof only makes the server refuse the
// rapid upload, which BeginUpload handles by falling back to a chunked upload,
// so a mistake here cannot corrupt data.
func ProofRange(accessToken string, size int64) (off, n int64) {
	if size <= 0 {
		return 0, 0
	}
	sum := md5.Sum([]byte(accessToken))
	hexSum := hex.EncodeToString(sum[:])
	v, err := strconv.ParseUint(hexSum[:16], 16, 64)
	if err != nil {
		return 0, 0
	}
	off = int64(v % uint64(size))
	n = 8
	if off+n > size {
		n = size - off
	}
	return off, n
}

// ProofCode encodes the proof bytes the way the Open API expects them.
func ProofCode(proofBytes []byte) string {
	return base64.StdEncoding.EncodeToString(proofBytes)
}

// createResp is the openFile/create response.
type createResp struct {
	DriveID      string `json:"drive_id"`
	FileID       string `json:"file_id"`
	UploadID     string `json:"upload_id"`
	FileName     string `json:"file_name"`
	RapidUpload  bool   `json:"rapid_upload"`
	Exist        bool   `json:"exist"`
	Status       string `json:"status"`
	PartInfoList []struct {
		PartNumber int    `json:"part_number"`
		UploadURL  string `json:"upload_url"`
	} `json:"part_info_list"`
}

// BeginUpload starts an upload, attempting the rapid (秒传) path first.
//
// The handshake is the documented three-step one:
//
//  1. create with pre_hash only. A miss returns a normal upload session, so the
//     probe costs nothing when the file is new.
//  2. a 409 "PreHashMatched" means some file starts with the same 1 KiB; retry
//     create with content_hash + proof_code.
//  3. rapid_upload=true means the server adopted the bytes and the entry is
//     final; otherwise the returned upload_id is used for a chunked upload.
func (p *Provider) BeginUpload(ctx context.Context, parentID, name string, size int64, h provider.Hashes) (provider.UploadSession, error) {
	drive, err := p.drive(ctx)
	if err != nil {
		return provider.UploadSession{}, err
	}
	if parentID == "" {
		parentID = p.rootID
	}
	parts := partCount(size, PartSize)

	sha1sum := strings.ToUpper(h[provider.HashSHA1])
	preSHA1 := strings.ToUpper(h[provider.HashPreSHA1])

	// Step 1: the pre_hash probe. It is free on a miss — the response is a
	// usable upload session — and for a file no larger than the probed prefix
	// the pre_hash is the content hash, so the probe is skipped there.
	if preSHA1 != "" && size > preHashBytes {
		req := p.createRequest(drive, parentID, name, size, parts)
		req["pre_hash"] = preSHA1
		var out createResp
		err := p.call(ctx, pathCreate, ratelimit.Upload, req, &out)
		switch {
		case err == nil:
			// No file on the server starts with these bytes: this session is
			// a plain chunked upload, no second round trip needed.
			return p.session(drive, out, size), nil
		case IsPreHashMatched(err):
			// fall through to the content-hash round
		default:
			return provider.UploadSession{}, err
		}
	}

	// Step 2: the content-hash round, when we can prove ownership of the bytes.
	if sha1sum != "" {
		req := p.createRequest(drive, parentID, name, size, parts)
		req["content_hash"] = sha1sum
		req["content_hash_name"] = "sha1"
		if code, ok, err := p.proofCode(ctx, parentID, name, size); err != nil {
			return provider.UploadSession{}, err
		} else if ok {
			req["proof_code"] = code
			req["proof_version"] = "v1"
		}
		var out createResp
		if err := p.call(ctx, pathCreate, ratelimit.Upload, req, &out); err != nil {
			// A rejected rapid attempt must never fail the upload: retry
			// without any hash so the file still lands via chunked transfer.
			if !errors.Is(err, provider.ErrAuth) && !errors.Is(err, provider.ErrRateLimited) &&
				!errors.Is(err, provider.ErrRiskControl) {
				return p.beginPlain(ctx, drive, parentID, name, size, parts)
			}
			return provider.UploadSession{}, err
		}
		s := p.session(drive, out, size)
		if out.RapidUpload {
			s.RapidDone = true
			e := provider.Entry{
				ID: out.FileID, ParentID: parentID, Name: pick(out.FileName, name),
				Kind: provider.KindFile, Size: size, ModTime: p.now(),
				Hashes:  provider.Hashes{provider.HashSHA1: strings.ToLower(sha1sum)},
				Version: strings.ToLower(sha1sum),
			}
			s.Entry = &e
			return s, nil
		}
		if out.UploadID != "" {
			return s, nil
		}
	}

	// Step 3: no usable hashes, or the rapid round produced no session.
	return p.beginPlain(ctx, drive, parentID, name, size, parts)
}

func (p *Provider) beginPlain(ctx context.Context, drive, parentID, name string, size int64, parts int) (provider.UploadSession, error) {
	var out createResp
	if err := p.call(ctx, pathCreate, ratelimit.Upload,
		p.createRequest(drive, parentID, name, size, parts), &out); err != nil {
		return provider.UploadSession{}, err
	}
	if out.UploadID == "" && !out.RapidUpload {
		return provider.UploadSession{}, fmt.Errorf("aliyun: create returned no upload_id for %q", name)
	}
	return p.session(drive, out, size), nil
}

func (p *Provider) createRequest(drive, parentID, name string, size int64, parts int) map[string]any {
	list := make([]map[string]any, 0, parts)
	for i := 1; i <= parts; i++ {
		list = append(list, map[string]any{"part_number": i})
	}
	return map[string]any{
		"drive_id":       drive,
		"parent_file_id": parentID,
		"name":           name,
		"type":           "file",
		// refuse rather than auto_rename: the VFS owns conflict resolution and
		// silently renaming would hide a lost update.
		"check_name_mode": "refuse",
		"size":            size,
		"part_info_list":  list,
	}
}

// proofCode uses the upload's immutable content, or the optional ProofBytes
// hook for standalone callers. Missing or short proof bytes cause a fallback
// to a plain content_hash create; cancellation is never swallowed.
func (p *Provider) proofCode(ctx context.Context, parentID, name string, size int64) (string, bool, error) {
	read := provider.UploadContentReaderFrom(ctx)
	if read == nil && p.proof != nil {
		read = func(ctx context.Context, off, n int64) ([]byte, error) { return p.proof(ctx, parentID, name, off, n) }
	}
	if read == nil || size <= 0 {
		return "", false, nil
	}
	tok, err := p.token(ctx)
	if err != nil {
		return "", false, err
	}
	off, n := ProofRange(tok, size)
	if n <= 0 {
		return "", false, nil
	}
	b, err := read(ctx, off, n)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", false, err
		}
		// Not being able to read the proof range is not fatal: skip the proof
		// and let the upload go the chunked way.
		return "", false, nil
	}
	if int64(len(b)) != n {
		return "", false, nil
	}
	return ProofCode(b), true, nil
}

func (p *Provider) session(drive string, out createResp, size int64) provider.UploadSession {
	s := provider.UploadSession{
		ID:       out.UploadID,
		PartSize: PartSize,
		Opaque: map[string]string{
			"drive_id":  drive,
			"file_id":   out.FileID,
			"upload_id": out.UploadID,
			"size":      strconv.FormatInt(size, 10),
		},
	}
	if out.UploadID != "" && len(out.PartInfoList) > 0 {
		urls := make(map[int]string, len(out.PartInfoList))
		for _, pi := range out.PartInfoList {
			if pi.UploadURL != "" {
				urls[pi.PartNumber] = pi.UploadURL
			}
		}
		p.partMu.Lock()
		p.partURLs[out.UploadID] = urls
		p.partMu.Unlock()
	}
	return s
}

func partCount(size, part int64) int {
	if size <= 0 {
		return 1
	}
	n := (size + part - 1) / part
	if n > MaxParts {
		n = MaxParts
	}
	return int(n)
}

func pick(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// UploadPart PUTs one part to its OSS-signed URL. idx is 0-based; Aliyun's
// part_number is 1-based.
func (p *Provider) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	partNo := idx + 1
	url, err := p.partURL(ctx, s, partNo, false)
	if err != nil {
		return provider.PartToken{}, err
	}
	etag, err := p.putPart(ctx, url, r, n)
	if err != nil && errors.Is(err, provider.ErrLinkExpired) {
		// OSS signatures expire an hour after create; a long upload has to
		// re-sign the remaining parts. The body is spent, so this can only be
		// retried when the caller gave us a seekable reader.
		if seeker, ok := r.(io.Seeker); ok {
			if _, serr := seeker.Seek(0, io.SeekStart); serr == nil {
				url, uerr := p.partURL(ctx, s, partNo, true)
				if uerr != nil {
					return provider.PartToken{}, uerr
				}
				etag, err = p.putPart(ctx, url, r, n)
			}
		}
	}
	if err != nil {
		return provider.PartToken{}, err
	}
	return provider.PartToken{Index: idx, ETag: etag}, nil
}

func (p *Provider) putPart(ctx context.Context, url string, r io.Reader, n int64) (string, error) {
	h := http.Header{}
	h.Set("Content-Length", strconv.FormatInt(n, 10))
	resp, err := p.client.Do(ctx, httpx.Request{
		Method: http.MethodPut,
		URL:    url,
		Class:  ratelimit.Upload,
		// Deliberately no Content-Type: the OSS signature in upload_url does
		// not cover one, and sending it makes OSS answer SignatureDoesNotMatch.
		Header: h,
		Body:   io.LimitReader(r, n),
		// OSS answers 409 when the part is already there, which is what a
		// retried part looks like; treating it as success keeps a resumed
		// upload from dead-ending.
		ExpectStatus: []int{http.StatusOK, http.StatusCreated, http.StatusConflict},
	})
	if err != nil {
		return "", err
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	// A 409 is only safe to treat as success when OSS also hands back the
	// existing part's ETag. Without one there is nothing to send to
	// CompleteUpload, and returning an empty token would fail the completion
	// with a confusing error far from the cause.
	if etag == "" && resp.Status == http.StatusConflict {
		return "", fmt.Errorf("aliyun: part upload returned 409 with no ETag; the upload session must be restarted: %w", provider.ErrNotFound)
	}
	return etag, nil
}

// partURL returns the signed URL for one part, asking getUploadUrl when the
// cache has none (a session resumed from the journal) or when force is set.
func (p *Provider) partURL(ctx context.Context, s provider.UploadSession, partNo int, force bool) (string, error) {
	uploadID := pick(s.Opaque["upload_id"], s.ID)
	if !force {
		p.partMu.Lock()
		u := p.partURLs[uploadID][partNo]
		p.partMu.Unlock()
		if u != "" {
			return u, nil
		}
	}
	drive := s.Opaque["drive_id"]
	if drive == "" {
		var err error
		if drive, err = p.drive(ctx); err != nil {
			return "", err
		}
	}
	var out createResp
	if err := p.call(ctx, pathUploadURL, ratelimit.Upload, map[string]any{
		"drive_id":  drive,
		"file_id":   s.Opaque["file_id"],
		"upload_id": uploadID,
		"part_info_list": []map[string]any{
			{"part_number": partNo},
		},
	}, &out); err != nil {
		return "", err
	}
	p.partMu.Lock()
	if p.partURLs[uploadID] == nil {
		p.partURLs[uploadID] = map[int]string{}
	}
	var url string
	for _, pi := range out.PartInfoList {
		p.partURLs[uploadID][pi.PartNumber] = pi.UploadURL
		if pi.PartNumber == partNo {
			url = pi.UploadURL
		}
	}
	p.partMu.Unlock()
	if url == "" {
		return "", fmt.Errorf("aliyun: getUploadUrl returned no url for part %d of %s", partNo, uploadID)
	}
	return url, nil
}

// CompleteUpload finalises the upload. parts is accepted for interface
// symmetry: Aliyun tracks the parts server-side and complete takes only the
// upload id.
func (p *Provider) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	drive := s.Opaque["drive_id"]
	if drive == "" {
		var err error
		if drive, err = p.drive(ctx); err != nil {
			return provider.Entry{}, err
		}
	}
	var it item
	if err := p.call(ctx, pathComplete, ratelimit.Upload, map[string]any{
		"drive_id":  drive,
		"file_id":   s.Opaque["file_id"],
		"upload_id": pick(s.Opaque["upload_id"], s.ID),
	}, &it); err != nil {
		return provider.Entry{}, err
	}
	p.partMu.Lock()
	delete(p.partURLs, pick(s.Opaque["upload_id"], s.ID))
	p.partMu.Unlock()
	if it.FileID == "" {
		return provider.Entry{}, fmt.Errorf("aliyun: complete returned no file_id for upload %s", s.ID)
	}
	e := it.entry()
	if e.Size == 0 {
		if sz, err := strconv.ParseInt(s.Opaque["size"], 10, 64); err == nil {
			e.Size = sz
		}
	}
	return e, nil
}

// ------------------------------------------------------------ namespace ---

// Mkdir creates a folder. check_name_mode "refuse" turns an existing name into
// provider.ErrExists instead of a silent auto-rename.
func (p *Provider) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	drive, err := p.drive(ctx)
	if err != nil {
		return provider.Entry{}, err
	}
	if parentID == "" {
		parentID = p.rootID
	}
	var out createResp
	if err := p.call(ctx, pathCreate, ratelimit.Meta, map[string]any{
		"drive_id":        drive,
		"parent_file_id":  parentID,
		"name":            name,
		"type":            "folder",
		"check_name_mode": "refuse",
	}, &out); err != nil {
		return provider.Entry{}, err
	}
	if out.Exist {
		return provider.Entry{}, provider.ErrExists
	}
	return provider.Entry{
		ID: out.FileID, ParentID: parentID, Name: pick(out.FileName, name),
		Kind: provider.KindDir, ModTime: p.now(),
	}, nil
}

// Rename changes an entry's name in place.
func (p *Provider) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	drive, err := p.drive(ctx)
	if err != nil {
		return provider.Entry{}, err
	}
	var it item
	if err := p.call(ctx, pathUpdate, ratelimit.Meta, map[string]any{
		"drive_id": drive, "file_id": id, "name": newName,
		"check_name_mode": "refuse",
	}, &it); err != nil {
		return provider.Entry{}, err
	}
	if it.FileID == "" {
		return p.Stat(ctx, id)
	}
	return it.entry(), nil
}

// moveResp is what move and copy return: the new id plus, for large folders,
// an async task the server runs in the background.
type moveResp struct {
	DriveID     string `json:"drive_id"`
	FileID      string `json:"file_id"`
	AsyncTaskID string `json:"async_task_id"`
	Exist       bool   `json:"exist"`
}

// Move relocates an entry under a new parent, keeping its name.
func (p *Provider) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	drive, err := p.drive(ctx)
	if err != nil {
		return provider.Entry{}, err
	}
	var out moveResp
	if err := p.call(ctx, pathMove, ratelimit.Meta, map[string]any{
		"drive_id": drive, "file_id": id, "to_parent_file_id": newParentID,
		"check_name_mode": "refuse",
	}, &out); err != nil {
		return provider.Entry{}, err
	}
	if out.Exist {
		return provider.Entry{}, provider.ErrExists
	}
	// A folder move can be async; the id is stable either way, so Stat gives
	// the caller the authoritative entry.
	// UNVERIFIED: whether Stat immediately reflects the new parent_file_id
	// while async_task_id is still running.
	return p.Stat(ctx, pick(out.FileID, id))
}

// Copy duplicates an entry server-side, implementing provider.ServerCopier.
func (p *Provider) Copy(ctx context.Context, id, newParentID, newName string) (provider.Entry, error) {
	drive, err := p.drive(ctx)
	if err != nil {
		return provider.Entry{}, err
	}
	req := map[string]any{
		"drive_id": drive, "file_id": id, "to_parent_file_id": newParentID,
		"auto_rename": false,
	}
	if newName != "" {
		req["new_name"] = newName
	}
	var out moveResp
	if err := p.call(ctx, pathCopy, ratelimit.Meta, req, &out); err != nil {
		return provider.Entry{}, err
	}
	if out.FileID == "" {
		return provider.Entry{}, fmt.Errorf("aliyun: copy returned no file_id for %s", id)
	}
	return p.Stat(ctx, out.FileID)
}

// Delete moves an entry to the recycle bin. The Open API also has a permanent
// delete; the recoverable one is the safer default for a filesystem where an
// rm can be a mistake.
func (p *Provider) Delete(ctx context.Context, id string) error {
	drive, err := p.drive(ctx)
	if err != nil {
		return err
	}
	var out moveResp
	return p.call(ctx, pathTrash, ratelimit.Meta, map[string]any{
		"drive_id": drive, "file_id": id,
	}, &out)
}

var (
	_ provider.Provider     = (*Provider)(nil)
	_ provider.ServerCopier = (*Provider)(nil)
	_ provider.Transporter  = (*Provider)(nil)
)

// Factory builds an Aliyun provider from its config block.
func Factory(name string, cfg map[string]any) (provider.Provider, error) {
	get := func(key string) string {
		v, ok := cfg[key]
		if !ok {
			return ""
		}
		switch s := v.(type) {
		case string:
			return s
		case fmt.Stringer:
			return s.String()
		default:
			return fmt.Sprint(v)
		}
	}
	if get("refresh_token") == "" && get("access_token") == "" {
		return nil, fmt.Errorf("aliyun: remote %q needs refresh_token or access_token", name)
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
		RefreshToken: get("refresh_token"),
		AccessToken:  get("access_token"),
		DriveID:      get("drive_id"),
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
	provider.Register("aliyun", Factory)
	provider.RegisterFields("aliyun", []provider.Field{
		{Name: "client_id", Prompt: "开放平台 client id / AppId"},
		{Name: "drive_id", Prompt: "网盘 drive id，留空则登录后自动取得"},
		{Name: "root_id", Prompt: "作为根的目录 id", Default: "root"},
	}, provider.Credentials{Fields: []string{"refresh_token", "client_secret", "access_token"},
		Note: "config auth 会打开浏览器完成授权"})

}
