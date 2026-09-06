// Package s3 implements the Provider interface for AWS S3 and compatible
// object stores. Slash-delimited keys are projected as directories; an
// optional immutable prefix confines every operation to one key namespace.
package s3

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/provider"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"
)

const (
	RootID              = "/"
	defaultPartSize     = 8 << 20
	minimumPartSize     = 5 << 20
	defaultSinglePutMax = 64 << 20
	maxTreeObjects      = 1_000_000
)

type Options struct {
	Name         string
	Endpoint     *url.URL
	Region       string
	Bucket       string
	Prefix       string
	AccessKey    string
	SecretKey    string
	SessionToken string
	Anonymous    bool
	Lookup       minio.BucketLookupType
	PartSize     int64
	Transport    http.RoundTripper
}

type Provider struct {
	name     string
	bucket   string
	prefix   string
	partSize int64
	client   *minio.Client
	core     minio.Core
	caps     provider.Caps
}

func New(opt Options) (*Provider, error) {
	if opt.Endpoint == nil || opt.Endpoint.Host == "" {
		return nil, errors.New("s3: endpoint is required")
	}
	if opt.Bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}
	if err := s3utils.CheckValidBucketName(opt.Bucket); err != nil {
		return nil, fmt.Errorf("s3: invalid bucket: %w", err)
	}
	prefix, err := cleanPrefix(opt.Prefix)
	if err != nil {
		return nil, err
	}
	partSize := opt.PartSize
	if partSize == 0 {
		partSize = defaultPartSize
	}
	if partSize < minimumPartSize || partSize > 5<<30 {
		return nil, fmt.Errorf("s3: part_size must be between 5MiB and 5GiB")
	}
	region := opt.Region
	if region == "" {
		region = "us-east-1"
	}
	creds := credentials.NewStaticV4(opt.AccessKey, opt.SecretKey, opt.SessionToken)
	if opt.Anonymous {
		creds = credentials.NewStaticV4("", "", "")
	}
	client, err := minio.New(opt.Endpoint.Host, &minio.Options{
		Creds: creds, Secure: opt.Endpoint.Scheme == "https", Region: region,
		BucketLookup: opt.Lookup, Transport: opt.Transport, MaxRetries: 1,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: create client: %w", err)
	}
	p := &Provider{
		name: opt.Name, bucket: opt.Bucket, prefix: prefix, partSize: partSize,
		client: client, core: minio.Core{Client: client},
		caps: provider.Caps{
			// Naming: what the drive refuses in a name, so a pool never places
			// a replica the drive would then reject.
			Naming:    provider.Naming{MaxPathBytes: 1024},
			RangeRead: true, StreamList: true,
			PartSize: partSize, MaxParts: 10000, UploadParallel: 4, SinglePutMax: defaultSinglePutMax,
			ServerMove: true, ServerRename: true, ServerCopy: true,
			LinkTTL: 15 * time.Minute, LinkShareable: true,
			QPS: provider.QPS{Meta: 20, Download: 16, Upload: 8}, MaxConnsPerHost: 16,
			Tier: provider.TierOfficial,
		},
	}
	return p, nil
}

func (p *Provider) Name() string                { return p.name }
func (p *Provider) Capabilities() provider.Caps { return p.caps }

func cleanPrefix(raw string) (string, error) {
	if strings.ContainsAny(raw, "\\\x00") || hasControl(raw) {
		return "", errors.New("s3: prefix contains an unsafe character")
	}
	raw = strings.Trim(raw, "/")
	if raw == "" {
		return "", nil
	}
	clean := path.Clean(raw)
	if clean != raw || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("s3: prefix must be a canonical relative key prefix")
	}
	return clean, nil
}

func cleanID(raw string, directory bool) (string, error) {
	if raw == "" || raw == "/" {
		return RootID, nil
	}
	if strings.ContainsAny(raw, "\\\x00") || hasControl(raw) {
		return "", errors.New("s3: object id contains an unsafe character")
	}
	rel := strings.TrimPrefix(raw, "/")
	want := "/" + strings.TrimSuffix(rel, "/")
	clean := path.Clean(want)
	if clean == "/" || clean == "/.." || strings.HasPrefix(clean, "/../") {
		return "", errors.New("s3: invalid object id")
	}
	if clean != want {
		return "", errors.New("s3: object id must be canonical and cannot contain dot segments or repeated separators")
	}
	if directory {
		clean = strings.TrimSuffix(clean, "/") + "/"
	}
	return clean, nil
}

