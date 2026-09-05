package pan115

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// Upload constants.
const (
	// statusRapidUploaded is /open/upload/init's "I already have this file".
	statusRapidUploaded = 2
	// statusNeedUpload means "send the bytes to OSS".
	statusNeedUpload = 1
	// signCheckStatus is the second step of the rapid-upload handshake: 115
	// believes it has a file with this SHA1 and wants the SHA1 of one byte
	// range as proof that the caller really holds the content.
	signCheckStatus = 701
	// maxSignRounds bounds the handshake. 115 normally asks once; more than a
	// couple of rounds means something is wrong and looping would look like
	// abuse to their risk control.
	maxSignRounds = 3
	// preIDLen is the prefix 115 hashes into `preid`.
	//
	// UNVERIFIED: community clients use the first 128 KiB. preid is only a
	// pre-filter, so a wrong length costs a missed rapid upload, never a
	// corrupt file.
	preIDLen = 128 << 10
	// defaultOSSEndpoint is used only when 115 hands back no endpoint.
	// UNVERIFIED: the region 115 buckets live in may change.
	defaultOSSEndpoint = "oss-cn-shenzhen.aliyuncs.com"
)

// RangeHasher returns the lowercase or uppercase hex SHA1 of the bytes
// [start, end] (inclusive, as 115 states its ranges) of the file being
// uploaded.
//
// It exists because 115's rapid upload is a two-step handshake and
// provider.BeginUpload only receives hashes, not the content: when 115 answers
// "statuscode 701, sign_check 1024-2047" the client must hash exactly those
// bytes and repeat the call. The uploader supplies one via WithRangeHasher (it
// has the staged blob on disk); without one the driver refuses the upload
// rather than guessing, so a file is never registered against content 115
// holds but we never verified.
type RangeHasher = provider.UploadRangeHasher

// WithRangeHasher attaches a RangeHasher to ctx for the duration of one
// upload. BeginUpload prefers it over Options.RangeHasher.
func WithRangeHasher(ctx context.Context, h RangeHasher) context.Context {
	return provider.WithUploadRangeHasher(ctx, h)
}

// FileRangeHasher returns a RangeHasher that reads from a local file — the
// staged blob the upload journal keeps.
func FileRangeHasher(path string) RangeHasher {
	return func(ctx context.Context, start, end int64) (string, error) {
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return "", err
		}
		return provider.ContentRangeHasher(f, st.Size())(ctx, start, end)
	}
}

// hasher picks the RangeHasher for this call: the per-upload one from the
// context wins over the driver-wide one from Options.
func (p *Pan115) hasher(ctx context.Context) RangeHasher {
	if h := provider.UploadRangeHasherFrom(ctx); h != nil {
		return h
	}
	return p.rangeHasher
}

// initResponse is the payload of /open/upload/init.
//
// UNVERIFIED: field names come from the 115 Open upload documentation and
// community SDKs. `pick_code` is also seen as `pickcode`, and the rapid-upload
// answer does not always carry `file_id`; both are handled, and a missing id
// falls back to looking the file up by name instead of inventing one.
type initResponse struct {
	Status      flexInt    `json:"status"`
	StatusCode  flexInt    `json:"statuscode"`
	PickCode    string     `json:"pick_code"`
	PickCodeAlt string     `json:"pickcode"`
	Bucket      string     `json:"bucket"`
	Object      string     `json:"object"`
	SignKey     string     `json:"sign_key"`
	SignCheck   string     `json:"sign_check"`
	FileID      flexString `json:"file_id"`
	Target      string     `json:"target"`
	Callback    struct {
		Callback    string `json:"callback"`
		CallbackVar string `json:"callback_var"`
	} `json:"callback"`
}

func (r initResponse) pickCode() string { return firstNonEmpty(r.PickCode, r.PickCodeAlt) }

