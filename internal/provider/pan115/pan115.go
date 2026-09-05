// Package pan115 implements the provider.Provider interface for 115 网盘 on
// top of the official 115 Open platform (open.115.com), which 115 launched in
// 2025 and which is the only interface they tolerate from third parties.
//
// Two facts shape everything in this driver:
//
//   - 115 keeps only TWO valid refresh tokens per account per app. A third
//     authorisation silently invalidates the oldest one, so the rotated token
//     returned by RefreshToken must be persisted after every refresh.
//   - 115 actively bans third-party clients. Community reports put the ban
//     threshold near three requests per second, and the punishment is an
//     account-wide "please verify in the official client" state rather than a
//     429. The capability matrix therefore ships QPS{Meta:1, Download:2,
//     Upload:1} and every verification-style answer is mapped to
//     provider.ErrRiskControl so the upper layer circuit-breaks the account
//     instead of pushing it further (docs/DESIGN.md §4.1, §4.2).
package pan115

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// Defaults for the 115 Open endpoints and the download User-Agent.
const (
	// DefaultBaseURL hosts the Open file/upload APIs.
	DefaultBaseURL = "https://proapi.115.com"
	// DefaultPassportURL hosts the OAuth token endpoints.
	DefaultPassportURL = "https://passportapi.115.com"
	// DefaultRootID is the id of the 115 root directory.
	DefaultRootID = "0"
	// DownloadUA is sent both when asking for a download URL and when
	// fetching it. 115 binds the issued URL to the User-Agent of the request
	// that created it, so a mismatch produces a 403 rather than bytes.
	DownloadUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) cloudfs/115open"
	// pageLimit is the directory page size. 115 accepts large pages and the
	// driver wants as few metadata calls as possible at QPS 1.
	pageLimit = 1000
)

// Options configures a Pan115. Every network dependency is injectable so a
// test can point the driver at an httptest.Server.
type Options struct {
	// Client is the shared HTTP client (proxying, rate limiting, breaking).
	// Required.
	Client *httpx.Client
	// BaseURL overrides DefaultBaseURL.
	BaseURL string
	// PassportURL overrides DefaultPassportURL.
	PassportURL string
	// OSSEndpoint overrides the OSS endpoint 115 hands out with the upload
	// token. When it carries a scheme the driver addresses OSS path-style
	// (scheme://host/bucket/object), which is what httptest.Server needs.
	OSSEndpoint string

	ClientID     string
	ClientSecret string
	// RefreshToken is the long-lived credential; see RefreshToken for why it
	// must be persisted after use.
	RefreshToken string
	// AccessToken seeds the driver with a token that is already valid, so a
	// short-lived process need not spend a refresh on startup.
	AccessToken string
	// RootID is the directory the mount is rooted at; defaults to "0".
	RootID string
	// UserAgent overrides DownloadUA.
	UserAgent string
	// RangeHasher lets the driver answer 115's rapid-upload range challenge
	// for every upload. A per-upload hasher passed through WithRangeHasher
	// takes precedence; see RangeHasher for why this callback exists.
	RangeHasher RangeHasher
}

// Pan115 is the 115 网盘 provider.
type Pan115 struct {
	provider.TokenPersistence
	name         string
	http         *httpx.Client
	baseURL      string
	passportURL  string
	ossEndpoint  string
	rootID       string
	ua           string
	clientID     string
	clientSecret string
	rangeHasher  RangeHasher

	tokens tokenStore

	mu sync.Mutex
	// pickCodes caches file_id -> pick_code. Downloads are addressed by pick
	// code, not by id, so without this cache every read costs an extra
	// metadata call — expensive at QPS 1.
	pickCodes map[string]string
	links     map[string]cachedLink
	// oss caches the STS credentials for uploads; they last about an hour and
	// re-fetching them costs a metadata call the account can barely spare.
	oss       ossCreds
	ossExpiry time.Time
}

type cachedLink struct {
	link    provider.Link
	fetched time.Time
}

var (
	_ provider.Provider    = (*Pan115)(nil)
	_ provider.Transporter = (*Pan115)(nil)
)