func safeName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func (p *Provider) keyForID(id string, directory bool) (string, error) {
	clean, err := cleanID(id, directory)
	if err != nil {
		return "", err
	}
	rel := strings.Trim(strings.TrimPrefix(clean, "/"), "/")
	key := rel
	if p.prefix != "" {
		key = p.prefix
		if rel != "" {
			key += "/" + rel
		}
	}
	if directory && key != "" {
		key += "/"
	}
	return key, nil
}

func (p *Provider) idForKey(key string, directory bool) (string, error) {
	rel := key
	if p.prefix != "" {
		if key == p.prefix || key == p.prefix+"/" {
			return RootID, nil
		}
		if !strings.HasPrefix(key, p.prefix+"/") {
			return "", errors.New("s3: server returned an object outside the configured prefix")
		}
		rel = strings.TrimPrefix(key, p.prefix+"/")
	}
	return cleanID("/"+rel, directory)
}

func childID(parent, name string, directory bool) (string, error) {
	if !safeName(name) {
		return "", fmt.Errorf("s3: unsafe child name %q", name)
	}
	parent, err := cleanID(parent, true)
	if err != nil {
		return "", err
	}
	id := path.Join(parent, name)
	return cleanID(id, directory)
}

func parentID(id string) string {
	clean := strings.TrimSuffix(id, "/")
	parent := path.Dir(clean)
	if parent == "." || parent == "" {
		return RootID
	}
	return strings.TrimSuffix(parent, "/") + "/"
}

func (p *Provider) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	if cursor != "" {
		return nil, "", provider.ErrUnsupported
	}
	var entries []provider.Entry
	err := p.ListStream(ctx, dirID, func(e provider.Entry) error {
		entries = append(entries, e)
		return nil
	})
	return entries, "", err
}

func (p *Provider) ListStream(ctx context.Context, dirID string, visit func(provider.Entry) error) error {
	dirID, err := cleanID(dirID, true)
	if err != nil {
		return err
	}
	prefix, err := p.keyForID(dirID, true)
	if err != nil {
		return err
	}
	fetchOwner := false
	seen := map[string]bool{}
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	objects := p.client.ListObjects(listCtx, p.bucket, minio.ListObjectsOptions{
		Prefix: prefix, Recursive: false, MaxKeys: 1000, FetchOwner: &fetchOwner,
	})
	drain := func() {
		cancel()
		for range objects {
		}
	}
	fail := func(err error) error {
		drain()
		return err
	}
	for obj := range objects {
		if obj.Err != nil {
			return fail(mapError(obj.Err))
		}
		if obj.Key == prefix || (prefix == "" && obj.Key == "") {
			continue
		}
		remainder := strings.TrimPrefix(obj.Key, prefix)
		if remainder == obj.Key && prefix != "" {
			return fail(errors.New("s3: listing escaped the requested prefix"))
		}
		directory := strings.HasSuffix(remainder, "/")
		name := strings.TrimSuffix(remainder, "/")
		if name == "" || strings.Contains(name, "/") || !safeName(name) {
			return fail(fmt.Errorf("s3: invalid direct child key %q", obj.Key))
		}
		id, err := p.idForKey(obj.Key, directory)
		if err != nil {
			return fail(err)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		entry := provider.Entry{ID: id, ParentID: dirID, Name: name, Kind: provider.KindFile, Size: obj.Size, ModTime: obj.LastModified, Version: strings.Trim(obj.ETag, `"`)}
		if directory {
			entry.Kind, entry.Size, entry.Version = provider.KindDir, 0, ""
		} else {
			entry.Hashes = etagHashes(entry.Version)
		}
		if err := visit(entry); err != nil {
			return fail(err)
		}
	}
	return ctx.Err()
}

func etagHashes(etag string) provider.Hashes {
	if len(etag) != 32 || strings.Contains(etag, "-") {
		return nil
	}
	if _, err := hex.DecodeString(etag); err != nil {
		return nil
	}
	return provider.Hashes{provider.HashMD5: strings.ToLower(etag)}
}

func (p *Provider) Stat(ctx context.Context, id string) (provider.Entry, error) {
	clean, err := cleanID(id, strings.HasSuffix(id, "/"))
	if err != nil {
		return provider.Entry{}, err
	}
	if clean == RootID {
		return provider.Entry{ID: RootID, Name: "", Kind: provider.KindDir}, nil
	}
	if !strings.HasSuffix(clean, "/") {
		key, _ := p.keyForID(clean, false)
		obj, err := p.client.StatObject(ctx, p.bucket, key, minio.StatObjectOptions{})
		if err == nil {
			return p.fileEntry(clean, obj), nil
		}
		if !errors.Is(mapError(err), provider.ErrNotFound) {
			return provider.Entry{}, mapError(err)
		}
	}
	dir, err := cleanID(clean, true)
	if err != nil {
		return provider.Entry{}, err
	}
	exists, err := p.directoryExists(ctx, dir)
	if err != nil {
		return provider.Entry{}, err
	}
	if !exists {
		return provider.Entry{}, provider.ErrNotFound
	}
	return provider.Entry{ID: dir, ParentID: parentID(dir), Name: path.Base(strings.TrimSuffix(dir, "/")), Kind: provider.KindDir}, nil
}

func (p *Provider) directoryExists(ctx context.Context, id string) (bool, error) {
	prefix, err := p.keyForID(id, true)
	if err != nil {
		return false, err
	}
	check, cancel := context.WithCancel(ctx)
	defer cancel()
	fetchOwner := false
	ch := p.client.ListObjects(check, p.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true, MaxKeys: 1, FetchOwner: &fetchOwner})
	for obj := range ch {
		if obj.Err != nil {
			return false, mapError(obj.Err)
		}
		cancel()
		for range ch {
		}
		return true, nil
	}
	return false, ctx.Err()
}

