package quark

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// Opaque keys used to persist an upload session in the journal. UploadPart and
// CompleteUpload read only from these, so a session rebuilt after a restart
// works exactly like a fresh one.
const (
	OpaqueTaskID       = "task_id"
	OpaqueUploadID     = "upload_id"
	OpaqueObjKey       = "obj_key"
	OpaqueBucket       = "bucket"
	OpaqueUploadURL    = "upload_url"
	OpaqueAuthInfo     = "auth_info"
	OpaqueCallbackURL  = "callback_url"
	OpaqueCallbackBody = "callback_body"
	OpaqueParentID     = "pdir_fid"
	OpaqueFileName     = "file_name"
	OpaqueMimeType     = "format_type"
	OpaqueSize         = "size"
)

// ossUserAgent is the x-oss-user-agent value the Quark web client sends. It is
// part of the string the server signs, so it cannot be changed independently.
//
// UNVERIFIED: this exact value, and the fact that it participates in the
// signature. If part uploads start failing with SignatureDoesNotMatch this is
// the first thing to re-capture from the browser.
const ossUserAgent = "aliyun-sdk-js/6.6.1 Chrome 98.0.4758.80 on Windows 10 64-bit"

// partContentType is the Content-Type sent with every part; it also appears in
// the signed canonical string.
const partContentType = "application/octet-stream"

// preUploadData is the reply to file/upload/pre.
type preUploadData struct {
	TaskID    string `json:"task_id"`
	Finish    bool   `json:"finish"`
	UploadID  string `json:"upload_id"`
	ObjKey    string `json:"obj_key"`
	Bucket    string `json:"bucket"`
	UploadURL string `json:"upload_url"`
	AuthInfo  string `json:"auth_info"`
	Fid       string `json:"fid"`
	// PartSize is honoured when the server supplies one; OSS rejects a
	// multipart upload whose parts do not match what it expects.
	PartSize int64 `json:"part_size"`
	Callback struct {
		CallbackURL  string `json:"callbackUrl"`
		CallbackBody string `json:"callbackBody"`
	} `json:"callback"`
}

// BeginUpload starts an upload of name under parentID.
//
// Quark's rapid-upload ("秒传") handshake takes MD5 and SHA1 together, so a
// hash-only upload is attempted only when the caller supplied both. When
// either is missing the driver goes straight to the chunked path rather than
// sending a half-filled hash request, which the service rejects.
func (q *Quark) BeginUpload(ctx context.Context, parentID, name string, size int64, h provider.Hashes) (provider.UploadSession, error) {
	if name == "" {
		return provider.UploadSession{}, fmt.Errorf("quark: BeginUpload requires a file name")
	}
	parent := q.dirOrRoot(parentID)
	now := time.Now().UnixMilli()
	body := map[string]any{
		"ccp_hash_update": true,
		"dir_name":        "",
		"file_name":       name,
		"format_type":     mimeTypeOf(name),
		"l_created_at":    now,
		"l_updated_at":    now,
		"pdir_fid":        parent,
		"size":            size,
	}
	var pre preUploadData
	if _, err := q.postJSON(ctx, q.apiURL("/file/upload/pre", nil), ratelimit.Upload, body, &pre); err != nil {
		return provider.UploadSession{}, err
	}
	if pre.TaskID == "" {
		return provider.UploadSession{}, fmt.Errorf("quark: upload pre for %q returned no task_id", name)
	}

	partSize := pre.PartSize
	if partSize <= 0 {
		partSize = defaultPartSize
	}
	opaque := map[string]string{
		OpaqueTaskID:       pre.TaskID,
		OpaqueUploadID:     pre.UploadID,
		OpaqueObjKey:       pre.ObjKey,
		OpaqueBucket:       pre.Bucket,
		OpaqueUploadURL:    pre.UploadURL,
		OpaqueAuthInfo:     pre.AuthInfo,
		OpaqueCallbackURL:  pre.Callback.CallbackURL,
		OpaqueCallbackBody: pre.Callback.CallbackBody,
		OpaqueParentID:     parent,
		OpaqueFileName:     name,
		OpaqueMimeType:     mimeTypeOf(name),
		OpaqueSize:         strconv.FormatInt(size, 10),
	}

	// The server occasionally settles the upload during pre when it already
	// holds the object.
	if pre.Finish {
		e, err := q.resolveUploaded(ctx, pre.Fid, parent, name, size, h)
		if err != nil {
			return provider.UploadSession{}, err
		}
		return provider.UploadSession{ID: pre.TaskID, PartSize: partSize, RapidDone: true, Entry: &e, Opaque: opaque}, nil
	}

	md5sum := h[provider.HashMD5]
	sha1sum := h[provider.HashSHA1]
	if md5sum != "" && sha1sum != "" {
		var hashData struct {
			Finish bool   `json:"finish"`
			Fid    string `json:"fid"`
		}
		hashBody := map[string]any{
			"task_id": pre.TaskID,
			"md5":     md5sum,
			"sha1":    sha1sum,
		}
		if _, err := q.postJSON(ctx, q.apiURL("/file/update/hash", nil), ratelimit.Upload, hashBody, &hashData); err != nil {
			return provider.UploadSession{}, err
		}
		if hashData.Finish {
			e, err := q.resolveUploaded(ctx, hashData.Fid, parent, name, size, h)
			if err != nil {
				return provider.UploadSession{}, err
			}
			return provider.UploadSession{ID: pre.TaskID, PartSize: partSize, RapidDone: true, Entry: &e, Opaque: opaque}, nil
		}
	}
	return provider.UploadSession{ID: pre.TaskID, PartSize: partSize, Opaque: opaque}, nil
}

