// Package baidu implements the Provider interface over the official Baidu
// NetDisk (百度网盘) open platform API at pan.baidu.com/rest/2.0/xpan.
//
// Two quirks shape the whole driver:
//
//   - The API addresses files by path for every management call but only by
//     fs_id for metadata and download links, so Entry.ID carries both (see
//     FileID).
//   - A download must carry User-Agent: pan.baidu.com. Baidu documents this as
//     unconditional and answers errno 31326 (命中防盗链) without it; in the
//     field it is anything over roughly 20 MB that fails. The header is
//     therefore both the client's default UA and part of Caps.LinkHeaders,
//     which MCP's get_download_url passes on to whoever fetches the link
//     (docs/DESIGN.md §4.1).
//
// Baidu publishes no delta feed, so Caps.Delta is false and this package does
// not implement provider.ChangeLister.
package baidu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// DefaultBaseURL hosts the xpan REST API.
const DefaultBaseURL = "https://pan.baidu.com"

// DefaultUploadURL hosts superfile2, the block upload endpoint. It is a
// different origin from the REST API and must stay separately configurable.
//
// UNVERIFIED: Baidu documents that the upload host should be discovered per
// upload via /rest/2.0/pcs/file?method=locateupload (which hands back a list of
// cN.pcs.baidu.com servers valid for about an hour). This driver uses the
// fallback host both reference implementations fall back to. Needs checking
// against a live account: whether large uploads are throttled or refused
// without the locateupload step.
const DefaultUploadURL = "https://d.pcs.baidu.com"

// DefaultOAuthURL hosts the token refresh endpoint.
const DefaultOAuthURL = "https://openapi.baidu.com"

// DownloadUserAgent is mandatory on every dlink request: Baidu's anti-leech
// check (errno 31326) rejects downloads — in practice anything over ~20 MB —
// unless the request presents this exact User-Agent.
const DownloadUserAgent = "pan.baidu.com"

// BlockSize is the block size Baidu requires for non-VIP accounts. VIP tiers
// allow 16 MiB and SVIP 32 MiB, but a wrong block size fails the create step,
// so the driver stays on the value every account accepts.
const BlockSize = 4 << 20

// SliceMD5Bytes is the prefix Baidu hashes as slice-md5 for the rapid-upload
// check.
const SliceMD5Bytes = 256 << 10

// RootID is the id of the drive root.
const RootID = "/"

// API paths.
const (
	pathFile       = "/rest/2.0/xpan/file"
	pathMultimedia = "/rest/2.0/xpan/multimedia"
	pathSuperfile2 = "/rest/2.0/pcs/superfile2"
	pathOAuthToken = "/oauth/2.0/token"
)

// Options configures a Provider.
type Options struct {
	// Name is the remote name used in metadata and cache keys.
	Name string
	// Client is the shared HTTP client. Required. Its UserAgent should be
	// DownloadUserAgent; Factory sets that up.
	Client *httpx.Client
	// BaseURL overrides DefaultBaseURL (tests point it at httptest).
	BaseURL string
	// UploadURL overrides DefaultUploadURL. Tests set it to the same
	// httptest server as BaseURL.
	UploadURL string
	// OAuthURL overrides DefaultOAuthURL.
	OAuthURL string
	// AccessToken authenticates every call as a query parameter. Required
	// unless RefreshToken is set.
	AccessToken string
	// RefreshToken, ClientID and ClientSecret enable transparent token
	// refresh; without them an expired token surfaces as provider.ErrAuth.
	RefreshToken           string
	ClientID, ClientSecret string
	// RootPath is the directory treated as the root (default "/").
	RootPath string
	// LinkTTL is how long a dlink is assumed valid (Baidu documents 8 h).
	LinkTTL time.Duration
	// PageSize is the list page size (default 1000, Baidu's maximum).
	PageSize int
	// Now is injectable for tests.
	Now func() time.Time
}

// Provider is the Baidu NetDisk backend.
type Provider struct {
	provider.TokenPersistence
	refreshMu sync.Mutex
	name      string
	base      string
	uploadURL string
	oauthURL  string
	client    *httpx.Client
	caps      provider.Caps
	root      string
	pageSize  int
	now       func() time.Time

	clientID     string
	clientSecret string

	mu           sync.Mutex
	accessToken  string
	refreshToken string
	expiry       time.Time

	// links caches dlinks until shortly before they expire. The block cache
	// asks for many ranges of one file in a row and filemetas competes with
	// every other metadata call for a bucket that only allows a couple of
	// requests per second.
	linkMu sync.Mutex
	links  map[string]provider.Link
}