func (p *Provider) fileEntry(id string, obj minio.ObjectInfo) provider.Entry {
	return provider.Entry{
		ID: id, ParentID: parentID(id), Name: path.Base(id), Kind: provider.KindFile,
		Size: obj.Size, ModTime: obj.LastModified, Version: strings.Trim(obj.ETag, `"`), Hashes: etagHashes(strings.Trim(obj.ETag, `"`)),
	}
}

func (p *Provider) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	if off < 0 {
		return nil, errors.New("s3: invalid byte range")
	}
	key, err := p.keyForID(id, false)
	if err != nil || key == "" {
		return nil, provider.ErrNotFound
	}
	opts := minio.GetObjectOptions{}
	if n <= 0 && off > 0 {
		err = opts.SetRange(off, 0)
	} else if n > 0 {
		err = opts.SetRange(off, off+n-1)
	}
	if err != nil {
		return nil, err
	}
	if version != "" {
		if err := opts.SetMatchETag(version); err != nil {
			return nil, err
		}
	}
	rc, _, _, err := p.core.GetObject(ctx, p.bucket, key, opts)
	if err != nil {
		return nil, mapError(err)
	}
	return rc, nil
}

func (p *Provider) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	key, err := p.keyForID(id, false)
	if err != nil || key == "" {
		return provider.Link{}, provider.ErrNotFound
	}
	expires := p.caps.LinkTTL
	u, err := p.client.PresignedGetObject(ctx, p.bucket, key, expires, nil)
	if err != nil {
		return provider.Link{}, mapError(err)
	}
	return provider.Link{URL: u.String(), ExpiresAt: time.Now().Add(expires)}, nil
}

func (p *Provider) PutFile(ctx context.Context, parent, name string, r io.Reader, size int64, _ provider.Hashes) (provider.Entry, error) {
	if size < 0 {
		return provider.Entry{}, errors.New("s3: upload size cannot be negative")
	}
	id, err := childID(parent, name, false)
	if err != nil {
		return provider.Entry{}, err
	}
	key, _ := p.keyForID(id, false)
	info, err := p.core.PutObject(ctx, p.bucket, key, io.LimitReader(r, size), size, "", "", minio.PutObjectOptions{DisableMultipart: true})
	if err != nil {
		return provider.Entry{}, mapError(err)
	}
	return p.uploadEntry(id, size, info), nil
}