// ossURL builds the address of an OSS object from an upload session.
//
// Quark answers file/upload/pre with the bare zone host ("http://pds.quark.cn")
// and names the bucket in a separate field. That host has no address of its
// own: OSS is virtual-hosted here, so only <bucket>.<zone> resolves, and
// joining the object key onto the zone alone produces a name that fails DNS
// before a single byte is sent. The scheme is forced to https because the
// field arrives as plain http and these requests carry the file's contents.
//
// UNVERIFIED: that <bucket>.<zone> is the name that resolves. The bare zone
// failing DNS is observed ("lookup pds.quark.cn: no such host" on every part
// upload); the subdomain form is inferred from OSS's virtual-hosted style and
// from the canonical resource Quark signs, which already carries the bucket.
// Confirm by completing one real chunked upload.
func ossURL(uploadURL, bucket, objKey string) string {
	host := uploadURL
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+len("://"):]
	}
	if bucket != "" && !strings.HasPrefix(host, bucket+".") {
		host = bucket + "." + host
	}
	return urlJoin("https://"+host, escapePath(objKey))
}

// UploadPart uploads one part. idx is zero-based; OSS part numbers start at 1.
//
// Each part needs its own signature, which only Quark can produce: the driver
// never sees the OSS secret, it sends the canonical string to
// file/upload/auth and gets back a ready-made Authorization value.
func (q *Quark) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	if err := requireOpaque(s, OpaqueUploadURL, OpaqueObjKey, OpaqueUploadID, OpaqueBucket); err != nil {
		return provider.PartToken{}, err
	}
	data, err := io.ReadAll(io.LimitReader(r, n))
	if err != nil {
		return provider.PartToken{}, fmt.Errorf("quark: read part %d: %w", idx, err)
	}
	if int64(len(data)) != n {
		return provider.PartToken{}, fmt.Errorf("quark: part %d short read: got %d bytes, want %d", idx, len(data), n)
	}

	partNumber := idx + 1
	query := fmt.Sprintf("?partNumber=%d&uploadId=%s", partNumber, s.Opaque[OpaqueUploadID])
	date := ossDate()
	// UNVERIFIED: the canonical string layout. This is the OSS v1 signature
	// input as the Quark web client builds it (verb, empty Content-MD5,
	// content type, date, the two x-oss-* headers in lexical order, then the
	// canonicalised resource). If the service starts rejecting parts, compare
	// this string against a captured browser request byte for byte.
	authMeta := strings.Join([]string{
		http.MethodPut,
		"",
		partContentType,
		date,
		"x-oss-date:" + date,
		"x-oss-user-agent:" + ossUserAgent,
		"/" + s.Opaque[OpaqueBucket] + "/" + s.Opaque[OpaqueObjKey] + query,
	}, "\n")

	authKey, err := q.uploadAuth(ctx, s, authMeta)
	if err != nil {
		return provider.PartToken{}, err
	}

	header := http.Header{}
	header.Set("Authorization", authKey)
	header.Set("Content-Type", partContentType)
	header.Set("Date", date)
	header.Set("x-oss-date", date)
	header.Set("x-oss-user-agent", ossUserAgent)

	// Deliberately not q.requestHeader: this request goes to Alibaba OSS, not
	// to Quark, and the account cookie must never leave the drive host.
	resp, err := q.cli.Do(ctx, httpx.Request{
		Method:  http.MethodPut,
		URL:     ossURL(s.Opaque[OpaqueUploadURL], s.Opaque[OpaqueBucket], s.Opaque[OpaqueObjKey]) + query,
		Class:   ratelimit.Upload,
		Header:  header,
		Body:    bytes.NewReader(data),
		GetBody: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
	})
	if err != nil {
		return provider.PartToken{}, err
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return provider.PartToken{}, fmt.Errorf("quark: part %d upload returned no ETag", partNumber)
	}
	return provider.PartToken{Index: idx, ETag: etag}, nil
}