// New builds a Provider from explicit options.
func New(opt Options) (*Provider, error) {
	if opt.Client == nil {
		return nil, errors.New("baidu: Client is required")
	}
	if opt.AccessToken == "" && opt.RefreshToken == "" {
		return nil, errors.New("baidu: remote is missing the required 'access_token' key")
	}
	ttl := opt.LinkTTL
	if ttl <= 0 {
		ttl = 8 * time.Hour
	}
	page := opt.PageSize
	if page <= 0 {
		page = 1000
	}
	root := opt.RootPath
	if root == "" {
		root = RootID
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	p := &Provider{
		name:         opt.Name,
		base:         strings.TrimSuffix(orDefault(opt.BaseURL, DefaultBaseURL), "/"),
		uploadURL:    strings.TrimSuffix(orDefault(opt.UploadURL, DefaultUploadURL), "/"),
		oauthURL:     strings.TrimSuffix(orDefault(opt.OAuthURL, DefaultOAuthURL), "/"),
		client:       opt.Client,
		root:         cleanPath(root),
		pageSize:     page,
		now:          now,
		clientID:     opt.ClientID,
		clientSecret: opt.ClientSecret,
		accessToken:  opt.AccessToken,
		refreshToken: opt.RefreshToken,
		links:        map[string]provider.Link{},
		caps: provider.Caps{
			HashTypes:   []provider.HashType{provider.HashMD5},
			RapidUpload: []provider.HashType{provider.HashMD5, provider.HashSliceMD5},
			RangeRead:   true,
			PartSize:    BlockSize,
			// Baidu documents a 1024-block ceiling (both reference drivers
			// observe 2048 in practice). At 4 MiB per block the documented
			// limit is 4 GiB, which is exactly the single-file limit for a
			// non-VIP account, so the conservative number is also the correct
			// one.
			MaxParts: 1024,
			// A block must finish inside 30 s or the server drops it, so the
			// driver keeps concurrency low rather than splitting bandwidth
			// across many slow blocks.
			UploadParallel: 2,
			ServerMove:     true,
			ServerRename:   true,
			ServerCopy:     true,
			Delta:          false,
			LinkTTL:        ttl,
			// Passed to whoever follows the link: without this exact UA Baidu
			// answers 403 for anything over ~20 MB.
			LinkHeaders:   map[string]string{"User-Agent": DownloadUserAgent},
			LinkShareable: true,
			// Non-SVIP accounts are throttled hard; start conservative and let
			// AIMD find the ceiling.
			QPS:             provider.QPS{Meta: 2, Download: 2, Upload: 1},
			MaxConnsPerHost: 4,
			Tier:            provider.TierOfficial,
		},
	}
	return p, nil
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

// RootPath returns the path the driver treats as the root.
func (p *Provider) RootPath() string { return p.root }

// -------------------------------------------------------------- file ids ---

// FileID is Baidu's split identity. The management endpoints
// (filemanager, precreate, create) address files by path, while filemetas and
// the download link need the numeric fs_id, and no endpoint translates one
// into the other cheaply. Entry.ID therefore encodes both as "<fs_id>:<path>";
// a bare path (always starting with "/") is accepted for ids the caller
// composed by hand.
type FileID struct {
	FSID uint64
	Path string
}

// ParseID decodes an Entry.ID.
func ParseID(id string) FileID {
	if id == "" {
		return FileID{Path: "/"}
	}
	if strings.HasPrefix(id, "/") {
		return FileID{Path: cleanPath(id)}
	}
	if i := strings.IndexByte(id, ':'); i >= 0 {
		n, err := strconv.ParseUint(id[:i], 10, 64)
		if err == nil {
			return FileID{FSID: n, Path: cleanPath(id[i+1:])}
		}
	}
	return FileID{Path: cleanPath("/" + id)}
}

// String encodes a FileID as an Entry.ID.
func (f FileID) String() string {
	if f.FSID == 0 {
		return f.Path
	}
	return strconv.FormatUint(f.FSID, 10) + ":" + f.Path
}

func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// ---------------------------------------------------------------- errors ---

// APIError is the errno envelope every xpan endpoint returns, including on
// HTTP 200: Baidu signals failure in the body, not in the status line.
type APIError struct {
	Errno   int    `json:"errno"`
	Errmsg  string `json:"errmsg"`
	Op      string `json:"-"`
	Request string `json:"request_id"`
}

// Error implements error.
func (e *APIError) Error() string {
	msg := e.Errmsg
	if msg == "" {
		msg = errnoText(e.Errno)
	}
	return fmt.Sprintf("baidu: %s failed: errno %d (%s)", e.Op, e.Errno, msg)
}

// HTTPStatus lets retry.Classify treat the errnos that no amount of retrying
// can fix as terminal. Baidu answers HTTP 200 even for them, so without this
// hint the classifier's default — retry — would keep hammering a full drive or
// an illegal path. Zero means "no opinion", which leaves the default in place.
func (e *APIError) HTTPStatus() int {
	switch e.Errno {
	case -7, -10, 31062, 31064, 31299, 31363, 31365:
		// no permission / quota full / illegal name / illegal path /
		// undersized first block / missing block / file too large.
		return http.StatusBadRequest
	}
	return 0
}

// Unwrap maps Baidu's errno onto the provider sentinels. Baidu answers HTTP
// 200 for almost every failure, so this table — not the status line — is what
// drives retry classification.
func (e *APIError) Unwrap() error {
	switch e.Errno {
	case -6, 6, 111, 31024, 31045, 20016, 20017:
		// -6 identity verification failed, 6 app not allowed to touch user
		// data, 31024 no permission, 31045 / 20016 / 20017 access token
		// expired, revoked or invalid.
		//
		// 111 is ambiguous: the错误码 table calls it "有其他异步任务正在执行"
		// while every field report (and both reference drivers) treat it as an
		// expired token. Treating it as auth costs one refresh and one retry
		// and recovers the common case; a genuinely busy account then fails
		// again with the same errno.
		return provider.ErrAuth
	case -8, 10, 31061:
		return provider.ErrExists
	case -3, -9, 31066:
		return provider.ErrNotFound
	case 31190, 31355:
		// "文件不存在" / "参数异常" during an upload means the uploadid the
		// session carries is gone or its block list no longer matches.
		// Reporting ErrNotFound makes the upload layer drop the session and
		// start a fresh precreate instead of retrying forever.
		return provider.ErrNotFound
	case 31034, 20012, 9013, 255:
		// 31034 命中接口频控, 20012 调用次数超限, 9013 hit frequence control,
		// 255 转存数量太多. All are throttles that clear on their own, so they
		// feed the AIMD limiter rather than tripping the breaker.
		return provider.ErrRateLimited
	case 31326:
		// 命中防盗链: the request reached the CDN without the mandatory
		// pan.baidu.com User-Agent, or the account is being anti-leech
		// filtered. Repeat offences get an account banned, so this trips the
		// circuit breaker.
		return provider.ErrRiskControl
	case 31360:
		return provider.ErrLinkExpired
	case 2, 31021, 42212, 42905:
		return provider.ErrTransient
	}
	return nil
}

func errnoText(n int) string {
	switch n {
	case -3, -9:
		return "文件或目录不存在"
	case -6:
		return "身份验证失败: access_token 无效或已过期"
	case -7:
		return "文件或目录名错误或无权访问"
	case -8:
		return "文件或目录已存在"
	case -10:
		return "云端容量已满"
	case 2:
		return "参数错误或服务端故障"
	case 6:
		return "不允许接入用户数据, 10 分钟后重新授权"
	case 111:
		return "access_token 无效, 或有其他异步任务正在执行"
	case 255:
		return "转存数量太多"
	case 9013:
		return "命中上传频控或黑名单"
	case 20012:
		return "调用次数已达上限, 触发限流"
	case 20016, 20017:
		return "access_token 已过期或已失效"
	case 31024:
		return "没有访问权限"
	case 31034:
		return "命中接口频控"
	case 31045:
		return "access_token 验证失败"
	case 31061:
		return "文件已存在"
	case 31062:
		return "文件名无效"
	case 31064:
		return "上传路径错误, 必须位于 /apps/<appname> 之下"
	case 31066:
		return "文件不存在"
	case 31190:
		return "分片缺失或 block_list 与已上传分片不一致"
	case 31299:
		return "第一个分片小于 4MB"
	case 31326:
		return "命中防盗链, 检查 User-Agent"
	case 31355:
		return "参数异常, 通常是 uploadid 失效"
	case 31360:
		return "下载链接已过期"
	case 31363:
		return "分片缺失"
	case 31365:
		return "文件总大小超限"
	}
	return "see https://pan.baidu.com/union/doc/ for errno meanings"
}

// check turns a non-zero errno into an APIError.
func check(op string, errno int, errmsg string) error {
	if errno == 0 {
		return nil
	}
	return &APIError{Errno: errno, Errmsg: errmsg, Op: op}
}

// ----------------------------------------------------------------- token ---

func (p *Provider) token(ctx context.Context) (string, error) {
	if err := p.FlushTokens(); err != nil {
		return "", fmt.Errorf("baidu: save credentials: %w", err)
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

// refresh exchanges the refresh token. Baidu's OAuth endpoint answers HTTP 200
// with an "error" field on failure, so the body is inspected either way.
func (p *Provider) refresh(ctx context.Context) (string, error) {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	if err := p.FlushTokens(); err != nil {
		return "", err
	}
	p.mu.Lock()
	rt, id, secret := p.refreshToken, p.clientID, p.clientSecret
	p.mu.Unlock()
	if rt == "" || id == "" || secret == "" {
		return "", fmt.Errorf("%w: baidu: access_token expired and no refresh_token/client_id/client_secret configured", provider.ErrAuth)
	}
	q := url.Values{}
	q.Set("grant_type", "refresh_token")
	q.Set("refresh_token", rt)
	q.Set("client_id", id)
	q.Set("client_secret", secret)
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	err := p.client.JSON(ctx, httpx.Request{
		Method: http.MethodGet,
		URL:    p.oauthURL + pathOAuthToken + "?" + q.Encode(),
		Class:  ratelimit.Meta,
	}, &out)
	if err != nil {
		return "", fmt.Errorf("%w: baidu: token refresh failed: %w", provider.ErrAuth, err)
	}
	if out.Error != "" || out.AccessToken == "" {
		return "", fmt.Errorf("%w: baidu: token refresh rejected: %s %s", provider.ErrAuth, out.Error, out.ErrorDesc)
	}
	p.mu.Lock()
	p.accessToken = out.AccessToken
	if out.RefreshToken != "" {
		p.refreshToken = out.RefreshToken
	}
	if out.ExpiresIn > 0 {
		p.expiry = p.now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	saveErr := p.SaveTokens(map[string]string{"refresh_token": out.RefreshToken})
	p.mu.Unlock()
	if saveErr != nil {
		return "", fmt.Errorf("baidu: save refreshed credentials: %w", saveErr)
	}
	return out.AccessToken, nil
}

// AccessToken returns the token currently in use. Tokens rotate on refresh, so
// the caller persists this to survive a restart.
func (p *Provider) AccessToken() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accessToken
}

// authURL appends the access token to a query. Baidu authenticates by query
// parameter, not by header.
func (p *Provider) authURL(ctx context.Context, base, path string, q url.Values) (string, error) {
	tok, err := p.token(ctx)
	if err != nil {
		return "", err
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("access_token", tok)
	return base + path + "?" + q.Encode(), nil
}

// get issues an authenticated GET and decodes out, retrying once after a token
// refresh when the server reports an auth errno.
func (p *Provider) get(ctx context.Context, op, base, apiPath string, q url.Values, class ratelimit.Class, out any) error {
	do := func() error {
		u, err := p.authURL(ctx, base, apiPath, q)
		if err != nil {
			return err
		}
		return p.client.JSON(ctx, httpx.Request{Method: http.MethodGet, URL: u, Class: class}, out)
	}
	return p.withRefresh(ctx, op, do, out)
}

// post issues an authenticated form POST and decodes out.
func (p *Provider) post(ctx context.Context, op, base, apiPath string, q url.Values, form map[string]string, class ratelimit.Class, out any) error {
	do := func() error {
		u, err := p.authURL(ctx, base, apiPath, q)
		if err != nil {
			return err
		}
		return p.client.JSON(ctx, httpx.Request{Method: http.MethodPost, URL: u, Class: class, Form: form}, out)
	}
	return p.withRefresh(ctx, op, do, out)
}

// errnoOf extracts the envelope errno from a decoded response.
type errnoCarrier interface{ errnoOf() (int, string) }

// withRefresh runs do, then inspects both the transport error and the decoded
// errno; an auth failure triggers exactly one refresh-and-retry.
func (p *Provider) withRefresh(ctx context.Context, op string, do func() error, out any) error {
	err := do()
	if err == nil {
		err = envelopeErr(op, out)
	}
	if err != nil && errors.Is(err, provider.ErrAuth) {
		if _, rerr := p.refresh(ctx); rerr != nil {
			// Keep the original failure in the chain: it names the errno the
			// server actually reported, which the refresh error does not.
			return fmt.Errorf("%w (token refresh failed: %v)", err, rerr)
		}
		if err = do(); err == nil {
			err = envelopeErr(op, out)
		}
	}
	return err
}

func envelopeErr(op string, out any) error {
	c, ok := out.(errnoCarrier)
	if !ok {
		return nil
	}
	errno, msg := c.errnoOf()
	return check(op, errno, msg)
}

// baseResp is embedded in every decoded response so envelopeErr can find the
// errno without a type switch per endpoint.
type baseResp struct {
	Errno  int    `json:"errno"`
	Errmsg string `json:"errmsg"`
}

func (b *baseResp) errnoOf() (int, string) { return b.Errno, b.Errmsg }

// ------------------------------------------------------------------ list ---

// fileItem is one entry of a list or filemetas response.
type fileItem struct {
	FSID           uint64 `json:"fs_id"`
	Path           string `json:"path"`
	ServerFilename string `json:"server_filename"`
	Filename       string `json:"filename"`
	IsDir          int    `json:"isdir"`
	Size           int64  `json:"size"`
	ServerMtime    int64  `json:"server_mtime"`
	LocalMtime     int64  `json:"local_mtime"`
	MD5            string `json:"md5"`
	Dlink          string `json:"dlink"`
}

func (it fileItem) entry(parent string) provider.Entry {
	name := it.ServerFilename
	if name == "" {
		name = it.Filename
	}
	if name == "" {
		name = path.Base(it.Path)
	}
	e := provider.Entry{
		ID:       FileID{FSID: it.FSID, Path: cleanPath(it.Path)}.String(),
		ParentID: parent,
		Name:     name,
		Kind:     provider.KindFile,
		Size:     it.Size,
	}
	if it.IsDir != 0 {
		e.Kind = provider.KindDir
		e.Size = 0
	}
	mt := it.ServerMtime
	if mt == 0 {
		mt = it.LocalMtime
	}
	if mt > 0 {
		e.ModTime = time.Unix(mt, 0).UTC()
	}
	if e.ParentID == "" {
		e.ParentID = cleanPath(path.Dir(cleanPath(it.Path)))
	}
	// Baidu's md5 field is empty for directories and, for very large files,
	// occasionally a non-hex "encrypted" form; only a plain 32-hex digest is
	// usable as a content hash.
	if md5hex := strings.ToLower(it.MD5); e.Kind == provider.KindFile && isHex32(md5hex) {
		e.Hashes = provider.Hashes{provider.HashMD5: md5hex}
		e.Version = md5hex
	} else {
		e.Version = fmt.Sprintf("%d-%d", it.Size, mt)
	}
	return e
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

type listResp struct {
	baseResp
	List []fileItem `json:"list"`
}

// List returns one page of a directory. The cursor is Baidu's numeric "start"
// offset: the API paginates by offset, not by token, and reports no
// "has_more", so a short page means the end.
func (p *Provider) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	dir := ParseID(dirID).Path
	if dirID == "" {
		dir = p.root
	}
	start := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("baidu: bad list cursor %q: %w", cursor, err)
		}
		start = n
	}
	q := url.Values{}
	q.Set("method", "list")
	q.Set("dir", dir)
	q.Set("start", strconv.Itoa(start))
	q.Set("limit", strconv.Itoa(p.pageSize))
	q.Set("web", "0")
	q.Set("order", "name")
	var out listResp
	if err := p.get(ctx, "list", p.base, pathFile, q, ratelimit.Meta, &out); err != nil {
		return nil, "", err
	}
	entries := make([]provider.Entry, 0, len(out.List))
	for _, it := range out.List {
		entries = append(entries, it.entry(dirID))
	}
	next := ""
	if len(out.List) == p.pageSize {
		next = strconv.Itoa(start + len(out.List))
	}
	return entries, next, nil
}

type metasResp struct {
	baseResp
	List []fileItem `json:"list"`
}

// filemetas fetches metadata (and optionally the download link) by fs_id.
func (p *Provider) filemetas(ctx context.Context, fsid uint64, dlink bool) (fileItem, error) {
	q := url.Values{}
	q.Set("method", "filemetas")
	q.Set("fsids", "["+strconv.FormatUint(fsid, 10)+"]")
	if dlink {
		q.Set("dlink", "1")
	}
	var out metasResp
	if err := p.get(ctx, "filemetas", p.base, pathMultimedia, q, ratelimit.Meta, &out); err != nil {
		return fileItem{}, err
	}
	if len(out.List) == 0 {
		return fileItem{}, provider.ErrNotFound
	}
	return out.List[0], nil
}

// Stat returns one entry. With an fs_id it is a single filemetas call; for a
// bare path the parent directory has to be listed, because the open platform
// exposes no "stat this path" endpoint.
func (p *Provider) Stat(ctx context.Context, id string) (provider.Entry, error) {
	f := ParseID(id)
	if id == "" {
		f.Path = p.root
	}
	if f.FSID != 0 {
		it, err := p.filemetas(ctx, f.FSID, false)
		if err != nil {
			return provider.Entry{}, err
		}
		if it.Path == "" {
			it.Path = f.Path
		}
		return it.entry(""), nil
	}
	if f.Path == p.root {
		return provider.Entry{ID: p.root, Name: "", Kind: provider.KindDir, Version: p.root}, nil
	}
	parent := cleanPath(path.Dir(f.Path))
	name := path.Base(f.Path)
	cursor := ""
	for {
		entries, next, err := p.List(ctx, parent, cursor)
		if err != nil {
			return provider.Entry{}, err
		}
		for _, e := range entries {
			if e.Name == name {
				return e, nil
			}
		}
		if next == "" {
			return provider.Entry{}, fmt.Errorf("%w: baidu: %s", provider.ErrNotFound, f.Path)
		}
		cursor = next
	}
}

// DownloadURL returns a dlink. The access token has to be appended to the link
// itself — the CDN authenticates the query string, not a header — and the
// caller must send Link.Headers (User-Agent: pan.baidu.com) or Baidu answers
// 403 for anything over ~20 MB.
func (p *Provider) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	p.linkMu.Lock()
	if l, ok := p.links[id]; ok && p.now().Add(time.Minute).Before(l.ExpiresAt) {
		p.linkMu.Unlock()
		return l, nil
	}
	p.linkMu.Unlock()

	f := ParseID(id)
	if f.FSID == 0 {
		e, err := p.Stat(ctx, id)
		if err != nil {
			return provider.Link{}, err
		}
		f = ParseID(e.ID)
		if f.FSID == 0 {
			return provider.Link{}, fmt.Errorf("%w: baidu: no fs_id for %s", provider.ErrNotFound, id)
		}
	}
	it, err := p.filemetas(ctx, f.FSID, true)
	if err != nil {
		return provider.Link{}, err
	}
	if it.Dlink == "" {
		return provider.Link{}, fmt.Errorf("%w: baidu: filemetas returned no dlink for %s", provider.ErrNotFound, id)
	}
	tok, err := p.token(ctx)
	if err != nil {
		return provider.Link{}, err
	}
	sep := "?"
	if strings.Contains(it.Dlink, "?") {
		sep = "&"
	}
	link := provider.Link{
		URL:       it.Dlink + sep + "access_token=" + url.QueryEscape(tok),
		ExpiresAt: p.now().Add(p.caps.LinkTTL),
		Headers:   map[string]string{"User-Agent": DownloadUserAgent},
	}
	p.linkMu.Lock()
	p.links[id] = link
	p.linkMu.Unlock()
	return link, nil
}

// ReadRange fetches a byte range from the dlink host. A dlink that expired
// early (errno 31360 surfaces at the CDN as a 403) is refreshed once.
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
		fresh, err2 := p.DownloadURL(ctx, id)
		if err2 != nil {
			return nil, err2
		}
		return p.getRange(ctx, fresh, off, n)
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
		Method: http.MethodGet,
		URL:    link.URL,
		Class:  ratelimit.Download,
		Header: h,
		// The dlink answers 302 to a pcs host; following it keeps the Range
		// and UA headers, which Go's client does for us.
		ExpectStatus: []int{http.StatusPartialContent, http.StatusOK},
		Stream:       true,
	})
	if err != nil {
		return nil, err
	}
	if resp.Status == http.StatusOK && off > 0 {
		if _, err := io.CopyN(io.Discard, resp.Body, off); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("baidu: server ignored Range and the file is shorter than offset %d: %w", off, err)
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

type precreateResp struct {
	baseResp
	Path       string   `json:"path"`
	UploadID   string   `json:"uploadid"`
	ReturnType int      `json:"return_type"`
	BlockList  []int    `json:"block_list"`
	Info       fileItem `json:"info"`
}

// createResp is the response of both mkdir and the final upload create.
type createResp struct {
	baseResp
	FSID           uint64 `json:"fs_id"`
	Path           string `json:"path"`
	ServerFilename string `json:"server_filename"`
	Size           int64  `json:"size"`
	IsDir          int    `json:"isdir"`
	Ctime          int64  `json:"ctime"`
	Mtime          int64  `json:"mtime"`
	MD5            string `json:"md5"`
}

func (c createResp) item() fileItem {
	return fileItem{
		FSID: c.FSID, Path: c.Path, ServerFilename: c.ServerFilename,
		IsDir: c.IsDir, Size: c.Size, ServerMtime: c.Mtime, MD5: c.MD5,
	}
}

// BeginUpload runs precreate. When Baidu already holds the content it answers
// return_type 2 with the finished entry (秒传) and no block ever moves.
//
// UNVERIFIED: precreate documents block_list as the MD5 of every 4 MiB block,
// but the Provider interface hands BeginUpload only whole-file hashes, so the
// per-block digests do not exist yet. For a single-block file the content MD5
// is the block list and is sent as-is; for larger files the driver sends a
// placeholder list of the right length and CompleteUpload sends the real
// digests, collected from the superfile2 responses, to create. Needs checking
// against a live account: whether precreate validates the placeholder list,
// and whether the rapid check still fires for multi-block files. Either way no
// data is at risk — a rejected precreate or a missed rapid hit only costs a
// full chunked upload.
func (p *Provider) BeginUpload(ctx context.Context, parentID, name string, size int64, h provider.Hashes) (provider.UploadSession, error) {
	parent := ParseID(parentID).Path
	if parentID == "" {
		parent = p.root
	}
	target := cleanPath(path.Join(parent, name))
	blocks := int((size + BlockSize - 1) / BlockSize)
	if blocks == 0 {
		blocks = 1
	}

	contentMD5 := strings.ToLower(h[provider.HashMD5])
	sliceMD5 := strings.ToLower(h[provider.HashSliceMD5])

	blockList := make([]string, blocks)
	for i := range blockList {
		blockList[i] = placeholderBlockMD5
	}
	if blocks == 1 && isHex32(contentMD5) {
		blockList[0] = contentMD5
	}
	bl, err := json.Marshal(blockList)
	if err != nil {
		return provider.UploadSession{}, err
	}

	form := map[string]string{
		"path":       target,
		"size":       strconv.FormatInt(size, 10),
		"isdir":      "0",
		"autoinit":   "1",
		"block_list": string(bl),
		// rtype 3 overwrites a same-named file. The uploader has already
		// compared versions and picked a conflict name when needed, so an
		// overwrite here is the intended result, not a lost update.
		"rtype": "3",
	}
	if isHex32(contentMD5) {
		form["content-md5"] = contentMD5
	}
	if isHex32(sliceMD5) {
		form["slice-md5"] = sliceMD5
	}

	q := url.Values{}
	q.Set("method", "precreate")
	var out precreateResp
	if err := p.post(ctx, "precreate", p.base, pathFile, q, form, ratelimit.Upload, &out); err != nil {
		return provider.UploadSession{}, err
	}

	s := provider.UploadSession{
		ID:       out.UploadID,
		PartSize: BlockSize,
		Opaque: map[string]string{
			"upload_id": out.UploadID,
			"path":      target,
			"size":      strconv.FormatInt(size, 10),
			"parent":    parent,
		},
	}
	if isHex32(contentMD5) {
		s.Opaque["content_md5"] = contentMD5
	}
	// return_type 2 means the server matched the content hashes and the file
	// already exists in the account: the upload is finished.
	if out.ReturnType == RapidReturnType || out.Info.FSID != 0 {
		it := out.Info
		if it.Path == "" {
			it.Path = target
		}
		if it.Size == 0 {
			it.Size = size
		}
		if it.MD5 == "" {
			it.MD5 = contentMD5
		}
		e := it.entry(parent)
		if e.Name == "" {
			e.Name = name
		}
		s.RapidDone = true
		s.Entry = &e
		return s, nil
	}
	if out.UploadID == "" {
		return provider.UploadSession{}, fmt.Errorf("baidu: precreate returned no uploadid for %s", target)
	}
	return s, nil
}

// RapidReturnType is the precreate return_type that means "the content was
// already on the server and the upload is done".
const RapidReturnType = 2

// placeholderBlockMD5 fills block_list slots whose real digest is not known
// until the block streams through UploadPart. See BeginUpload's UNVERIFIED
// note.
const placeholderBlockMD5 = "00000000000000000000000000000000"

// UploadPart sends one 4 MiB block to superfile2. The returned ETag is the
// block's MD5 as the server computed it, which CompleteUpload replays as
// block_list — so a corrupted block is caught by create, not by the user.
//
// The block is buffered in memory (at most PartSize) because the multipart
// body needs a known length: superfile2 rejects chunked transfer encoding, and
// buffering also makes the request safely retryable.
func (p *Provider) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	buf := &bytes.Buffer{}
	body := multipart.NewWriter(buf)
	fw, err := body.CreateFormFile("file", "blob")
	if err != nil {
		return provider.PartToken{}, err
	}
	copied, err := io.Copy(fw, io.LimitReader(r, n))
	if err != nil {
		return provider.PartToken{}, err
	}
	// A short read means the staged blob is smaller than the journal believes.
	// Uploading it anyway would commit a truncated file that looks successful,
	// so fail here and let the upload be retried or dead-lettered.
	if copied != n {
		return provider.PartToken{}, fmt.Errorf("baidu: part %d is short: read %d of %d bytes", idx, copied, n)
	}
	if err := body.Close(); err != nil {
		return provider.PartToken{}, err
	}

	q := url.Values{}
	q.Set("method", "upload")
	q.Set("type", "tmpfile")
	q.Set("path", s.Opaque["path"])
	q.Set("uploadid", pick(s.Opaque["upload_id"], s.ID))
	q.Set("partseq", strconv.Itoa(idx))

	var out struct {
		baseResp
		MD5 string `json:"md5"`
	}
	payload := buf.Bytes()
	do := func() error {
		u, err := p.authURL(ctx, p.uploadURL, pathSuperfile2, q)
		if err != nil {
			return err
		}
		hdr := http.Header{}
		hdr.Set("Content-Type", body.FormDataContentType())
		return p.client.JSON(ctx, httpx.Request{
			Method: http.MethodPost,
			URL:    u,
			Class:  ratelimit.Upload,
			Header: hdr,
			Body:   bytes.NewReader(payload),
			GetBody: func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(payload)), nil
			},
		}, &out)
	}
	if err := p.withRefresh(ctx, "superfile2", do, &out); err != nil {
		return provider.PartToken{}, err
	}
	if out.MD5 == "" {
		return provider.PartToken{}, fmt.Errorf("baidu: superfile2 returned no md5 for block %d of %s", idx, s.Opaque["path"])
	}
	return provider.PartToken{Index: idx, ETag: strings.ToLower(out.MD5)}, nil
}