// NewWithOptions builds a Pan115 from an explicit Options value.
func NewWithOptions(name string, opt Options) (*Pan115, error) {
	if opt.Client == nil {
		return nil, fmt.Errorf("pan115: Options.Client is required")
	}
	if opt.RefreshToken == "" && opt.AccessToken == "" {
		return nil, fmt.Errorf("pan115: missing required config key %q", "refresh_token")
	}
	p := &Pan115{
		name:         name,
		http:         opt.Client,
		baseURL:      strings.TrimRight(orDefault(opt.BaseURL, DefaultBaseURL), "/"),
		passportURL:  strings.TrimRight(orDefault(opt.PassportURL, DefaultPassportURL), "/"),
		ossEndpoint:  strings.TrimRight(opt.OSSEndpoint, "/"),
		rootID:       orDefault(opt.RootID, DefaultRootID),
		ua:           orDefault(opt.UserAgent, DownloadUA),
		clientID:     opt.ClientID,
		clientSecret: opt.ClientSecret,
		rangeHasher:  opt.RangeHasher,
		pickCodes:    map[string]string{},
		links:        map[string]cachedLink{},
	}
	p.tokens.access = opt.AccessToken
	p.tokens.refresh = opt.RefreshToken
	return p, nil
}

// New is the provider registry factory. It reads refresh_token (required
// unless access_token is given), client_id, client_secret, root_id and
// user_agent from the remote's config block.
func New(name string, cfg map[string]any) (provider.Provider, error) {
	refresh, err := cfgString(cfg, "refresh_token", false)
	if err != nil {
		return nil, err
	}
	access, err := cfgString(cfg, "access_token", false)
	if err != nil {
		return nil, err
	}
	if refresh == "" && access == "" {
		return nil, fmt.Errorf("pan115: missing required config key %q", "refresh_token")
	}
	clientID, err := cfgString(cfg, "client_id", false)
	if err != nil {
		return nil, err
	}
	clientSecret, err := cfgString(cfg, "client_secret", false)
	if err != nil {
		return nil, err
	}
	rootID, err := cfgString(cfg, "root_id", false)
	if err != nil {
		return nil, err
	}
	ua, err := cfgString(cfg, "user_agent", false)
	if err != nil {
		return nil, err
	}
	base, err := cfgString(cfg, "base_url", false)
	if err != nil {
		return nil, err
	}
	passport, err := cfgString(cfg, "passport_url", false)
	if err != nil {
		return nil, err
	}
	// Prefer the daemon's client: it already applies the proxy rules, the rate
	// limiter and the circuit breaker for this remote. 115 binds a download URL
	// to the User-Agent that asked for it, so the agent is put on the shared
	// client when it carries none.
	client, shared := httpx.FromConfig(cfg, httpx.Options{
		Remote:    name,
		UserAgent: orDefault(ua, DownloadUA),
	})
	if shared && client.UserAgent == "" {
		client.UserAgent = orDefault(ua, DownloadUA)
	}
	return NewWithOptions(name, Options{
		Client:       client,
		BaseURL:      base,
		PassportURL:  passport,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RefreshToken: refresh,
		AccessToken:  access,
		RootID:       rootID,
		UserAgent:    ua,
	})
}

// SetTransport lets the daemon replace the HTTP client after construction.
func (p *Pan115) SetTransport(client any) {
	if c, ok := client.(*httpx.Client); ok && c != nil {
		if c.UserAgent == "" {
			c.UserAgent = p.ua
		}
		p.http = c
	}
}

func init() { provider.Register("pan115", New) }

// Name returns the remote's configured name.
func (p *Pan115) Name() string { return p.name }

// Capabilities reports what this backend can do. The QPS values are
// deliberately at the floor: 115 bans clients that push harder, and a ban is
// far more expensive than a slow directory walk.
func (p *Pan115) Capabilities() provider.Caps {
	return provider.Caps{
		HashTypes:      []provider.HashType{provider.HashSHA1},
		RapidUpload:    []provider.HashType{provider.HashSHA1},
		RangeRead:      true,
		PartSize:       8 << 20,
		MaxParts:       10000,
		UploadParallel: 1,
		ServerMove:     true,
		ServerRename:   true,
		ServerCopy:     false,
		// 115 Open has no delta / change feed, so the MetaStore must poll.
		Delta:   false,
		LinkTTL: 2 * time.Hour,
		// A 115 download URL only serves the User-Agent that requested it.
		LinkHeaders:     map[string]string{"User-Agent": p.ua},
		LinkShareable:   true,
		QPS:             provider.QPS{Meta: 1, Download: 2, Upload: 1},
		MaxConnsPerHost: 4,
		Tier:            provider.TierOfficial,
	}
}

