package tianyi

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

// Opaque keys carried in the UploadSession. They are all strings because the
// journal persists them verbatim, and UploadPart/CompleteUpload read the
// session back from the journal after a restart: nothing about an in-flight
// upload may live only in memory.
const (
	optUploadFileID = "upload_file_id"
	optParentID     = "parent_id"
	optName         = "name"
	optSize         = "size"
	optFileMD5      = "file_md5"
	optSliceMD5     = "slice_md5"
	optLazyCheck    = "lazy_check"
	optPartSize     = "part_size"
)

// initUploadResp is the initMultiUpload payload. The upload host answers in
// XML, so this is decoded with httpx.Client.XML.
//
// UNVERIFIED: the XML element names and whether the payload nests its fields
// under <data>. The JSON form of this endpoint is documented by community
// clients as {"code":"SUCCESS","data":{"uploadFileId":…,"fileDataExists":…}};
// this struct accepts both the nested and the flat spelling so a mismatch
// degrades to "no rapid upload" rather than to a bad session. No XMLName is
// declared, so the root element name does not matter.
type initUploadResp struct {
	envelope
	Data struct {
		UploadFileID   looseString `xml:"uploadFileId"`
		FileDataExists int         `xml:"fileDataExists"`
		UploadType     int         `xml:"uploadType"`
		UploadHost     string      `xml:"uploadHost"`
	} `xml:"data"`
	FlatUploadFileID   looseString `xml:"uploadFileId"`
	FlatFileDataExists int         `xml:"fileDataExists"`
}

func (r *initUploadResp) uploadFileID() string {
	return firstNonEmpty(r.Data.UploadFileID.String(), r.FlatUploadFileID.String())
}

func (r *initUploadResp) rapidHit() bool {
	return r.Data.FileDataExists == 1 || r.FlatFileDataExists == 1
}

// uploadURLsResp is the getMultiUploadUrls payload. The per-part entries are
// keyed by element name ("partNumber_1"), which no static struct can express,
// so they are collected with `,any` and matched by name afterwards.
type uploadURLsResp struct {
	envelope
	UploadURLs struct {
		Parts []uploadPartURL `xml:",any"`
	} `xml:"uploadUrls"`
}

// uploadPartURL is one signed part destination.
type uploadPartURL struct {
	XMLName       xml.Name
	RequestURL    string `xml:"requestURL"`
	RequestHeader string `xml:"requestHeader"`
}

// commitResp is the commitMultiUploadFile payload.
type commitResp struct {
	envelope
	File struct {
		UserFileID looseString `xml:"userFileId"`
		FileID     looseString `xml:"fileId"`
		ID         looseString `xml:"id"`
		FileName   string      `xml:"fileName"`
		FileSize   int64       `xml:"fileSize"`
		FileMD5    string      `xml:"fileMd5"`
		MD5        string      `xml:"md5"`
		CreateDate string      `xml:"createDate"`
	} `xml:"file"`
}

func (r *commitResp) fileID() string {
	return firstNonEmpty(r.File.UserFileID.String(), r.File.FileID.String(), r.File.ID.String())
}