// CompleteUpload calls create with the authoritative block list.
func (p *Provider) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	sorted := append([]provider.PartToken(nil), parts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Index < sorted[j].Index })
	blocks := make([]string, 0, len(sorted))
	for _, pt := range sorted {
		blocks = append(blocks, pt.ETag)
	}
	if len(blocks) == 0 {
		// A zero-length file still needs one block entry; the MD5 of nothing.
		blocks = append(blocks, pick(s.Opaque["content_md5"], emptyMD5))
	}
	bl, err := json.Marshal(blocks)
	if err != nil {
		return provider.Entry{}, err
	}
	target := s.Opaque["path"]
	form := map[string]string{
		"path":       target,
		"size":       s.Opaque["size"],
		"isdir":      "0",
		"block_list": string(bl),
		"uploadid":   pick(s.Opaque["upload_id"], s.ID),
		"rtype":      "3",
	}
	q := url.Values{}
	q.Set("method", "create")
	var out createResp
	if err := p.post(ctx, "create", p.base, pathFile, q, form, ratelimit.Upload, &out); err != nil {
		return provider.Entry{}, err
	}
	it := out.item()
	if it.Path == "" {
		it.Path = target
	}
	e := it.entry(s.Opaque["parent"])
	if e.Name == "" {
		e.Name = path.Base(target)
	}
	return e, nil
}

