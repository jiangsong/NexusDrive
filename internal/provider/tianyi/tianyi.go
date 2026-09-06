// Package tianyi implements the 天翼云盘 (Tianyi Cloud, cloud.189.cn) backend.
//
// 189 has no public open platform for the personal cloud, so this driver
// speaks the PC client protocol that public documentation and community
// clients describe. Four properties of that protocol shape the code here:
//
//   - Every api.cloud.189.cn call is signed. The signature is an HMAC-SHA1
//     over a canonical string built from the session key, the HTTP method, the
//     request path and the Date header, keyed by the session secret. The Date
//     put on the wire and the Date fed to the signer must be the same string,
//     so both come from one place (see signedHeader).
//   - Parts of the API answer in XML rather than JSON. getSessionForPC.action
//     returns a <userSession> document and the upload host
//     (upload.cloud.189.cn/person/*) answers in XML unless a JSON Accept
//     header is negotiated. Those call sites use httpx.Client.XML; every other
//     call site uses httpx.Client.JSON.
//   - Sessions expire and the server says so with a res_code rather than a
//     401, so the error mapper has to look inside 200 responses.
//   - There is no delta / change feed of any kind, so the driver deliberately
//     does not implement provider.ChangeLister and reports Caps.Delta = false.
//
// The driver never hardcodes a host on a request path: all four base URLs are
// injectable through Options so a test can point the whole driver at a single
// httptest.Server.
package tianyi

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// Default endpoints. Each is overridable through Options so tests (and users
// behind a mirror) can redirect the driver.
const (
	// DefaultBaseURL serves the signed file APIs (/open/file/*, /open/batch/*).
	DefaultBaseURL = "https://api.cloud.189.cn"
	// DefaultAuthURL serves the login box (/api/logbox/*) and
	// getSessionForPC.action.
	DefaultAuthURL = "https://open.e.189.cn"
	// DefaultWebURL serves the PC login page whose inline JavaScript carries
	// the one-shot login parameters (lt, reqId, paramId).
	DefaultWebURL = "https://cloud.189.cn"
	// DefaultUploadURL serves the /person/* upload endpoints, which answer in
	// XML.
	DefaultUploadURL = "https://upload.cloud.189.cn"
)

// DefaultRootID is the id of the personal cloud root folder. 189 uses the
// sentinel "-11" rather than a real folder id.
//
// UNVERIFIED: "-11" is what community clients use for the personal (non
// family) root; confirm against a live account before relying on it for a
// family cloud, which uses a different root.
const DefaultRootID = "-11"

// DefaultPartSize is the multipart slice size. It is also the value sent as
// sliceSize on initMultiUpload, so changing it changes the server-side slice
// layout and must not differ between BeginUpload and UploadPart.
const DefaultPartSize int64 = 8 << 20

// DefaultUserAgent is sent on every request. 189 edge nodes reject requests
// with no or an obviously scripted User-Agent, so the driver always presents a
// desktop browser string.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36"

// LinkTTL is how long a resolved download URL is treated as usable.
//
// UNVERIFIED: 189 does not publish the lifetime of the signed URL returned by
// getFileDownloadUrl.action; community clients observe "tens of minutes". The
// driver assumes 30 minutes and additionally re-resolves on the first 403/410,
// so an over-long guess costs one retry rather than a failed read.
const LinkTTL = 30 * time.Minute

// listPageSize is the page size asked of listFiles.action. It doubles as the
// "was this the last page?" test, so it must match the pageSize actually sent.
const listPageSize = 100

// PC client identifiers sent on the login and session calls.
//
// UNVERIFIED: appID/clientType/appVersion/channelID are the values community
// clients send for the desktop client. They are accepted today but 189 may
// pin them to real client releases.
const (
	appID      = "8025431004"
	clientType = "TELEPC"
	appVersion = "6.2"
	channelID  = "web_cloud.189.cn"
	returnURL  = "https://m.cloud.189.cn/zhuanti/2020/loginErrorPc/index.html"
	accountTyp = "01"
)

// Options configures a Tianyi driver directly, bypassing the config map. Every
// endpoint is injectable so a test can point the driver at one httptest.Server.
type Options struct {
	// Client is the shared HTTP client. It must be non-nil in tests; the
	// registry factory builds a default one when it is nil.
	Client *httpx.Client
	// BaseURL, AuthURL, WebURL and UploadURL default to the Default* constants
	// when empty.
	BaseURL   string
	AuthURL   string
	WebURL    string
	UploadURL string
	// Username and Password drive the RSA login-box flow. AccessToken skips it.
	Username    string
	Password    string
	AccessToken string
	// RootID defaults to DefaultRootID.
	RootID string
	// UserAgent defaults to DefaultUserAgent. It is only applied when this
	// driver builds its own client.
	UserAgent string
}