// BeginUpload starts an upload, attempting an MD5 秒传 (rapid upload) first
// whenever the caller supplied a whole-file MD5.
//
// 189 answers the rapid-upload question inside initMultiUpload: fileDataExists
// == 1 means the server already holds this content and only needs the commit
// call. When it is 0 the very same uploadFileId is used for the chunked path,
// so the optimistic attempt costs no extra round trip.
//
// Slice hashes: for a file that fits in one slice the slice MD5 equals the file
// MD5, which is what is sent. Larger files would need the MD5 of every slice,
// which this driver cannot compute from a hash map, so they are sent with
// lazyCheck=1 and only the whole-file MD5.
//
// UNVERIFIED: that sliceMd5 == fileMd5 for a single-slice file, and the exact
// meaning of lazyCheck. Both are how community clients use the endpoint. If
// either is wrong the server answers fileDataExists=0 and the chunked path
// runs, so a wrong guess costs bandwidth, never correctness.
func (t *Tianyi) BeginUpload(ctx context.Context, parentID, name string, size int64, h provider.Hashes) (provider.UploadSession, error) {
	if parentID == "" {
		parentID = t.rootID
	}
	if name == "" {
		return provider.UploadSession{}, fmt.Errorf("tianyi: BeginUpload needs a file name")
	}
	if size < 0 {
		return provider.UploadSession{}, fmt.Errorf("tianyi: negative upload size %d", size)
	}
	partSize := t.caps.PartSize
	fileMD5 := strings.ToUpper(strings.TrimSpace(h[provider.HashMD5]))
	sliceMD5 := ""
	lazyCheck := "1"
	if fileMD5 != "" && size <= partSize {
		sliceMD5 = fileMD5
		lazyCheck = "0"
	}

	q := url.Values{}
	q.Set("parentFolderId", parentID)
	q.Set("fileName", name)
	q.Set("fileSize", strconv.FormatInt(size, 10))
	q.Set("sliceSize", strconv.FormatInt(partSize, 10))
	q.Set("lazyCheck", lazyCheck)
	if fileMD5 != "" {
		q.Set("fileMd5", fileMD5)
	}
	if sliceMD5 != "" {
		q.Set("sliceMd5", sliceMD5)
	}

	var out initUploadResp
	if err := t.getXML(ctx, apiURL(t.upload, "/person/initMultiUpload", q), ratelimit.Upload, &out); err != nil {
		return provider.UploadSession{}, err
	}
	uploadFileID := out.uploadFileID()
	if uploadFileID == "" {
		return provider.UploadSession{}, fmt.Errorf("tianyi: initMultiUpload returned no uploadFileId")
	}
	sess := provider.UploadSession{
		ID:       uploadFileID,
		PartSize: partSize,
		Opaque: map[string]string{
			optUploadFileID: uploadFileID,
			optParentID:     parentID,
			optName:         name,
			optSize:         strconv.FormatInt(size, 10),
			optFileMD5:      fileMD5,
			optSliceMD5:     sliceMD5,
			optLazyCheck:    lazyCheck,
			optPartSize:     strconv.FormatInt(partSize, 10),
		},
	}
	if fileMD5 != "" && out.rapidHit() {
		e, err := t.commit(ctx, sess)
		if err != nil {
			return provider.UploadSession{}, fmt.Errorf("tianyi: commit rapid upload: %w", err)
		}
		sess.RapidDone = true
		sess.Entry = &e
	}
	return sess, nil
}

// parseUploadHeader splits the "&"-joined k=v header list the upload host
// returns alongside a part URL. Values may themselves contain ':' (an OSS
// Authorization is "OSS <key>:<signature>"), so the split is on the first '='
// and only falls back to ':' for a pair that has no '=' at all.
func parseUploadHeader(s string) map[string]string {
	out := map[string]string{}
	if s == "" {
		return out
	}
	if unescaped, err := url.QueryUnescape(s); err == nil {
		s = unescaped
	}
	for _, pair := range strings.Split(s, "&") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		sep := strings.Index(pair, "=")
		if sep <= 0 {
			sep = strings.Index(pair, ":")
		}
		if sep <= 0 {
			continue
		}
		out[strings.TrimSpace(pair[:sep])] = strings.TrimSpace(pair[sep+1:])
	}
	return out
}

// UploadPart uploads one slice. idx is 0-based (the uploader's convention);
// 189 numbers parts from 1, so the wire value is idx+1.
//
// Each part needs its own signed destination, obtained from getMultiUploadUrls
// with the part's MD5, so the server can reject a corrupted slice on arrival
// rather than at commit time.
func (t *Tianyi) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	uploadFileID := sessionValue(s, optUploadFileID)
	if uploadFileID == "" {
		return provider.PartToken{}, fmt.Errorf("tianyi: upload session has no %s", optUploadFileID)
	}
	if idx < 0 {
		return provider.PartToken{}, fmt.Errorf("tianyi: negative part index %d", idx)
	}
	data, err := io.ReadAll(io.LimitReader(r, n))
	if err != nil {
		return provider.PartToken{}, err
	}
	if int64(len(data)) != n {
		return provider.PartToken{}, fmt.Errorf("tianyi: part %d is %d bytes, want %d", idx, len(data), n)
	}
	sum := md5.Sum(data)
	partNumber := idx + 1

	q := url.Values{}
	q.Set("uploadFileId", uploadFileID)
	// partInfo is "<partNumber>-<base64 of the raw MD5 digest>".
	q.Set("partInfo", fmt.Sprintf("%d-%s", partNumber, base64.StdEncoding.EncodeToString(sum[:])))

	var urls uploadURLsResp
	if err := t.getXML(ctx, apiURL(t.upload, "/person/getMultiUploadUrls", q), ratelimit.Upload, &urls); err != nil {
		return provider.PartToken{}, err
	}
	part, err := pickPartURL(urls.UploadURLs.Parts, partNumber)
	if err != nil {
		return provider.PartToken{}, err
	}

	hdr := http.Header{}
	for k, v := range parseUploadHeader(part.RequestHeader) {
		hdr.Set(k, v)
	}
	// The part URL is signed by the upload host; it must not carry the 189
	// session signature, so this request bypasses Tianyi.call.
	resp, err := t.cli.Do(ctx, httpx.Request{
		Method:  http.MethodPut,
		URL:     part.RequestURL,
		Class:   ratelimit.Upload,
		Header:  hdr,
		Body:    bytes.NewReader(data),
		GetBody: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
	})
	if err != nil {
		return provider.PartToken{}, err
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if etag == "" {
		// 189 assembles by uploadFileId + partNumber, so a missing ETag is not
		// fatal; the local digest keeps the journal's part record meaningful.
		etag = hex.EncodeToString(sum[:])
	}
	return provider.PartToken{Index: idx, ETag: etag}, nil
}