// emptyMD5 is the MD5 of zero bytes.
const emptyMD5 = "d41d8cd98f00b204e9800998ecf8427e"

func pick(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ------------------------------------------------------------- namespace ---

// Mkdir creates a directory. Baidu reuses the create endpoint with isdir=1.
func (p *Provider) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	parent := ParseID(parentID).Path
	if parentID == "" {
		parent = p.root
	}
	target := cleanPath(path.Join(parent, name))
	q := url.Values{}
	q.Set("method", "create")
	var out createResp
	err := p.post(ctx, "mkdir", p.base, pathFile, q, map[string]string{
		"path":  target,
		"isdir": "1",
		// rtype 0 refuses to touch an existing directory, which surfaces as
		// errno -8 → provider.ErrExists instead of a silent rename.
		"rtype": "0",
	}, ratelimit.Meta, &out)
	if err != nil {
		return provider.Entry{}, err
	}
	it := out.item()
	if it.Path == "" {
		it.Path = target
	}
	it.IsDir = 1
	e := it.entry(parent)
	if e.Name == "" {
		e.Name = name
	}
	return e, nil
}

// manageResp is the filemanager envelope. A per-entry errno can be non-zero
// while the top-level one is 0, so both are checked.
type manageResp struct {
	baseResp
	TaskID int64 `json:"taskid"`
	Info   []struct {
		Errno int    `json:"errno"`
		Path  string `json:"path"`
	} `json:"info"`
}