// RootID returns the directory id the mount is rooted at.
func (p *Pan115) RootID() string { return p.rootID }

// fileItem is one row of /open/ufile/files.
type fileItem struct {
	FID  flexString `json:"fid"`
	CID  flexString `json:"cid"`
	PID  flexString `json:"pid"`
	FC   flexString `json:"fc"`
	FN   string     `json:"fn"`
	FS   flexInt64  `json:"fs"`
	SHA1 string     `json:"sha1"`
	PC   string     `json:"pc"`
	UPT  flexInt64  `json:"upt"`
	UET  flexInt64  `json:"uet"`
}

// entry converts a listing row into a provider.Entry.
//
// UNVERIFIED: 115 Open documents `fc` as "0" for a folder and "1" for a file,
// and keys every row by `fid`. The classic web API instead uses `cid` for
// folders and omits `fc`, and some Open responses still look like that, so
// both shapes are accepted. Confirm against a live account.
func (it fileItem) entry(fallbackParent string) provider.Entry {
	id := string(it.FID)
	kind := provider.KindFile
	switch {
	case string(it.FC) == "0":
		kind = provider.KindDir
	case string(it.FC) == "1":
		kind = provider.KindFile
	case id == "" && it.CID != "":
		// No fc and a cid: classic-style folder row.
		kind = provider.KindDir
	case it.SHA1 == "":
		kind = provider.KindDir
	}
	if id == "" {
		id = string(it.CID)
	}
	parent := string(it.PID)
	if parent == "" {
		parent = fallbackParent
	}
	mod := int64(it.UPT)
	if mod == 0 {
		mod = int64(it.UET)
	}
	e := provider.Entry{
		ID:       id,
		ParentID: parent,
		Name:     it.FN,
		Kind:     kind,
		Size:     int64(it.FS),
		Version:  it.SHA1,
	}
	if mod > 0 {
		e.ModTime = time.Unix(mod, 0).UTC()
	}
	if kind == provider.KindFile {
		if it.SHA1 != "" {
			e.Hashes = provider.Hashes{provider.HashSHA1: strings.ToLower(it.SHA1)}
		}
		if e.Version == "" {
			e.Version = strconv.FormatInt(mod, 10)
		}
	} else {
		e.Size = 0
		e.Version = strconv.FormatInt(mod, 10)
	}
	return e
}

// List returns one page of dirID. The cursor is the next offset, encoded as a
// decimal string; "" means the listing is complete.
func (p *Pan115) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	if dirID == "" {
		dirID = p.rootID
	}
	offset := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("pan115: bad list cursor %q: %w", cursor, err)
		}
		offset = n
	}
	q := url.Values{}
	q.Set("cid", dirID)
	q.Set("limit", strconv.Itoa(pageLimit))
	q.Set("offset", strconv.Itoa(offset))
	q.Set("show_dir", "1")
	// o/asc pin a stable order so paging cannot skip or repeat rows while the
	// directory is being written to.
	q.Set("o", "file_name")
	q.Set("asc", "1")

	var items []fileItem
	env, err := p.call(ctx, httpx.Request{
		Method: "GET",
		URL:    p.baseURL + "/open/ufile/files?" + q.Encode(),
		Class:  ratelimit.Meta,
	}, &items)
	if err != nil {
		return nil, "", err
	}
	out := make([]provider.Entry, 0, len(items))
	for _, it := range items {
		e := it.entry(dirID)
		if e.ID == "" {
			continue
		}
		p.rememberPickCode(e.ID, it.PC)
		out = append(out, e)
	}
	// Prefer the server's total, but a full page with no `count` still means
	// "there is probably more": stopping there would silently truncate a
	// directory listing, which is the one failure the VFS cannot detect.
	next := ""
	switch total := int(env.Count); {
	case total > 0 && offset+len(items) < total:
		next = strconv.Itoa(offset + len(items))
	case total == 0 && len(items) == pageLimit:
		next = strconv.Itoa(offset + len(items))
	}
	return out, next, nil
}