func (p *Provider) BeginUpload(ctx context.Context, parent, name string, size int64, _ provider.Hashes) (provider.UploadSession, error) {
	if size < 0 {
		return provider.UploadSession{}, errors.New("s3: upload size cannot be negative")
	}
	id, err := childID(parent, name, false)
	if err != nil {
		return provider.UploadSession{}, err
	}
	key, _ := p.keyForID(id, false)
	uploadID, err := p.core.NewMultipartUpload(ctx, p.bucket, key, minio.PutObjectOptions{})
	if err != nil {
		return provider.UploadSession{}, mapError(err)
	}
	return provider.UploadSession{
		ID: uploadID, PartSize: p.partSize,
		Opaque: map[string]string{"upload_id": uploadID, "key": key, "id": id, "size": strconv.FormatInt(size, 10)},
	}, nil
}

func sessionFields(s provider.UploadSession) (uploadID, key, id string, size int64, err error) {
	uploadID, key, id = s.Opaque["upload_id"], s.Opaque["key"], s.Opaque["id"]
	if uploadID == "" {
		uploadID = s.ID
	}
	size, err = strconv.ParseInt(s.Opaque["size"], 10, 64)
	if uploadID == "" || key == "" || id == "" || err != nil || size < 0 {
		err = errors.New("s3: invalid persisted upload session")
	}
	return
}

func (p *Provider) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	uploadID, key, _, _, err := sessionFields(s)
	if err != nil || idx < 0 || idx >= p.caps.MaxParts || n < 0 || n > p.partSize {
		return provider.PartToken{}, errors.New("s3: invalid upload part")
	}
	part, err := p.core.PutObjectPart(ctx, p.bucket, key, uploadID, idx+1, io.LimitReader(r, n), n, minio.PutObjectPartOptions{})
	if err != nil {
		return provider.PartToken{}, mapError(err)
	}
	return provider.PartToken{Index: idx, ETag: strings.Trim(part.ETag, `"`)}, nil
}

func (p *Provider) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	uploadID, key, id, size, err := sessionFields(s)
	if err != nil {
		return provider.Entry{}, err
	}
	ordered := append([]provider.PartToken(nil), parts...)
	if len(ordered) == 0 {
		return provider.Entry{}, errors.New("s3: multipart upload has no parts")
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })
	complete := make([]minio.CompletePart, len(ordered))
	for i, part := range ordered {
		if part.Index != i || part.ETag == "" {
			return provider.Entry{}, errors.New("s3: upload parts must be unique and contiguous from zero")
		}
		complete[i] = minio.CompletePart{PartNumber: i + 1, ETag: part.ETag}
	}
	info, err := p.core.CompleteMultipartUpload(ctx, p.bucket, key, uploadID, complete, minio.PutObjectOptions{})
	if err != nil {
		return provider.Entry{}, mapError(err)
	}
	return p.uploadEntry(id, size, info), nil
}

func (p *Provider) uploadEntry(id string, size int64, info minio.UploadInfo) provider.Entry {
	return provider.Entry{
		ID: id, ParentID: parentID(id), Name: path.Base(id), Kind: provider.KindFile,
		Size: size, ModTime: info.LastModified, Version: strings.Trim(info.ETag, `"`), Hashes: etagHashes(strings.Trim(info.ETag, `"`)),
	}
}

func (p *Provider) Mkdir(ctx context.Context, parent, name string) (provider.Entry, error) {
	id, err := childID(parent, name, true)
	if err != nil {
		return provider.Entry{}, err
	}
	if _, err := p.Stat(ctx, strings.TrimSuffix(id, "/")); err == nil {
		return provider.Entry{}, provider.ErrExists
	} else if !errors.Is(err, provider.ErrNotFound) {
		return provider.Entry{}, err
	}
	key, _ := p.keyForID(id, true)
	_, err = p.core.PutObject(ctx, p.bucket, key, strings.NewReader(""), 0, "", "", minio.PutObjectOptions{DisableMultipart: true, ContentType: "application/x-directory"})
	if err != nil {
		return provider.Entry{}, mapError(err)
	}
	return provider.Entry{ID: id, ParentID: parentID(id), Name: name, Kind: provider.KindDir}, nil
}