// filemanager runs one copy/move/rename/delete operation synchronously
// (async=0), so the caller never has to poll a taskid.
func (p *Provider) filemanager(ctx context.Context, opera string, filelist any) error {
	fl, err := json.Marshal(filelist)
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("method", "filemanager")
	q.Set("opera", opera)
	var out manageResp
	if err := p.post(ctx, opera, p.base, pathFile, q, map[string]string{
		// async 0 makes the server finish before answering, so the caller sees
		// a consistent tree; the alternative would need task polling.
		"async":    "0",
		"filelist": string(fl),
		// fail rather than overwrite: a name collision has to reach the VFS as
		// provider.ErrExists so it can write a conflict copy, not silently
		// destroy the other file.
		"ondup": "fail",
	}, ratelimit.Meta, &out); err != nil {
		return err
	}
	for _, i := range out.Info {
		if err := check(opera, i.Errno, ""); err != nil {
			return err
		}
	}
	return nil
}

// Rename renames an entry in place.
func (p *Provider) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	f := ParseID(id)
	if err := p.filemanager(ctx, "rename", []map[string]string{
		{"path": f.Path, "newname": newName},
	}); err != nil {
		return provider.Entry{}, err
	}
	target := cleanPath(path.Join(path.Dir(f.Path), newName))
	return p.Stat(ctx, FileID{FSID: f.FSID, Path: target}.String())
}

