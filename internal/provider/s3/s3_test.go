package s3

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
	"cloudfs/internal/vfs"

	"github.com/minio/minio-go/v7"
)

type fakeS3 struct {
	mu             sync.Mutex
	objects        map[string][]byte
	parts          map[string]map[int][]byte
	nextID         int
	badAuth        bool
	lastCopySource string
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, parts: map[string]map[int][]byte{}}
}

func (s *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test-access/") {
		s.mu.Lock()
		s.badAuth = true
		s.mu.Unlock()
		s3Error(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	if r.URL.Path == "/bucket" || r.URL.Path == "/bucket/" {
		if _, ok := r.URL.Query()["location"]; ok {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
			return
		}
		if r.Method == http.MethodGet {
			s.list(w, r)
			return
		}
	}
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	switch r.Method {
	case http.MethodHead:
		s.head(w, key)
	case http.MethodGet:
		s.get(w, r, key)
	case http.MethodPut:
		s.put(w, r, key)
	case http.MethodPost:
		s.post(w, r, key)
	case http.MethodDelete:
		s.mu.Lock()
		delete(s.objects, key)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

type listBucketResult struct {
	XMLName        xml.Name       `xml:"ListBucketResult"`
	Xmlns          string         `xml:"xmlns,attr"`
	Name           string         `xml:"Name"`
	Prefix         string         `xml:"Prefix"`
	Delimiter      string         `xml:"Delimiter,omitempty"`
	MaxKeys        int            `xml:"MaxKeys"`
	IsTruncated    bool           `xml:"IsTruncated"`
	Contents       []listContents `xml:"Contents"`
	CommonPrefixes []commonPrefix `xml:"CommonPrefixes"`
}

type listContents struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

func (s *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix, delimiter := r.URL.Query().Get("prefix"), r.URL.Query().Get("delimiter")
	s.mu.Lock()
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	seenPrefix := map[string]bool{}
	result := listBucketResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Name: "bucket", Prefix: prefix, Delimiter: delimiter, MaxKeys: 1000}
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		if delimiter != "" {
			if i := strings.Index(rest, delimiter); i >= 0 {
				common := prefix + rest[:i+1]
				if !seenPrefix[common] {
					seenPrefix[common] = true
					result.CommonPrefixes = append(result.CommonPrefixes, commonPrefix{Prefix: common})
				}
				continue
			}
		}
		body := s.objects[key]
		result.Contents = append(result.Contents, listContents{Key: key, LastModified: "2026-09-05T00:00:00.000Z", ETag: `"` + etag(body) + `"`, Size: int64(len(body)), StorageClass: "STANDARD"})
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
}

func (s *fakeS3) head(w http.ResponseWriter, key string) {
	s.mu.Lock()
	body, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		s3Error(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	w.Header().Set("ETag", `"`+etag(body)+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Last-Modified", time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

func (s *fakeS3) get(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	body, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		s3Error(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	w.Header().Set("ETag", `"`+etag(body)+`"`)
	w.Header().Set("Last-Modified", time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if match := strings.Trim(r.Header.Get("If-Match"), `"`); match != "" && match != etag(body) {
		s3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
		return
	}
	start, end := 0, len(body)-1
	if raw := r.Header.Get("Range"); raw != "" {
		if strings.HasSuffix(raw, "-") {
			_, _ = fmt.Sscanf(raw, "bytes=%d-", &start)
		} else {
			_, _ = fmt.Sscanf(raw, "bytes=%d-%d", &start, &end)
		}
		if start < 0 || start >= len(body) || end < start {
			s3Error(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
			return
		}
		if end >= len(body) {
			end = len(body) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	_, _ = w.Write(body[start : end+1])
}

func (s *fakeS3) put(w http.ResponseWriter, r *http.Request, key string) {
	if source := r.Header.Get("X-Amz-Copy-Source"); source != "" {
		s.mu.Lock()
		s.lastCopySource = source
		s.mu.Unlock()
		source, _ = url.PathUnescape(strings.TrimPrefix(strings.TrimPrefix(source, "/"), "bucket/"))
		s.mu.Lock()
		body, ok := s.objects[source]
		if ok {
			s.objects[key] = bytes.Clone(body)
		}
		s.mu.Unlock()
		if !ok {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		_, _ = fmt.Fprintf(w, `<CopyObjectResult><LastModified>2026-09-05T00:00:00.000Z</LastModified><ETag>"%s"</ETag></CopyObjectResult>`, etag(body))
		return
	}
	body, _ := readS3Body(r)
	if uploadID := r.URL.Query().Get("uploadId"); uploadID != "" {
		part, _ := strconv.Atoi(r.URL.Query().Get("partNumber"))
		s.mu.Lock()
		if s.parts[uploadID] == nil {
			s.mu.Unlock()
			s3Error(w, http.StatusNotFound, "NoSuchUpload")
			return
		}
		s.parts[uploadID][part] = body
		s.mu.Unlock()
		w.Header().Set("ETag", `"`+etag(body)+`"`)
		return
	}
	s.mu.Lock()
	s.objects[key] = body
	s.mu.Unlock()
	w.Header().Set("ETag", `"`+etag(body)+`"`)
}

func readS3Body(r *http.Request) ([]byte, error) {
	if !strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
		return io.ReadAll(r.Body)
	}
	reader := bufio.NewReader(r.Body)
	var out []byte
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeText := strings.Split(strings.TrimSpace(line), ";")[0]
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("bad aws chunk size %q", sizeText)
		}
		if size == 0 {
			return out, nil
		}
		start := len(out)
		out = append(out, make([]byte, size)...)
		if _, err := io.ReadFull(reader, out[start:]); err != nil {
			return nil, err
		}
		terminator := make([]byte, 2)
		if _, err := io.ReadFull(reader, terminator); err != nil {
			return nil, err
		}
		if string(terminator) != "\r\n" {
			return nil, errors.New("bad aws chunk terminator")
		}
	}
}

type completeRequest struct {
	Parts []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	} `xml:"Part"`
}

func (s *fakeS3) post(w http.ResponseWriter, r *http.Request, key string) {
	if _, ok := r.URL.Query()["uploads"]; ok {
		s.mu.Lock()
		s.nextID++
		id := fmt.Sprintf("upload-%d", s.nextID)
		s.parts[id] = map[int][]byte{}
		s.mu.Unlock()
		_, _ = fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, key, id)
		return
	}
	uploadID := r.URL.Query().Get("uploadId")
	var complete completeRequest
	if err := xml.NewDecoder(r.Body).Decode(&complete); err != nil {
		s3Error(w, http.StatusBadRequest, "MalformedXML")
		return
	}
	s.mu.Lock()
	parts, ok := s.parts[uploadID]
	if !ok {
		s.mu.Unlock()
		s3Error(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	var body []byte
	for _, part := range complete.Parts {
		body = append(body, parts[part.PartNumber]...)
	}
	s.objects[key] = body
	delete(s.parts, uploadID)
	s.mu.Unlock()
	_, _ = fmt.Fprintf(w, `<CompleteMultipartUploadResult><Location>/bucket/%s</Location><Bucket>bucket</Bucket><Key>%s</Key><ETag>"%s-2"</ETag></CompleteMultipartUploadResult>`, key, key, etag(body))
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<Error><Code>%s</Code><Message>test error</Message><RequestId>req</RequestId></Error>`, code)
}

func etag(body []byte) string {
	sum := md5.Sum(body)
	return hex.EncodeToString(sum[:])
}

func newTestProvider(t *testing.T, state *fakeS3) (*Provider, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(state)
	u, _ := url.Parse(server.URL)
	p, err := New(testOptions(u))
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return p, server
}

func testOptions(endpoint *url.URL) Options {
	return Options{
		Name: "archive", Endpoint: endpoint, Region: "us-east-1", Bucket: "bucket", Prefix: "tenant",
		AccessKey: "test-access", SecretKey: "test-secret", Lookup: minio.BucketLookupPath,
		PartSize: minimumPartSize, Transport: http.DefaultTransport,
	}
}

func TestListStatReadAndDownloadURL(t *testing.T) {
	state := newFakeS3()
	state.objects["tenant/root.txt"] = []byte("root")
	state.objects["tenant/docs/"] = nil
	state.objects["tenant/docs/movie.mkv"] = []byte("0123456789")
	p, server := newTestProvider(t, state)
	defer server.Close()
	ctx := context.Background()

	entries, _, err := p.List(ctx, RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "root.txt" || entries[1].Name != "docs" || entries[1].Kind != provider.KindDir {
		t.Fatalf("root listing = %+v", entries)
	}
	entries, _, err = p.List(ctx, "/docs/", "")
	if err != nil || len(entries) != 1 || entries[0].Name != "movie.mkv" {
		t.Fatalf("docs listing = %+v, %v", entries, err)
	}
	entry, err := p.Stat(ctx, "/docs/movie.mkv")
	if err != nil || entry.Size != 10 || entry.Version == "" || entry.Hashes[provider.HashMD5] == "" {
		t.Fatalf("stat = %+v, %v", entry, err)
	}
	rc, err := p.ReadRange(ctx, entry.ID, entry.Version, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "3456" {
		t.Fatalf("range = %q", got)
	}
	rc, err = p.ReadRange(ctx, entry.ID, entry.Version, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "0123456789" {
		t.Fatalf("full range = %q", got)
	}
	link, err := p.DownloadURL(ctx, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link.URL)
	if u.Query().Get("X-Amz-Signature") == "" || strings.Contains(link.URL, "test-secret") || !link.ExpiresAt.After(time.Now()) {
		t.Fatalf("bad presigned link: %s", link.URL)
	}
	state.mu.Lock()
	badAuth := state.badAuth
	state.mu.Unlock()
	if badAuth {
		t.Fatal("an SDK request was not signed")
	}
}

func TestUploadsCopyMoveRenameAndDelete(t *testing.T) {
	state := newFakeS3()
	p, server := newTestProvider(t, state)
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	restartOptions := testOptions(endpoint)
	restartOptions.Transport = server.Client().Transport
	ctx := context.Background()

	if _, err := p.Mkdir(ctx, RootID, "in"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PutFile(ctx, "/in/", "small.txt", strings.NewReader("small"), 5, nil); err != nil {
		t.Fatal(err)
	}
	s, err := p.BeginUpload(ctx, "/in/", "large.bin", 6, nil)
	if err != nil {
		t.Fatal(err)
	}
	part0, err := p.UploadPart(ctx, s, 0, strings.NewReader("abc"), 3)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(restartOptions)
	if err != nil {
		t.Fatal(err)
	}
	part1, err := restarted.UploadPart(ctx, s, 1, strings.NewReader("def"), 3)
	if err != nil {
		t.Fatal(err)
	}
	unordered := []provider.PartToken{part1, part0}
	large, err := restarted.CompleteUpload(ctx, s, unordered)
	if err != nil || large.Size != 6 {
		t.Fatalf("complete = %+v, %v", large, err)
	}
	if unordered[0].Index != 1 || unordered[1].Index != 0 {
		t.Fatal("CompleteUpload mutated the caller's token slice")
	}
	copyEntry, err := p.Copy(ctx, large.ID, RootID, "copy.bin")
	if err != nil || copyEntry.Size != 6 {
		state.mu.Lock()
		defer state.mu.Unlock()
		t.Fatalf("copy = %+v, %v; source=%q objects=%v", copyEntry, err, state.lastCopySource, state.objects)
	}
	renamed, err := p.Rename(ctx, copyEntry.ID, "renamed.bin")
	if err != nil || renamed.ID != "/renamed.bin" {
		t.Fatalf("rename = %+v, %v", renamed, err)
	}
	moved, err := p.Move(ctx, renamed.ID, "/in/")
	if err != nil || moved.ID != "/in/renamed.bin" {
		t.Fatalf("move = %+v, %v", moved, err)
	}
	if err := p.Delete(ctx, moved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stat(ctx, moved.ID); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("deleted object stat = %v", err)
	}
	dirMoved, err := p.Rename(ctx, "/in/", "archive")
	if err != nil || dirMoved.ID != "/archive/" {
		t.Fatalf("directory rename = %+v, %v", dirMoved, err)
	}
	if _, err := p.Stat(ctx, "/in/"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("old directory survived rename: %v", err)
	}
	rc, err := p.ReadRange(ctx, "/archive/large.bin", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	archived, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(archived) != "abcdef" {
		t.Fatalf("directory rename changed content: %q", archived)
	}
	if err := p.Delete(ctx, "/archive/"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stat(ctx, "/archive/"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("deleted directory stat = %v", err)
	}
	if err := p.Delete(ctx, RootID); err == nil {
		t.Fatal("provider root deletion was accepted")
	}
}

func TestValidationAndErrorMapping(t *testing.T) {
	state := newFakeS3()
	p, server := newTestProvider(t, state)
	defer server.Close()
	if _, _, err := p.List(context.Background(), "/a/../b", ""); err == nil {
		t.Fatal("non-canonical id was accepted")
	}
	if _, err := p.Mkdir(context.Background(), RootID, "../escape"); err == nil {
		t.Fatal("unsafe name was accepted")
	}
	if !errors.Is(mapError(minio.ErrorResponse{Code: "SlowDown", Message: "slow"}), provider.ErrRateLimited) {
		t.Fatal("SlowDown was not classified")
	}
}

func TestListStreamStopsOnVisitorError(t *testing.T) {
	state := newFakeS3()
	state.objects["tenant/a"] = []byte("a")
	state.objects["tenant/b"] = []byte("b")
	p, server := newTestProvider(t, state)
	defer server.Close()
	sentinel := errors.New("stop")
	calls := 0
	err := p.ListStream(context.Background(), RootID, func(provider.Entry) error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) || calls != 1 {
		t.Fatalf("visitor stop = %v after %d calls", err, calls)
	}
}

func TestFactoryAndVFSReadPath(t *testing.T) {
	state := newFakeS3()
	state.objects["tenant/media/movie.mkv"] = []byte("movie bytes")
	server := httptest.NewServer(state)
	defer server.Close()
	shared := httpx.New(httpx.Options{HTTP: server.Client()})
	pAny, err := provider.New("s3", "archive", map[string]any{
		"endpoint": server.URL, "region": "us-east-1", "bucket": "bucket", "prefix": "tenant",
		"lookup": "path", "access_key_id": "test-access", "secret_access_key": "test-secret",
		provider.ConfigHTTPClient: shared,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Factory("bad", map[string]any{
		"endpoint": server.URL, "bucket": "bucket", "anonymous": true,
		"access_key_id": "ignored", "secret_access_key": "ignored",
	}); err == nil {
		t.Fatal("anonymous configuration silently ignored credentials")
	}
	dir := t.TempDir()
	store, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	blockCache, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer blockCache.Close()
	fs, err := vfs.New(vfs.Options{
		Meta: store, Cache: blockCache, DefaultDirTTL: time.Minute, AttrTTL: time.Minute,
		Mounts: []vfs.Mount{{Prefix: "/", Remote: "archive", RootID: RootID, Provider: pAny, Mode: config.ModeReadonly, DirTTL: time.Minute}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	entries, err := fs.ReadDirPath(context.Background(), "/media")
	if err != nil || len(entries) != 1 || entries[0].Name != "movie.mkv" {
		t.Fatalf("VFS listing = %+v, %v", entries, err)
	}
	body, err := fs.ReadFileRange(context.Background(), "/media/movie.mkv", 0, 0)
	if err != nil || string(body) != "movie bytes" {
		t.Fatalf("VFS read = %q, %v", body, err)
	}
}

func TestCapabilitiesSayTheIDsArePaths(t *testing.T) {
	p, _ := newTestProvider(t, newFakeS3())
	if !p.Capabilities().PathIDs {
		t.Fatal("an S3 key is a path, so renaming a prefix changes every id beneath it; the VFS needs to be told")
	}
}
