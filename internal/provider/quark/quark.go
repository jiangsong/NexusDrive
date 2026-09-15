// Package quark implements the 夸克网盘 (Quark Drive) backend.
//
// # Why this driver is "unofficial"
//
// Quark's official open platform has been in closed beta since 2024 and
// publishes no public documentation, no sandbox and no third-party client
// registration. This driver therefore speaks the reverse-engineered API at
// drive-pc.quark.cn that the Quark PC client and the web front-end use, with
// the account's browser cookie as the only credential. That interface carries
// no compatibility promise: field names, numeric error codes and whole
// endpoints have changed without notice before and will again. Every shape
// that could not be checked against a live account is flagged with an
// UNVERIFIED comment naming exactly what to verify.
//
// # Why the limits are so conservative
//
// Caps.Tier is TierUnofficial and Caps.QPS allows one metadata request and one
// upload request per second (two for downloads). Quark actively risk-controls
// traffic that does not look like its own client: bursts first draw a captcha
// challenge and then a multi-hour block of the whole account, which is far
// more expensive than the throughput a higher rate would buy. For the same
// reason every risk-control signal is mapped to provider.ErrRiskControl, which
// makes the upper layer circuit-break the account instead of retrying into a
// ban (see docs/DESIGN.md §4.2).
package quark

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// DefaultBaseURL is the API host the Quark PC client talks to. It is a var on
// the Options struct rather than a constant in the request paths so tests can
// point the whole driver at an httptest.Server.
const DefaultBaseURL = "https://drive-pc.quark.cn"

// DefaultUserAgent identifies us as the Quark PC client. Quark rejects or
// risk-controls requests whose User-Agent it does not recognise, and the same
// value must be replayed when fetching a download URL, so it is also published
// in Caps.LinkHeaders.
//
// UNVERIFIED: the exact client version suffix. Community clients have used
// this string successfully; Quark may start pinning a minimum version.
const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/112.0.0.0 Safari/537.36 " +
	"Core/1.112.4.406 QuarkPC/1.5.10"

// DefaultReferer is sent on API and download requests; the CDN 403s without it.
const DefaultReferer = "https://pan.quark.cn"

// DefaultRootID is the file id of the drive root. Quark uses the literal
// string "0" rather than an opaque id.
const DefaultRootID = "0"

// LinkTTL is how long a resolved download URL is reused before it is fetched
// again. Quark does not state a validity window and community clients observe
// links working for much longer, so this is a deliberate underestimate: a
// stale link costs a failed read plus a re-resolve, which is far more
// expensive under a 1 QPS metadata budget than resolving a little too often.
const LinkTTL = 30 * time.Minute

// listPageSize is the number of children requested per List call. Quark
// accepts larger pages, but the directory listing is the hottest metadata call
// and the QPS budget is 1/s, so a big page is worth the larger response.
const listPageSize = 100

// defaultPartSize is used when the pre-upload response does not name one.
const defaultPartSize = 8 << 20

// defaultTaskPollInterval is the pause between polls of an asynchronous
// move/delete task.
const defaultTaskPollInterval = 300 * time.Millisecond

// Options configures a Quark driver directly, bypassing the config map. Tests
// use it to inject an httpx.Client and a BaseURL pointing at an httptest
// server; the registry factory fills it in from the remote's config block.
type Options struct {
	// Client is the shared HTTP client (proxy routing, rate limiting,
	// circuit breaking). Required in production; when nil a plain client is
	// built so a driver is still usable in isolation.
	Client *httpx.Client
	// BaseURL overrides DefaultBaseURL. Every API path is built from it.
	BaseURL string
	// Cookie is the raw Cookie header value copied from a logged-in browser.
	Cookie string
	// RootID is the directory id treated as the drive root. Empty means "0".
	RootID string
	// UserAgent overrides DefaultUserAgent.
	UserAgent string
}

// Quark is the 夸克网盘 backend. It is safe for concurrent use.
type Quark struct {
	name   string
	cli    *httpx.Client
	base   string
	rootID string
	ua     string

	// mu guards the rotating cookie jar and the download link cache.
	mu sync.Mutex
	// cookieOrder preserves the order cookies were first seen so the header
	// we send stays byte-stable across calls.
	cookieOrder []string
	cookieVals  map[string]string
	links       map[string]cachedLink

	// taskPollInterval paces Move/Delete task polling. It is a field rather
	// than a constant so tests do not have to sleep for real.
	taskPollInterval time.Duration
	// pageSize is the List page size; a field so tests can exercise real
	// pagination without fabricating hundred-entry fixtures.
	pageSize int
}

type cachedLink struct {
	url     string
	expires time.Time
}

var (
	_ provider.Provider    = (*Quark)(nil)
	_ provider.Transporter = (*Quark)(nil)
)