// Move relocates an entry into another directory, keeping its name.
func (p *Provider) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	f := ParseID(id)
	dest := ParseID(newParentID).Path
	name := path.Base(f.Path)
	if err := p.filemanager(ctx, "move", []map[string]string{
		{"path": f.Path, "dest": dest, "newname": name},
	}); err != nil {
		return provider.Entry{}, err
	}
	return p.Stat(ctx, FileID{FSID: f.FSID, Path: cleanPath(path.Join(dest, name))}.String())
}

// Copy duplicates an entry server-side, implementing provider.ServerCopier.
func (p *Provider) Copy(ctx context.Context, id, newParentID, newName string) (provider.Entry, error) {
	f := ParseID(id)
	dest := ParseID(newParentID).Path
	if newName == "" {
		newName = path.Base(f.Path)
	}
	if err := p.filemanager(ctx, "copy", []map[string]string{
		{"path": f.Path, "dest": dest, "newname": newName},
	}); err != nil {
		return provider.Entry{}, err
	}
	return p.Stat(ctx, cleanPath(path.Join(dest, newName)))
}

// Delete moves an entry to the recycle bin, where Baidu keeps it for ten days.
func (p *Provider) Delete(ctx context.Context, id string) error {
	f := ParseID(id)
	return p.filemanager(ctx, "delete", []string{f.Path})
}