func (p *Provider) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	if !safeName(newName) {
		return provider.Entry{}, errors.New("s3: invalid new name")
	}
	source, err := p.Stat(ctx, id)
	if err != nil {
		return provider.Entry{}, err
	}
	target, err := childID(source.ParentID, newName, source.Kind == provider.KindDir)
	if err != nil {
		return provider.Entry{}, err
	}
	return p.moveTo(ctx, source, target)
}

func (p *Provider) Move(ctx context.Context, id, newParent string) (provider.Entry, error) {
	source, err := p.Stat(ctx, id)
	if err != nil {
		return provider.Entry{}, err
	}
	target, err := childID(newParent, source.Name, source.Kind == provider.KindDir)
	if err != nil {
		return provider.Entry{}, err
	}
	return p.moveTo(ctx, source, target)
}

func (p *Provider) moveTo(ctx context.Context, source provider.Entry, target string) (provider.Entry, error) {
	checkTarget := target
	if source.Kind == provider.KindDir {
		checkTarget = strings.TrimSuffix(target, "/")
	}
	if _, err := p.Stat(ctx, checkTarget); err == nil {
		return provider.Entry{}, provider.ErrExists
	} else if !errors.Is(err, provider.ErrNotFound) {
		return provider.Entry{}, err
	}
	if source.Kind == provider.KindFile {
		if err := p.copyKey(ctx, source.ID, target, source.Size); err != nil {
			return provider.Entry{}, err
		}
		if err := p.deleteKey(ctx, source.ID, false); err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			_ = p.deleteKey(cleanup, target, false)
			cancel()
			return provider.Entry{}, err
		}
		return p.Stat(ctx, target)
	}
	if err := p.moveTree(ctx, source.ID, target); err != nil {
		return provider.Entry{}, err
	}
	return p.Stat(ctx, target)
}

func (p *Provider) Copy(ctx context.Context, id, newParent, newName string) (provider.Entry, error) {
	source, err := p.Stat(ctx, id)
	if err != nil {
		return provider.Entry{}, err
	}
	if source.Kind != provider.KindFile {
		return provider.Entry{}, provider.ErrUnsupported
	}
	if newName == "" {
		newName = source.Name
	}
	target, err := childID(newParent, newName, false)
	if err != nil {
		return provider.Entry{}, err
	}
	if _, err := p.Stat(ctx, target); err == nil {
		return provider.Entry{}, provider.ErrExists
	} else if !errors.Is(err, provider.ErrNotFound) {
		return provider.Entry{}, err
	}
	if err := p.copyKey(ctx, source.ID, target, source.Size); err != nil {
		return provider.Entry{}, err
	}
	return p.Stat(ctx, target)
}

func (p *Provider) copyKey(ctx context.Context, sourceID, targetID string, size int64) error {
	source, err := p.keyForID(sourceID, strings.HasSuffix(sourceID, "/"))
	if err != nil {
		return err
	}
	target, err := p.keyForID(targetID, strings.HasSuffix(targetID, "/"))
	if err != nil {
		return err
	}
	dst := minio.CopyDestOptions{Bucket: p.bucket, Object: target}
	src := minio.CopySrcOptions{Bucket: p.bucket, Object: source}
	var copyErr error
	if size <= 5<<30 {
		_, copyErr = p.client.CopyObject(ctx, dst, src)
	} else {
		_, copyErr = p.client.ComposeObject(ctx, dst, src)
	}
	return mapError(copyErr)
}

func (p *Provider) moveTree(ctx context.Context, sourceID, targetID string) error {
	sourcePrefix, _ := p.keyForID(sourceID, true)
	targetPrefix, _ := p.keyForID(targetID, true)
	if strings.HasPrefix(targetPrefix, sourcePrefix) {
		return errors.New("s3: cannot move a directory into itself")
	}
	fetchOwner := false
	type object struct {
		source, target string
		size           int64
	}
	var objects []object
	for obj := range p.client.ListObjects(ctx, p.bucket, minio.ListObjectsOptions{Prefix: sourcePrefix, Recursive: true, FetchOwner: &fetchOwner}) {
		if obj.Err != nil {
			return mapError(obj.Err)
		}
		if len(objects) >= maxTreeObjects {
			return errors.New("s3: directory move exceeds one million objects")
		}
		objects = append(objects, object{source: obj.Key, target: targetPrefix + strings.TrimPrefix(obj.Key, sourcePrefix), size: obj.Size})
	}
	if len(objects) == 0 {
		return provider.ErrNotFound
	}
	var copied []string
	for _, obj := range objects {
		srcID, _ := p.idForKey(obj.source, strings.HasSuffix(obj.source, "/"))
		dstID, _ := p.idForKey(obj.target, strings.HasSuffix(obj.target, "/"))
		if err := p.copyKey(ctx, srcID, dstID, obj.size); err != nil {
			p.rollbackCopies(copied)
			return err
		}
		copied = append(copied, dstID)
	}
	for i := len(objects) - 1; i >= 0; i-- {
		id, _ := p.idForKey(objects[i].source, strings.HasSuffix(objects[i].source, "/"))
		if err := p.deleteKey(ctx, id, strings.HasSuffix(id, "/")); err != nil {
			return err // sources not yet deleted remain; copied data is retained.
		}
	}
	return nil
}