func pickPartURL(parts []uploadPartURL, partNumber int) (uploadPartURL, error) {
	want := fmt.Sprintf("partNumber_%d", partNumber)
	for _, p := range parts {
		if p.XMLName.Local == want {
			return p, nil
		}
	}
	// Only one destination was returned and it is unnamed or named
	// differently: it can only be the part that was asked for.
	if len(parts) == 1 && parts[0].RequestURL != "" {
		return parts[0], nil
	}
	return uploadPartURL{}, fmt.Errorf("tianyi: getMultiUploadUrls returned no url for %s", want)
}

// CompleteUpload commits the upload and returns the resulting entry.
//
// The part tokens are not sent: 189 assembles the file server-side from the
// uploadFileId and the part numbers, so the ETags exist only for the journal's
// resume bookkeeping. They are still checked for obvious gaps before the
// commit, because committing a short upload would publish a truncated file.
func (t *Tianyi) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	size, _ := strconv.ParseInt(sessionValue(s, optSize), 10, 64)
	partSize, _ := strconv.ParseInt(sessionValue(s, optPartSize), 10, 64)
	if partSize > 0 && size > 0 {
		want := int((size + partSize - 1) / partSize)
		if len(parts) != want {
			return provider.Entry{}, fmt.Errorf("tianyi: upload has %d parts, want %d for %d bytes",
				len(parts), want, size)
		}
	}
	return t.commit(ctx, s)
}

func (t *Tianyi) commit(ctx context.Context, s provider.UploadSession) (provider.Entry, error) {
	uploadFileID := sessionValue(s, optUploadFileID)
	if uploadFileID == "" {
		return provider.Entry{}, fmt.Errorf("tianyi: upload session has no %s", optUploadFileID)
	}
	q := url.Values{}
	q.Set("uploadFileId", uploadFileID)
	q.Set("lazyCheck", firstNonEmpty(sessionValue(s, optLazyCheck), "1"))
	// opertype 3 means "overwrite an existing file of the same name".
	//
	// UNVERIFIED: the opertype enum. 3 is what community clients send for a
	// normal upload.
	q.Set("opertype", "3")
	if v := sessionValue(s, optFileMD5); v != "" {
		q.Set("fileMd5", v)
	}
	if v := sessionValue(s, optSliceMD5); v != "" {
		q.Set("sliceMd5", v)
	}

	var out commitResp
	if err := t.getXML(ctx, apiURL(t.upload, "/person/commitMultiUploadFile", q), ratelimit.Upload, &out); err != nil {
		return provider.Entry{}, err
	}
	id := out.fileID()
	if id == "" {
		return provider.Entry{}, fmt.Errorf("tianyi: commitMultiUploadFile returned no file id")
	}
	size, _ := strconv.ParseInt(sessionValue(s, optSize), 10, 64)
	e := provider.Entry{
		ID:       id,
		ParentID: sessionValue(s, optParentID),
		Name:     firstNonEmpty(out.File.FileName, sessionValue(s, optName)),
		Kind:     provider.KindFile,
		Size:     max64(out.File.FileSize, size),
		ModTime:  parseTime(out.File.CreateDate),
	}
	if e.ModTime.IsZero() {
		e.ModTime = time.Now()
	}
	if sum := strings.ToLower(firstNonEmpty(out.File.FileMD5, out.File.MD5, sessionValue(s, optFileMD5))); sum != "" {
		e.Hashes = provider.Hashes{provider.HashMD5: sum}
		e.Version = sum
	}
	// A file committed without an MD5 still needs a change token: the block
	// cache keys on it, and an empty one would alias two different contents.
	provider.EnsureVersion(&e)
	t.rememberKinds([]provider.Entry{e})
	return e, nil
}

func sessionValue(s provider.UploadSession, key string) string {
	if s.Opaque != nil {
		if v := s.Opaque[key]; v != "" {
			return v
		}
	}
	if key == optUploadFileID {
		return s.ID
	}
	return ""
}