// folderInfo is the payload of /open/folder/get_info. 115 uses the same
// endpoint for files and folders.
type folderInfo struct {
	FileID       flexString      `json:"file_id"`
	FileName     string          `json:"file_name"`
	FolderName   string          `json:"folder_name"`
	FileCategory flexString      `json:"file_category"`
	SizeByte     flexInt64       `json:"size_byte"`
	Size         json.RawMessage `json:"size"`
	SHA1         string          `json:"sha1"`
	PickCode     string          `json:"pick_code"`
	UTime        flexInt64       `json:"utime"`
	PTime        flexInt64       `json:"ptime"`
	Paths        []struct {
		FileID   flexString `json:"file_id"`
		FileName string     `json:"file_name"`
	} `json:"paths"`
}

// Stat returns one entry by id.
//
// UNVERIFIED: /open/folder/get_info is documented for folders and, in
// community clients, answers for files too, reporting `file_category` "1" and
// a sha1. If a live account disagrees for files, the fallback below (sha1
// present means file) still classifies correctly.
func (p *Pan115) Stat(ctx context.Context, id string) (provider.Entry, error) {
	if id == "" {
		id = p.rootID
	}
	var info folderInfo
	if _, err := p.call(ctx, httpx.Request{
		Method: "GET",
		URL:    p.baseURL + "/open/folder/get_info?file_id=" + url.QueryEscape(id),
		Class:  ratelimit.Meta,
	}, &info); err != nil {
		return provider.Entry{}, err
	}
	e := provider.Entry{ID: id, Name: firstNonEmpty(info.FileName, info.FolderName)}
	if s := string(info.FileID); s != "" {
		e.ID = s
	}
	switch {
	case string(info.FileCategory) == "0":
		e.Kind = provider.KindDir
	case string(info.FileCategory) == "1":
		e.Kind = provider.KindFile
	case info.SHA1 != "":
		e.Kind = provider.KindFile
	default:
		e.Kind = provider.KindDir
	}
	if n := len(info.Paths); n > 0 {
		e.ParentID = string(info.Paths[n-1].FileID)
	}
	if e.Kind == provider.KindFile {
		e.Size = int64(info.SizeByte)
		if e.Size == 0 {
			// `size` is a human string ("1.2MB") for folders but a number for
			// files on some responses; only take it when it parses.
			if n, ok := numericJSON(info.Size); ok {
				e.Size = n
			}
		}
		if info.SHA1 != "" {
			e.Hashes = provider.Hashes{provider.HashSHA1: strings.ToLower(info.SHA1)}
			e.Version = info.SHA1
		}
	}
	mod := int64(info.UTime)
	if mod == 0 {
		mod = int64(info.PTime)
	}
	if mod > 0 {
		e.ModTime = time.Unix(mod, 0).UTC()
	}
	if e.Version == "" {
		e.Version = strconv.FormatInt(mod, 10)
	}
	p.rememberPickCode(e.ID, info.PickCode)
	return e, nil
}

func (p *Pan115) rememberPickCode(id, pc string) {
	if id == "" || pc == "" {
		return
	}
	p.mu.Lock()
	p.pickCodes[id] = pc
	p.mu.Unlock()
}

// pickCode resolves a file id to the pick code the download endpoint wants,
// consulting the cache before spending a metadata call.
func (p *Pan115) pickCode(ctx context.Context, id string) (string, error) {
	p.mu.Lock()
	pc := p.pickCodes[id]
	p.mu.Unlock()
	if pc != "" {
		return pc, nil
	}
	if _, err := p.Stat(ctx, id); err != nil {
		return "", err
	}
	p.mu.Lock()
	pc = p.pickCodes[id]
	p.mu.Unlock()
	if pc == "" {
		return "", fmt.Errorf("pan115: no pick code for %s: %w", id, provider.ErrUnsupported)
	}
	return pc, nil
}

// downURLEntry is one value of the /open/ufile/downurl map.
type downURLEntry struct {
	FileName string    `json:"file_name"`
	FileSize flexInt64 `json:"file_size"`
	PickCode string    `json:"pick_code"`
	URL      struct {
		URL string `json:"url"`
	} `json:"url"`
}