// BeginUpload starts an upload, attempting 115's SHA1 rapid upload first.
//
// The rapid path is two steps. Step one posts the full-file SHA1; 115 answers
// either "have it" (status 2), "send it" (status 1), or "prove it"
// (statuscode 701 plus sign_key and a sign_check byte range). Step two hashes
// that range and repeats the call with sign_key/sign_val. See RangeHasher for
// why the content is reached through a callback.
func (p *Pan115) BeginUpload(ctx context.Context, parentID, name string, size int64, h provider.Hashes) (provider.UploadSession, error) {
	if parentID == "" {
		parentID = p.rootID
	}
	sum := strings.ToUpper(h[provider.HashSHA1])
	if sum == "" {
		return provider.UploadSession{}, fmt.Errorf(
			"%w: pan115: upload requires the file sha1 (Caps.HashTypes advertises sha1)", provider.ErrUnsupported)
	}
	hash := p.hasher(ctx)
	form := map[string]string{
		"file_name": name,
		"file_size": strconv.FormatInt(size, 10),
		// 115 addresses an upload destination as U_<area>_<cid>; area 1 is the
		// user's own space.
		"target": "U_1_" + parentID,
		"fileid": sum,
	}
	if hash != nil && size > 0 {
		end := int64(preIDLen)
		if size < end {
			end = size
		}
		if pre, err := hash(ctx, 0, end-1); err == nil && pre != "" {
			form["preid"] = strings.ToUpper(pre)
		}
	}

	var init initResponse
	for round := 0; ; round++ {
		init = initResponse{}
		if _, err := p.call(ctx, httpx.Request{
			Method: "POST",
			URL:    p.baseURL + "/open/upload/init",
			Class:  ratelimit.Upload,
			Form:   form,
		}, &init); err != nil {
			return provider.UploadSession{}, err
		}
		if int(init.StatusCode) != signCheckStatus || init.SignKey == "" {
			break
		}
		if round >= maxSignRounds {
			return provider.UploadSession{}, fmt.Errorf(
				"pan115: upload init still asking for a range signature after %d rounds", maxSignRounds)
		}
		if hash == nil {
			// Degrade loudly. Answering with a wrong signature would either be
			// rejected or, worse, register this name against whatever content
			// 115 already holds for that SHA1.
			return provider.UploadSession{}, fmt.Errorf(
				"%w: pan115: 115 asked for a signature of bytes %s but no RangeHasher was attached (see pan115.WithRangeHasher)",
				provider.ErrUnsupported, init.SignCheck)
		}
		start, end, err := parseSignCheck(init.SignCheck)
		if err != nil {
			return provider.UploadSession{}, err
		}
		val, err := hash(ctx, start, end)
		if err != nil {
			return provider.UploadSession{}, fmt.Errorf("pan115: sign range %s: %w", init.SignCheck, err)
		}
		form["sign_key"] = init.SignKey
		form["sign_val"] = strings.ToUpper(val)
	}

	if int(init.Status) == statusRapidUploaded {
		e, err := p.rapidEntry(ctx, init, parentID, name, size, sum)
		if err != nil {
			return provider.UploadSession{}, err
		}
		return provider.UploadSession{
			ID:        "rapid-" + e.ID,
			RapidDone: true,
			Entry:     &e,
			Opaque:    map[string]string{"pick_code": init.pickCode()},
		}, nil
	}
	if int(init.Status) != statusNeedUpload {
		return provider.UploadSession{}, fmt.Errorf("pan115: upload init returned status %d (statuscode %d)",
			int(init.Status), int(init.StatusCode))
	}
	if init.Bucket == "" || init.Object == "" {
		return provider.UploadSession{}, fmt.Errorf("pan115: upload init returned no OSS target for %q", name)
	}

	creds, endpoint, err := p.ossToken(ctx)
	if err != nil {
		return provider.UploadSession{}, err
	}
	uploadID, err := p.ossInitiate(ctx, creds, endpoint, init.Bucket, init.Object)
	if err != nil {
		return provider.UploadSession{}, err
	}
	return provider.UploadSession{
		ID:       firstNonEmpty(init.pickCode(), init.Object),
		PartSize: 8 << 20,
		// Opaque is journalled, so it must carry everything a restarted
		// process needs. Credentials are deliberately NOT stored: they expire
		// in about an hour and are re-fetched with the account's own token.
		Opaque: map[string]string{
			"bucket":       init.Bucket,
			"object":       init.Object,
			"upload_id":    uploadID,
			"endpoint":     endpoint,
			"callback":     init.Callback.Callback,
			"callback_var": init.Callback.CallbackVar,
			"pick_code":    init.pickCode(),
			"parent_id":    parentID,
			"name":         name,
			"sha1":         sum,
			"size":         strconv.FormatInt(size, 10),
		},
	}, nil
}