// Tianyi is the 天翼云盘 provider.
type Tianyi struct {
	name   string
	cli    *httpx.Client
	caps   provider.Caps
	rootID string

	base   string
	auth   string
	web    string
	upload string

	username    string
	password    string
	accessToken string

	// pollInterval and pollAttempts bound the wait for an asynchronous batch
	// task (move / delete). They are fields rather than constants so tests do
	// not have to sleep.
	pollInterval time.Duration
	pollAttempts int

	// authMu serialises login so concurrent callers cannot stampede the login
	// box, which is itself rate limited and captcha-guarded.
	authMu sync.Mutex
	sess   session

	// mu guards the caches below.
	mu    sync.Mutex
	links map[string]cachedLink
	// kinds remembers whether an id is a file or a folder. 189 has separate
	// rename endpoints for the two, and the Provider interface only hands us
	// an id, so a cached kind saves a Stat on every rename.
	kinds map[string]provider.Kind
}

var (
	_ provider.Provider    = (*Tianyi)(nil)
	_ provider.Transporter = (*Tianyi)(nil)
)

// NewWithOptions builds a driver from explicit options. It is the constructor
// tests use; New adapts the config map onto it.
func NewWithOptions(name string, opt Options) (*Tianyi, error) {
	if opt.AccessToken == "" {
		switch {
		case opt.Username == "" && opt.Password == "":
			return nil, fmt.Errorf(`tianyi: config needs either "access_token" or both "username" and "password"`)
		case opt.Username == "":
			return nil, fmt.Errorf(`tianyi: config key "username" is required alongside "password"`)
		case opt.Password == "":
			return nil, fmt.Errorf(`tianyi: config key "password" is required alongside "username"`)
		}
	}
	cli := opt.Client
	if cli == nil {
		ua := opt.UserAgent
		if ua == "" {
			ua = DefaultUserAgent
		}
		cli = httpx.New(httpx.Options{Remote: name, Account: opt.Username, UserAgent: ua})
	}
	t := &Tianyi{
		name:         name,
		cli:          cli,
		rootID:       firstNonEmpty(opt.RootID, DefaultRootID),
		base:         strings.TrimRight(firstNonEmpty(opt.BaseURL, DefaultBaseURL), "/"),
		auth:         strings.TrimRight(firstNonEmpty(opt.AuthURL, DefaultAuthURL), "/"),
		web:          strings.TrimRight(firstNonEmpty(opt.WebURL, DefaultWebURL), "/"),
		upload:       strings.TrimRight(firstNonEmpty(opt.UploadURL, DefaultUploadURL), "/"),
		username:     opt.Username,
		password:     opt.Password,
		accessToken:  opt.AccessToken,
		links:        map[string]cachedLink{},
		kinds:        map[string]provider.Kind{},
		pollInterval: 300 * time.Millisecond,
		pollAttempts: 40,
	}
	t.caps = provider.Caps{
		// UNVERIFIED: the forbidden set is the Windows-like set most domestic
		// drives document; verify against the real API's error on each character
		// and on names ending in a dot or a space.
		Naming:      provider.Naming{MaxNameBytes: 255, ForbiddenRunes: "\\:*?\"<>|"},
		HashTypes:   []provider.HashType{provider.HashMD5},
		RapidUpload: []provider.HashType{provider.HashMD5},
		RangeRead:   true,

		PartSize: DefaultPartSize,
		// UNVERIFIED: 189 does not document a part-count ceiling. 10000 is the
		// OSS-style convention and is low enough to stay safe.
		MaxParts: 10000,
		// One part at a time: the upload host issues a per-part signed URL, and
		// pushing several in parallel is the fastest way to draw attention.
		UploadParallel: 1,

		ServerMove:   true,
		ServerRename: true,
		// UNVERIFIED: 189 does have a COPY batch task, but this driver does not
		// implement provider.ServerCopier, so the capability is reported false.
		ServerCopy: false,

		// No delta feed exists, so the driver does not implement ChangeLister.
		Delta: false,

		LinkTTL: LinkTTL,
		// The signed URL carries its own credentials, so any process can fetch
		// it; the driver still passes its User-Agent along because some edge
		// nodes reject unknown agents.
		LinkHeaders:   map[string]string{"User-Agent": firstNonEmpty(opt.UserAgent, DefaultUserAgent)},
		LinkShareable: true,

		// Meta 2/s, download 2/s, upload 1/s: 189 tolerates more than 115 does,
		// but the account is still a consumer account and sustained bursts draw
		// throttling, so the driver starts conservative and lets AIMD find the
		// ceiling.
		QPS:             provider.QPS{Meta: 2, Download: 2, Upload: 1},
		MaxConnsPerHost: 4,
		Tier:            provider.TierOfficial,
	}
	return t, nil
}