func (p *Provider) rollbackCopies(ids []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := len(ids) - 1; i >= 0; i-- {
		_ = p.deleteKey(ctx, ids[i], strings.HasSuffix(ids[i], "/"))
	}
}

func (p *Provider) Delete(ctx context.Context, id string) error {
	clean, err := cleanID(id, strings.HasSuffix(id, "/"))
	if err != nil {
		return err
	}
	if clean == RootID {
		return errors.New("s3: refusing to delete the provider root")
	}
	entry, err := p.Stat(ctx, clean)
	if err != nil {
		return err
	}
	if entry.Kind == provider.KindFile {
		return p.deleteKey(ctx, entry.ID, false)
	}
	prefix, _ := p.keyForID(entry.ID, true)
	fetchOwner := false
	var ids []string
	for obj := range p.client.ListObjects(ctx, p.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true, FetchOwner: &fetchOwner}) {
		if obj.Err != nil {
			return mapError(obj.Err)
		}
		if len(ids) >= maxTreeObjects {
			return errors.New("s3: directory delete exceeds one million objects")
		}
		child, err := p.idForKey(obj.Key, strings.HasSuffix(obj.Key, "/"))
		if err != nil {
			return err
		}
		ids = append(ids, child)
	}
	for i := len(ids) - 1; i >= 0; i-- {
		if err := p.deleteKey(ctx, ids[i], strings.HasSuffix(ids[i], "/")); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) deleteKey(ctx context.Context, id string, directory bool) error {
	key, err := p.keyForID(id, directory)
	if err != nil || key == "" {
		return provider.ErrNotFound
	}
	return mapError(p.client.RemoveObject(ctx, p.bucket, key, minio.RemoveObjectOptions{}))
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	resp := minio.ToErrorResponse(err)
	switch resp.Code {
	case "NoSuchKey", "NoSuchBucket", "NotFound", "NoSuchUpload", "NoSuchObject":
		return fmt.Errorf("%w: %v", provider.ErrNotFound, err)
	case "BucketAlreadyExists", "BucketAlreadyOwnedByYou", "EntityAlreadyExists":
		return fmt.Errorf("%w: %v", provider.ErrExists, err)
	case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "ExpiredToken", "InvalidToken", "TokenRefreshRequired":
		return fmt.Errorf("%w: %v", provider.ErrAuth, err)
	case "SlowDown", "Throttling", "ThrottlingException", "RequestLimitExceeded", "TooManyRequestsException":
		return fmt.Errorf("%w: %v", provider.ErrRateLimited, err)
	case "PreconditionFailed", "ConditionalRequestConflict", "OperationAborted":
		return fmt.Errorf("%w: %v", provider.ErrConflict, err)
	case "InternalError", "RequestTimeout", "RequestTimeTooSkewed", "ServiceUnavailable":
		return fmt.Errorf("%w: %v", provider.ErrTransient, err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %v", provider.ErrAuth, err)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %v", provider.ErrNotFound, err)
	case resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusPreconditionFailed:
		return fmt.Errorf("%w: %v", provider.ErrConflict, err)
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w: %v", provider.ErrRateLimited, err)
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: %v", provider.ErrTransient, err)
	}
	return err
}

var (
	_ provider.Provider     = (*Provider)(nil)
	_ provider.StreamLister = (*Provider)(nil)
	_ provider.SinglePutter = (*Provider)(nil)
	_ provider.ServerCopier = (*Provider)(nil)
)