// rapidEntry resolves the entry a rapid upload produced.
func (p *Pan115) rapidEntry(ctx context.Context, init initResponse, parentID, name string, size int64, sum string) (provider.Entry, error) {
	if id := string(init.FileID); id != "" {
		p.rememberPickCode(id, init.pickCode())
		e, err := p.Stat(ctx, id)
		if err == nil {
			return e, nil
		}
		// The id is trustworthy even when the follow-up Stat is throttled.
		return provider.Entry{
			ID: id, ParentID: parentID, Name: name, Kind: provider.KindFile,
			Size: size, ModTime: time.Now().UTC(), Version: strings.ToLower(sum),
			Hashes: provider.Hashes{provider.HashSHA1: strings.ToLower(sum)},
		}, nil
	}
	// No id in the response: find the file we just created rather than
	// returning an entry the cache would key under an empty id.
	return p.findChild(ctx, parentID, name)
}

// parseSignCheck splits 115's inclusive "start-end" byte range.
func parseSignCheck(s string) (int64, int64, error) {
	parts := strings.SplitN(strings.TrimSpace(s), "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("pan115: malformed sign_check %q", s)
	}
	start, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	end, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err1 != nil || err2 != nil || end < start {
		return 0, 0, fmt.Errorf("pan115: malformed sign_check %q", s)
	}
	return start, end, nil
}

// ossToken returns cached STS credentials, refreshing them when they are gone
// or close to expiry, plus the endpoint to address.
func (p *Pan115) ossToken(ctx context.Context) (ossCreds, string, error) {
	p.mu.Lock()
	if p.oss.valid() && time.Now().Add(time.Minute).Before(p.ossExpiry) {
		c, ep := p.oss, p.ossEndpointFor(p.oss)
		p.mu.Unlock()
		return c, ep, nil
	}
	p.mu.Unlock()

	var c ossCreds
	if _, err := p.call(ctx, httpx.Request{
		Method: "GET",
		URL:    p.baseURL + "/open/upload/get_token",
		Class:  ratelimit.Upload,
	}, &c); err != nil {
		return ossCreds{}, "", err
	}
	c.normalise()
	if !c.valid() {
		return ossCreds{}, "", fmt.Errorf("pan115: upload token response carried no credentials")
	}
	exp := time.Now().Add(30 * time.Minute)
	if c.Expiration != "" {
		if t, err := time.Parse(time.RFC3339, c.Expiration); err == nil {
			exp = t
		}
	}
	p.mu.Lock()
	p.oss, p.ossExpiry = c, exp
	ep := p.ossEndpointFor(c)
	p.mu.Unlock()
	return c, ep, nil
}

// ossEndpointFor resolves which OSS origin to talk to. Caller holds p.mu.
func (p *Pan115) ossEndpointFor(c ossCreds) string {
	if p.ossEndpoint != "" {
		return p.ossEndpoint
	}
	if c.Endpoint != "" {
		return c.Endpoint
	}
	return defaultOSSEndpoint
}