// completeXML is the OSS CompleteMultipartUpload request body.
type completeXML struct {
	XMLName xml.Name         `xml:"CompleteMultipartUpload"`
	Parts   []completeXMLPar `xml:"Part"`
}

type completeXMLPar struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

// CompleteUpload commits a chunked upload.
//
// Two commits are involved: the OSS CompleteMultipartUpload, whose callback
// tells Quark the object landed, and Quark's own file/upload/finish. Both are
// needed — without the callback the drive never learns about the object, and
// without finish the task stays open.
func (q *Quark) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	if err := requireOpaque(s, OpaqueUploadURL, OpaqueObjKey, OpaqueUploadID, OpaqueBucket, OpaqueTaskID); err != nil {
		return provider.Entry{}, err
	}
	ordered := append([]provider.PartToken(nil), parts...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })
	doc := completeXML{}
	for _, p := range ordered {
		doc.Parts = append(doc.Parts, completeXMLPar{PartNumber: p.Index + 1, ETag: quoteETag(p.ETag)})
	}
	bodyXML, err := xml.Marshal(doc)
	if err != nil {
		return provider.Entry{}, fmt.Errorf("quark: build complete body: %w", err)
	}

	sum := md5.Sum(bodyXML)
	contentMD5 := base64.StdEncoding.EncodeToString(sum[:])
	date := ossDate()
	callback, err := json.Marshal(map[string]string{
		"callbackUrl":  s.Opaque[OpaqueCallbackURL],
		"callbackBody": s.Opaque[OpaqueCallbackBody],
	})
	if err != nil {
		return provider.Entry{}, fmt.Errorf("quark: build callback: %w", err)
	}
	callbackB64 := base64.StdEncoding.EncodeToString(callback)
	query := "?uploadId=" + s.Opaque[OpaqueUploadID]

	// UNVERIFIED: as with UploadPart, the exact canonical string. The
	// x-oss-callback header is what makes Quark register the finished object,
	// and it must appear both as a header and inside the signed string.
	authMeta := strings.Join([]string{
		http.MethodPost,
		contentMD5,
		"application/xml",
		date,
		"x-oss-callback:" + callbackB64,
		"x-oss-date:" + date,
		"x-oss-user-agent:" + ossUserAgent,
		"/" + s.Opaque[OpaqueBucket] + "/" + s.Opaque[OpaqueObjKey] + query,
	}, "\n")
	authKey, err := q.uploadAuth(ctx, s, authMeta)
	if err != nil {
		return provider.Entry{}, err
	}

	header := http.Header{}
	header.Set("Authorization", authKey)
	header.Set("Content-Type", "application/xml")
	header.Set("Content-MD5", contentMD5)
	header.Set("Date", date)
	header.Set("x-oss-date", date)
	header.Set("x-oss-user-agent", ossUserAgent)
	header.Set("x-oss-callback", callbackB64)

	resp, err := q.cli.Do(ctx, httpx.Request{
		Method:  http.MethodPost,
		URL:     ossURL(s.Opaque[OpaqueUploadURL], s.Opaque[OpaqueBucket], s.Opaque[OpaqueObjKey]) + query,
		Class:   ratelimit.Upload,
		Header:  header,
		Body:    bytes.NewReader(bodyXML),
		GetBody: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(bodyXML)), nil },
	})
	if err != nil {
		return provider.Entry{}, err
	}
	// OSS returns the callback's own reply, which is a Quark envelope naming
	// the created file. It is best-effort: the finish call below is the
	// authoritative step.
	fid := fidFromCallback(resp.Bytes)

	var finish struct {
		Finish bool   `json:"finish"`
		Fid    string `json:"fid"`
	}
	finishBody := map[string]any{
		"obj_key": s.Opaque[OpaqueObjKey],
		"task_id": s.Opaque[OpaqueTaskID],
	}
	if _, err := q.postJSON(ctx, q.apiURL("/file/upload/finish", nil), ratelimit.Upload, finishBody, &finish); err != nil {
		return provider.Entry{}, err
	}
	if fid == "" {
		fid = finish.Fid
	}
	size, _ := strconv.ParseInt(s.Opaque[OpaqueSize], 10, 64)
	return q.resolveUploaded(ctx, fid, s.Opaque[OpaqueParentID], s.Opaque[OpaqueFileName], size, nil)
}