var (
	_ provider.Provider     = (*Provider)(nil)
	_ provider.ServerCopier = (*Provider)(nil)
	_ provider.Transporter  = (*Provider)(nil)
)

// Factory builds a Baidu provider from its config block.
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
	if get("access_token") == "" && get("refresh_token") == "" {
		return nil, fmt.Errorf("baidu: remote %q is missing the required 'access_token' key", name)
	}
	// Every request, not just downloads, carries the pan.baidu.com UA: the
	// >20 MB download rule is enforced on the CDN and a mismatch between API
	// and CDN user agents has no upside.
	// Prefer the daemon's client: it already applies the proxy rules, the rate
	// limiter and the circuit breaker for this remote. It carries no
	// User-Agent of its own, so the mandatory one is put on it here.
	client, shared := httpx.FromConfig(cfg, httpx.Options{
		Remote: name, Account: get("client_id"), UserAgent: DownloadUserAgent,
	})
	if shared && client.UserAgent == "" {
		client.UserAgent = DownloadUserAgent
	}
	return New(Options{
		Name:         name,
		Client:       client,
		BaseURL:      get("base_url"),
		UploadURL:    get("upload_url"),
		OAuthURL:     get("oauth_url"),
		AccessToken:  get("access_token"),
		RefreshToken: get("refresh_token"),
		ClientID:     get("client_id"),
		ClientSecret: get("client_secret"),
		RootPath:     get("root_id"),
	})
}

// SetTransport lets the daemon replace the HTTP client after construction.
func (p *Provider) SetTransport(client any) {
	if c, ok := client.(*httpx.Client); ok && c != nil {
		if c.UserAgent == "" {
			c.UserAgent = DownloadUserAgent
		}
		p.client = c
	}
}

func init() {
	provider.Register("baidu", Factory)
	provider.RegisterFields("baidu", []provider.Field{
		{Name: "client_id", Prompt: "开放平台 AppKey"},
		{Name: "root_id", Prompt: "作为根的目录", Default: "/"},
	}, provider.Credentials{Fields: []string{"refresh_token", "client_secret", "access_token"},
		Note: "config auth 会打开浏览器完成授权"})

}