// DownloadURL returns a direct link. The link is bound to the User-Agent used
// here, which is why Caps.LinkHeaders repeats it: whoever fetches the URL —
// the VFS, an external player, the MCP tool — has to send the same UA.
func (p *Pan115) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	pc, err := p.pickCode(ctx, id)
	if err != nil {
		return provider.Link{}, err
	}
	var m map[string]downURLEntry
	if _, err := p.call(ctx, httpx.Request{
		Method: "POST",
		URL:    p.baseURL + "/open/ufile/downurl",
		Class:  ratelimit.Download,
		Header: http.Header{"User-Agent": []string{p.ua}},
		Form:   map[string]string{"pick_code": pc, "ua": p.ua},
	}, &m); err != nil {
		return provider.Link{}, err
	}
	pick, ok := m[id]
	if !ok {
		// 115 keys the map by its own file id, which is not always the id the
		// caller holds (a shared file, for instance). With a single entry the
		// answer is unambiguous.
		if len(m) != 1 {
			return provider.Link{}, fmt.Errorf("pan115: no download url for %s: %w", id, provider.ErrNotFound)
		}
		for _, v := range m {
			pick = v
		}
	}
	if pick.URL.URL == "" {
		return provider.Link{}, fmt.Errorf("pan115: empty download url for %s: %w", id, provider.ErrNotFound)
	}
	return provider.Link{
		URL:       pick.URL.URL,
		ExpiresAt: time.Now().Add(2 * time.Hour),
		Headers:   map[string]string{"User-Agent": p.ua},
	}, nil
}

// link returns a cached download URL for (id, version), refreshing it when it
// is missing or close to expiry. Caching matters because a sequential read of
// a large file issues many ranges and 115 allows very few metadata calls.
func (p *Pan115) link(ctx context.Context, id, version string) (provider.Link, error) {
	key := id + "\x00" + version
	p.mu.Lock()
	c, ok := p.links[key]
	p.mu.Unlock()
	if ok && time.Now().Add(time.Minute).Before(c.link.ExpiresAt) {
		return c.link, nil
	}
	l, err := p.DownloadURL(ctx, id)
	if err != nil {
		return provider.Link{}, err
	}
	p.mu.Lock()
	p.links[key] = cachedLink{link: l, fetched: time.Now()}
	p.mu.Unlock()
	return l, nil
}

// forgetLink drops a cached link, so the next read re-resolves it.
func (p *Pan115) forgetLink(id, version string) {
	p.mu.Lock()
	delete(p.links, id+"\x00"+version)
	p.mu.Unlock()
}

// ReadRange streams n bytes from off.
//
// 115 has no conditional-read facility, so version cannot be enforced on the
// wire; it is used as part of the link cache key, which means a caller that
// notices a new version stops reusing the old file's URL.
func (p *Pan115) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	l, err := p.link(ctx, id, version)
	if err != nil {
		return nil, err
	}
	rc, err := p.fetch(ctx, l, off, n)
	if err == nil {
		return rc, nil
	}
	// A 403/410 from the CDN means the URL died early. Drop it and try once
	// with a fresh one before reporting failure upward.
	if !isLinkExpired(err) {
		return nil, err
	}
	p.forgetLink(id, version)
	l, err2 := p.link(ctx, id, version)
	if err2 != nil {
		return nil, err
	}
	return p.fetch(ctx, l, off, n)
}

func (p *Pan115) fetch(ctx context.Context, l provider.Link, off, n int64) (io.ReadCloser, error) {
	h := http.Header{}
	for k, v := range l.Headers {
		h.Set(k, v)
	}
	h.Set("Range", httpx.RangeHeader(off, n))
	resp, err := p.http.Do(ctx, httpx.Request{
		Method:       "GET",
		URL:          l.URL,
		Class:        ratelimit.Download,
		Header:       h,
		Stream:       true,
		ExpectStatus: []int{http.StatusOK, http.StatusPartialContent},
	})
	if err != nil {
		return nil, err
	}
	// A 115 CDN node that ignores Range would otherwise hand back the start of
	// the file as if it were the requested window.
	return httpx.RangeBody(resp, off, n)
}