// ossRequest signs and performs one OSS call.
func (p *Pan115) ossRequest(ctx context.Context, creds ossCreds, endpoint, bucket, object, method string,
	query url.Values, header http.Header, body []byte, class ratelimit.Class) (*httpx.Response, error) {

	if header == nil {
		header = http.Header{}
	}
	date := time.Now().UTC().Format(http.TimeFormat)
	header.Set("Date", date)
	if creds.SecurityToken != "" {
		header.Set("x-oss-security-token", creds.SecurityToken)
	}
	sts := ossStringToSign(method, header.Get("Content-MD5"), header.Get("Content-Type"), date,
		header, bucket, object, query)
	header.Set("Authorization", ossAuthorization(creds.AccessKeyID, creds.AccessKeySecret, sts))

	req := httpx.Request{
		Method: method,
		URL:    ossURL(endpoint, bucket, object, query),
		Class:  class,
		Header: header,
	}
	if body != nil {
		req.Body = bytes.NewReader(body)
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	return p.http.Do(ctx, req)
}

// ossInitiate opens the multipart upload and returns its upload id.
func (p *Pan115) ossInitiate(ctx context.Context, creds ossCreds, endpoint, bucket, object string) (string, error) {
	q := url.Values{"uploads": {""}}
	resp, err := p.ossRequest(ctx, creds, endpoint, bucket, object, "POST", q, nil, nil, ratelimit.Upload)
	if err != nil {
		return "", fmt.Errorf("pan115: initiate multipart upload: %w", err)
	}
	var out initiateMultipartUploadResult
	if err := xml.Unmarshal(resp.Bytes, &out); err != nil {
		return "", fmt.Errorf("pan115: decode InitiateMultipartUploadResult: %w", err)
	}
	if out.UploadID == "" {
		return "", fmt.Errorf("pan115: initiate multipart upload returned no UploadId")
	}
	return out.UploadID, nil
}

// sessionOSS pulls the OSS coordinates out of a (possibly journal-restored)
// session. A session without them cannot be resumed, and ErrNotFound is the
// signal the uploader uses to start a fresh one.
func sessionOSS(s provider.UploadSession) (bucket, object, uploadID, endpoint string, err error) {
	get := func(k string) string {
		if s.Opaque == nil {
			return ""
		}
		return s.Opaque[k]
	}
	bucket, object, uploadID, endpoint = get("bucket"), get("object"), get("upload_id"), get("endpoint")
	if bucket == "" || object == "" || uploadID == "" {
		return "", "", "", "", fmt.Errorf("%w: pan115: upload session %q has no OSS state", provider.ErrNotFound, s.ID)
	}
	return bucket, object, uploadID, endpoint, nil
}

// UploadPart uploads one part. OSS part numbers are 1-based while the caller
// counts from 0, so idx+1 goes on the wire.
//
// The part is buffered in memory because both the OSS signature and a retry
// need the exact bytes; parts are Caps.PartSize (8 MiB), which is the size the
// uploader streams anyway.
func (p *Pan115) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	bucket, object, uploadID, endpoint, err := sessionOSS(s)
	if err != nil {
		return provider.PartToken{}, err
	}
	data := make([]byte, 0, n)
	buf := bytes.NewBuffer(data)
	if _, err := io.Copy(buf, io.LimitReader(r, n)); err != nil {
		return provider.PartToken{}, err
	}
	if int64(buf.Len()) != n {
		return provider.PartToken{}, fmt.Errorf("pan115: part %d: read %d of %d bytes", idx, buf.Len(), n)
	}
	creds, ep, err := p.ossToken(ctx)
	if err != nil {
		return provider.PartToken{}, err
	}
	if endpoint == "" {
		endpoint = ep
	}
	q := url.Values{
		"partNumber": {strconv.Itoa(idx + 1)},
		"uploadId":   {uploadID},
	}
	resp, err := p.ossRequest(ctx, creds, endpoint, bucket, object, "PUT", q, nil, buf.Bytes(), ratelimit.Upload)
	if err != nil {
		return provider.PartToken{}, fmt.Errorf("pan115: upload part %d: %w", idx, err)
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if etag == "" {
		return provider.PartToken{}, fmt.Errorf("pan115: part %d: OSS returned no ETag", idx)
	}
	return provider.PartToken{Index: idx, ETag: etag}, nil
}

// callbackResult is what 115 returns through the OSS upload callback once the
// multipart upload completes: the OSS response body is a 115 envelope, not
// OSS XML.
type callbackResult struct {
	FileID   flexString `json:"file_id"`
	FileName string     `json:"file_name"`
	PickCode string     `json:"pick_code"`
	SHA1     string     `json:"sha1"`
	FileSize flexInt64  `json:"file_size"`
	CID      flexString `json:"cid"`
}

// CompleteUpload finishes the multipart upload. The x-oss-callback headers
// make OSS notify 115, whose answer (a 115 JSON envelope, not OSS XML) is the
// authoritative record of the new file.
func (p *Pan115) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	bucket, object, uploadID, endpoint, err := sessionOSS(s)
	if err != nil {
		return provider.Entry{}, err
	}
	sorted := append([]provider.PartToken(nil), parts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Index < sorted[j].Index })
	body := completeMultipartUpload{Parts: make([]ossPart, 0, len(sorted))}
	for _, pt := range sorted {
		body.Parts = append(body.Parts, ossPart{PartNumber: pt.Index + 1, ETag: `"` + strings.Trim(pt.ETag, `"`) + `"`})
	}
	xmlBody, err := xml.Marshal(body)
	if err != nil {
		return provider.Entry{}, err
	}

	creds, ep, err := p.ossToken(ctx)
	if err != nil {
		return provider.Entry{}, err
	}
	if endpoint == "" {
		endpoint = ep
	}
	header := http.Header{"Content-Type": []string{"application/xml"}}
	if cb := s.Opaque["callback"]; cb != "" {
		header.Set("x-oss-callback", base64.StdEncoding.EncodeToString([]byte(cb)))
	}
	if cv := s.Opaque["callback_var"]; cv != "" {
		header.Set("x-oss-callback-var", base64.StdEncoding.EncodeToString([]byte(cv)))
	}
	q := url.Values{"uploadId": {uploadID}}
	resp, err := p.ossRequest(ctx, creds, endpoint, bucket, object, "POST", q, header, xmlBody, ratelimit.Upload)
	if err != nil {
		return provider.Entry{}, fmt.Errorf("pan115: complete multipart upload: %w", err)
	}

	parentID := firstNonEmpty(s.Opaque["parent_id"], p.rootID)
	name := s.Opaque["name"]
	size, _ := strconv.ParseInt(s.Opaque["size"], 10, 64)
	sum := strings.ToLower(s.Opaque["sha1"])

	var cb callbackResult
	if err := decodeEnvelope(resp.Bytes, &cb); err != nil {
		// The bytes are on the server either way; surface the mapped error so
		// a risk-control answer here still trips the breaker.
		return provider.Entry{}, fmt.Errorf("pan115: upload callback: %w", err)
	}
	id := string(cb.FileID)
	if id == "" {
		// UNVERIFIED: the callback body is configured by 115 and may omit
		// file_id. Looking the child up is slower but never invents an id.
		return p.findChild(ctx, parentID, firstNonEmpty(cb.FileName, name))
	}
	p.rememberPickCode(id, firstNonEmpty(cb.PickCode, s.Opaque["pick_code"]))
	e := provider.Entry{
		ID:       id,
		ParentID: firstNonEmpty(string(cb.CID), parentID),
		Name:     firstNonEmpty(cb.FileName, name),
		Kind:     provider.KindFile,
		Size:     size,
		ModTime:  time.Now().UTC(),
	}
	if int64(cb.FileSize) > 0 {
		e.Size = int64(cb.FileSize)
	}
	if h := strings.ToLower(firstNonEmpty(cb.SHA1, sum)); h != "" {
		e.Hashes = provider.Hashes{provider.HashSHA1: h}
		e.Version = h
	}
	return e, nil
}