// uploadAuth exchanges a canonical string for an OSS Authorization value.
func (q *Quark) uploadAuth(ctx context.Context, s provider.UploadSession, authMeta string) (string, error) {
	body := map[string]any{
		"auth_info": s.Opaque[OpaqueAuthInfo],
		"auth_meta": authMeta,
		"task_id":   s.Opaque[OpaqueTaskID],
	}
	var data struct {
		AuthKey string `json:"auth_key"`
	}
	if _, err := q.postJSON(ctx, q.apiURL("/file/upload/auth", nil), ratelimit.Upload, body, &data); err != nil {
		return "", err
	}
	if data.AuthKey == "" {
		return "", fmt.Errorf("quark: upload auth returned no auth_key")
	}
	return data.AuthKey, nil
}

// resolveUploaded turns whatever the commit path produced into a real Entry.
//
// It never fabricates an id: when the service did not name one, the parent
// directory is searched for the file by name. Only when that also fails does
// it report an error, because an Entry with no id would poison the metadata
// store far worse than a visible failure.
func (q *Quark) resolveUploaded(ctx context.Context, fid, parentID, name string, size int64, h provider.Hashes) (provider.Entry, error) {
	if fid != "" {
		if e, err := q.Stat(ctx, fid); err == nil && e.ID != "" {
			return e, nil
		}
		// Stat's shape is unverified; a failure there must not undo a
		// successful upload, and the id is known to be good.
		e := provider.Entry{
			ID: fid, ParentID: parentID, Name: name,
			Kind: provider.KindFile, Size: size, ModTime: time.Now(),
		}
		applyHashes(&e, h)
		// This entry becomes the block cache key for the file just uploaded,
		// so it must carry a version even when no hash was available.
		provider.EnsureVersion(&e)
		return e, nil
	}
	if e, ok, err := q.findInDir(ctx, parentID, name); err != nil {
		return provider.Entry{}, err
	} else if ok {
		return e, nil
	}
	return provider.Entry{}, fmt.Errorf("quark: upload of %q committed but the resulting file id could not be resolved", name)
}

func applyHashes(e *provider.Entry, h provider.Hashes) {
	if len(h) == 0 {
		return
	}
	e.Hashes = provider.Hashes{}
	for k, v := range h {
		e.Hashes[k] = v
	}
	if v := h[provider.HashMD5]; v != "" && e.Version == "" {
		e.Version = v
	}
}

// findInDirPages bounds the directory scan used as the last resort for
// resolving a freshly uploaded file.
const findInDirPages = 3

func (q *Quark) findInDir(ctx context.Context, dirID, name string) (provider.Entry, bool, error) {
	cursor := ""
	for i := 0; i < findInDirPages; i++ {
		entries, next, err := q.List(ctx, dirID, cursor)
		if err != nil {
			return provider.Entry{}, false, err
		}
		for _, e := range entries {
			if e.Name == name {
				return e, true, nil
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return provider.Entry{}, false, nil
}

// fidFromCallback pulls the created file id out of the OSS callback reply.
//
// UNVERIFIED: the callback body's shape. It is parsed defensively — anything
// unrecognised yields an empty id, which sends the caller down the
// finish/listing path instead of producing a wrong one.
func fidFromCallback(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil || !env.ok() {
		return ""
	}
	var data struct {
		Fid string `json:"fid"`
	}
	if err := env.into(&data); err != nil {
		return ""
	}
	return data.Fid
}

func requireOpaque(s provider.UploadSession, keys ...string) error {
	for _, k := range keys {
		if s.Opaque[k] == "" {
			return fmt.Errorf("quark: upload session %q is missing %q: %w", s.ID, k, provider.ErrNotFound)
		}
	}
	return nil
}

// quoteETag normalises an ETag to the quoted form OSS expects in the complete
// document. OSS returns it quoted, but a session replayed from the journal may
// have lost the quotes.
func quoteETag(etag string) string {
	if strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`) && len(etag) >= 2 {
		return etag
	}
	return `"` + strings.Trim(etag, `"`) + `"`
}

// ossDate formats the GMT date OSS signs.
func ossDate() string { return time.Now().UTC().Format(http.TimeFormat) }

// mimeTypeOf guesses the format_type Quark stores alongside the file. It is
// cosmetic: a wrong guess changes the icon the web UI shows, nothing more.
func mimeTypeOf(name string) string {
	if t := mime.TypeByExtension(strings.ToLower(path.Ext(name))); t != "" {
		return t
	}
	return "application/octet-stream"
}