// Mkdir creates a directory under parentID.
//
// UNVERIFIED: /open/folder/add answers with {file_id, file_name}; some builds
// return `cid` instead. Both are read, and a missing id falls back to looking
// the child up by name so the caller never receives an entry with no id.
func (p *Pan115) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	if parentID == "" {
		parentID = p.rootID
	}
	var out struct {
		FileID   flexString `json:"file_id"`
		CID      flexString `json:"cid"`
		FileName string     `json:"file_name"`
	}
	if _, err := p.call(ctx, httpx.Request{
		Method: "POST",
		URL:    p.baseURL + "/open/folder/add",
		Class:  ratelimit.Meta,
		Form:   map[string]string{"pid": parentID, "file_name": name},
	}, &out); err != nil {
		return provider.Entry{}, err
	}
	id := firstNonEmpty(string(out.FileID), string(out.CID))
	if id == "" {
		return p.findChild(ctx, parentID, name)
	}
	return provider.Entry{
		ID:       id,
		ParentID: parentID,
		Name:     firstNonEmpty(out.FileName, name),
		Kind:     provider.KindDir,
		ModTime:  time.Now().UTC(),
	}, nil
}

// Rename renames an entry in place. 115 answers the update call with a status
// only, so the fresh entry is read back rather than reconstructed locally.
func (p *Pan115) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	if _, err := p.call(ctx, httpx.Request{
		Method: "POST",
		URL:    p.baseURL + "/open/ufile/update",
		Class:  ratelimit.Meta,
		Form:   map[string]string{"file_id": id, "file_name": newName},
	}, nil); err != nil {
		return provider.Entry{}, err
	}
	return p.Stat(ctx, id)
}

// Move re-parents an entry server-side.
func (p *Pan115) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	if newParentID == "" {
		newParentID = p.rootID
	}
	if _, err := p.call(ctx, httpx.Request{
		Method: "POST",
		URL:    p.baseURL + "/open/ufile/move",
		Class:  ratelimit.Meta,
		Form:   map[string]string{"file_ids": id, "to_cid": newParentID},
	}, nil); err != nil {
		return provider.Entry{}, err
	}
	return p.Stat(ctx, id)
}

// Delete moves an entry to the 115 recycle bin.
func (p *Pan115) Delete(ctx context.Context, id string) error {
	_, err := p.call(ctx, httpx.Request{
		Method: "POST",
		URL:    p.baseURL + "/open/ufile/delete",
		Class:  ratelimit.Meta,
		Form:   map[string]string{"file_ids": id},
	}, nil)
	if err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.pickCodes, id)
	p.mu.Unlock()
	return nil
}

// findChild locates a child by name, paging through the directory. It is the
// safe fallback for endpoints that do not return the id of what they created.
func (p *Pan115) findChild(ctx context.Context, parentID, name string) (provider.Entry, error) {
	cursor := ""
	for {
		entries, next, err := p.List(ctx, parentID, cursor)
		if err != nil {
			return provider.Entry{}, err
		}
		for _, e := range entries {
			if e.Name == name {
				return e, nil
			}
		}
		if next == "" {
			return provider.Entry{}, fmt.Errorf("pan115: %q not found under %s: %w", name, parentID, provider.ErrNotFound)
		}
		cursor = next
	}
}

// cfgString reads a string-ish config value. YAML gives ints for numeric keys
// such as root_id, so numbers are accepted and formatted.
func cfgString(cfg map[string]any, key string, required bool) (string, error) {
	v, ok := cfg[key]
	if !ok || v == nil {
		if required {
			return "", fmt.Errorf("pan115: missing required config key %q", key)
		}
		return "", nil
	}
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		if s == "" && required {
			return "", fmt.Errorf("pan115: config key %q is empty", key)
		}
		return s, nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return strconv.FormatInt(int64(t), 10), nil
	default:
		return "", fmt.Errorf("pan115: config key %q must be a string, got %T", key, v)
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// numericJSON parses a raw JSON scalar that may be a number or a numeric
// string, reporting false for anything else (such as "1.2MB").
func numericJSON(raw json.RawMessage) (int64, bool) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return int64(f), true
}