// New is the registry factory. It reads username/password/access_token/root_id
// (and the optional *_url and user_agent overrides) from the remote's config
// block.
func New(name string, cfg map[string]any) (*Tianyi, error) {
	get := func(key string) (string, error) { return cfgString(cfg, key) }
	username, err := get("username")
	if err != nil {
		return nil, err
	}
	password, err := get("password")
	if err != nil {
		return nil, err
	}
	token, err := get("access_token")
	if err != nil {
		return nil, err
	}
	rootID, err := get("root_id")
	if err != nil {
		return nil, err
	}
	base, err := get("base_url")
	if err != nil {
		return nil, err
	}
	auth, err := get("auth_url")
	if err != nil {
		return nil, err
	}
	web, err := get("web_url")
	if err != nil {
		return nil, err
	}
	upload, err := get("upload_url")
	if err != nil {
		return nil, err
	}
	ua, err := get("user_agent")
	if err != nil {
		return nil, err
	}
	// Prefer the daemon's client: it already applies the proxy rules, the rate
	// limiter and the circuit breaker for this remote. 189 edge nodes reject
	// requests with no User-Agent, so the agent is put on the shared client
	// when it carries none.
	agent := firstNonEmpty(ua, DefaultUserAgent)
	client, shared := httpx.FromConfig(cfg, httpx.Options{
		Remote: name, Account: username, UserAgent: agent,
	})
	if shared && client.UserAgent == "" {
		client.UserAgent = agent
	}
	return NewWithOptions(name, Options{
		Client:      client,
		BaseURL:     base,
		AuthURL:     auth,
		WebURL:      web,
		UploadURL:   upload,
		Username:    username,
		Password:    password,
		AccessToken: token,
		RootID:      rootID,
		UserAgent:   ua,
	})
}

// SetTransport lets the daemon replace the HTTP client after construction.
func (t *Tianyi) SetTransport(client any) {
	if c, ok := client.(*httpx.Client); ok && c != nil {
		if c.UserAgent == "" {
			c.UserAgent = firstNonEmpty(t.caps.LinkHeaders["User-Agent"], DefaultUserAgent)
		}
		t.cli = c
	}
}

func init() {
	provider.RegisterFields("tianyi", []provider.Field{
		{Name: "username", Prompt: "天翼云盘账号（手机号）"},
		{Name: "root_id", Prompt: "作为根的目录 id", Default: "-11"},
	}, provider.Credentials{Fields: []string{"password"}, Note: "账号密码"})
	provider.Register("tianyi", func(name string, cfg map[string]any) (provider.Provider, error) {
		t, err := New(name, cfg)
		if err != nil {
			// Return a nil interface, not a typed nil pointer.
			return nil, err
		}
		return t, nil
	})
}

// Name returns the remote name this driver was configured under.
func (t *Tianyi) Name() string { return t.name }

// Capabilities returns the capability matrix. The upper layers read it instead
// of special-casing the backend by name.
func (t *Tianyi) Capabilities() provider.Caps { return t.caps }

// RootID returns the folder id the driver treats as the root of the remote.
func (t *Tianyi) RootID() string { return t.rootID }

// cachedLink is a resolved download URL and the moment it stops being trusted.
type cachedLink struct {
	url     string
	expires time.Time
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// cfgString reads one config key as a string. YAML hands numbers through as
// int/float64, so ids written unquoted still work.
func cfgString(cfg map[string]any, key string) (string, error) {
	v, ok := cfg[key]
	if !ok || v == nil {
		return "", nil
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		if x == math.Trunc(x) {
			return strconv.FormatInt(int64(x), 10), nil
		}
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(x), nil
	default:
		return "", fmt.Errorf("tianyi: config key %q must be a string, got %T", key, v)
	}
}