// New builds a Quark driver from a remote's config block. Recognised keys:
//
//	cookie      (required) the Cookie header value from a logged-in browser
//	root_id     (optional) directory id to treat as the root, default "0"
//	user_agent  (optional) override for DefaultUserAgent
//	base_url    (optional) override for DefaultBaseURL
//
// The HTTP client comes from the daemon through provider.ConfigHTTPClient
// (httpx.FromConfig), so the driver inherits the proxy rules, the rate limiter
// and the circuit breaker; a standalone client is built only when the config
// carries none, which is the case in unit tests.
func New(name string, cfg map[string]any) (*Quark, error) {
	cookie, err := requiredString(cfg, "cookie")
	if err != nil {
		return nil, err
	}
	opt := Options{
		Cookie:    cookie,
		RootID:    optionalString(cfg, "root_id"),
		UserAgent: optionalString(cfg, "user_agent"),
		BaseURL:   optionalString(cfg, "base_url"),
	}
	ua := opt.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	// Prefer the daemon's client: it already applies the proxy rules, the rate
	// limiter and the circuit breaker for this remote. Quark risk-controls
	// requests whose User-Agent it does not recognise, so the agent is put on
	// the shared client when it carries none.
	client, shared := httpx.FromConfig(cfg, httpx.Options{Remote: name, UserAgent: ua})
	if shared && client.UserAgent == "" {
		client.UserAgent = ua
	}
	opt.Client = client
	return NewWithOptions(name, opt)
}

// NewWithOptions builds a Quark driver from an explicit Options. It is the
// constructor tests and embedders use.
func NewWithOptions(name string, opt Options) (*Quark, error) {
	if strings.TrimSpace(opt.Cookie) == "" {
		return nil, fmt.Errorf("quark: missing required config key %q (paste the Cookie header of a logged-in pan.quark.cn session)", "cookie")
	}
	ua := opt.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	base := strings.TrimSuffix(opt.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	root := opt.RootID
	if root == "" {
		root = DefaultRootID
	}
	cli := opt.Client
	if cli == nil {
		cli = httpx.New(httpx.Options{Remote: name, UserAgent: ua})
	}
	q := &Quark{
		name:             name,
		cli:              cli,
		base:             base,
		rootID:           root,
		ua:               ua,
		cookieVals:       map[string]string{},
		links:            map[string]cachedLink{},
		taskPollInterval: defaultTaskPollInterval,
		pageSize:         listPageSize,
	}
	q.setCookieHeader(opt.Cookie)
	return q, nil
}

// Name returns the remote name this driver instance was created for.
func (q *Quark) Name() string { return q.name }

// RootID is the directory id List and friends should be called with for the
// top of the drive. The Provider interface has no root accessor, so callers
// that need it (and the empty dirID shorthand inside this driver) use this.
func (q *Quark) RootID() string { return q.rootID }

// Capabilities reports the capability matrix. See the package comment for why
// the QPS numbers are so low and why LinkShareable is false.
func (q *Quark) Capabilities() provider.Caps {
	return provider.Caps{
		// UNVERIFIED: the forbidden set is the Windows-like set most domestic
		// drives document; verify against the real API's error on each character
		// and on names ending in a dot or a space.
		Naming:    provider.Naming{MaxNameBytes: 255, ForbiddenRunes: "\\:*?\"<>|"},
		HashTypes: []provider.HashType{provider.HashMD5, provider.HashSHA1},
		// Quark's hash handshake takes MD5 and SHA1 together; supplying only
		// one is not enough to attempt a rapid upload.
		RapidUpload:    []provider.HashType{provider.HashMD5, provider.HashSHA1},
		RangeRead:      true,
		PartSize:       defaultPartSize,
		MaxParts:       10000,
		UploadParallel: 1,
		ServerMove:     true,
		ServerRename:   true,
		// Quark has a server-side copy endpoint, but this driver does not
		// implement ServerCopier, so the matrix must not advertise it.
		ServerCopy: false,
		// No public delta/change feed exists for this API.
		Delta:   false,
		LinkTTL: LinkTTL,
		LinkHeaders: map[string]string{
			"User-Agent": q.ua,
			"Referer":    DefaultReferer,
		},
		// Quark's CDN requires the account Cookie on the download request, so
		// a link cannot be handed to an unrelated process (an external player,
		// the MCP get_download_url tool) and still work.
		LinkShareable: false,
		// UNVERIFIED: CDN request-rate threshold before risk control.
		QPS:             provider.QPS{Meta: 1, Download: 2, Upload: 1, Transfer: 4},
		MaxConnsPerHost: 4,
		Tier:            provider.TierUnofficial,
	}
}

// dirOrRoot maps the empty directory id onto the configured root.
func (q *Quark) dirOrRoot(id string) string {
	if id == "" {
		return q.rootID
	}
	return id
}

func requiredString(cfg map[string]any, key string) (string, error) {
	v, ok := cfg[key]
	if !ok {
		return "", fmt.Errorf("quark: missing required config key %q", key)
	}
	s := toString(v)
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("quark: config key %q is empty", key)
	}
	return s, nil
}

func optionalString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	return toString(v)
}

// toString accepts the scalar types a YAML decode can produce for what is
// logically a string (root_id: 0 is a very natural thing to write).
func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// SetTransport lets the daemon replace the HTTP client after construction.
func (q *Quark) SetTransport(client any) {
	if c, ok := client.(*httpx.Client); ok && c != nil {
		if c.UserAgent == "" {
			c.UserAgent = q.ua
		}
		q.cli = c
	}
}

func init() {
	provider.RegisterFields("quark", []provider.Field{
		{Name: "root_id", Prompt: "作为根的目录 id", Default: "0"},
		{Name: "user_agent", Prompt: "请求 User-Agent，留空用内置值"},
	}, provider.Credentials{Fields: []string{"cookie"},
		Note: "CloudFS 通过夸克 App 扫码授权取得会话；这是非官方接口，限流更保守"})
	provider.Register("quark", func(name string, cfg map[string]any) (provider.Provider, error) {
		return New(name, cfg)
	})
}
