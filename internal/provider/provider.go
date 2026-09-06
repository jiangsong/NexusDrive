// Package provider defines the single interface every cloud drive backend
// implements, plus the capability matrix the VFS, cache and upload layers use
// to pick strategies per backend. See docs/DESIGN.md §4.1.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Kind distinguishes files from directories.
type Kind int

const (
	KindFile Kind = iota
	KindDir
)

// HashType names a content hash algorithm a backend can compute or verify.
type HashType string

const (
	HashMD5    HashType = "md5"
	HashSHA1   HashType = "sha1"
	HashSHA256 HashType = "sha256"
	// HashSliceMD5 is Baidu's MD5 of the first 256 KiB.
	HashSliceMD5 HashType = "slice_md5"
	// HashPreSHA1 is Aliyun's SHA1 of the first 1 KiB (pre_hash).
	HashPreSHA1 HashType = "pre_sha1"
	// HashCRC32C is computed for every staged write regardless of what the
	// provider wants: it is what recovery checks a blob against.
	HashCRC32C HashType = "crc32c"
)

// Hashes maps hash type to lowercase hex digest.
type Hashes map[HashType]string

// Entry is one file or directory as the backend reports it.
type Entry struct {
	ID       string
	ParentID string
	Name     string
	Kind     Kind
	Size     int64
	ModTime  time.Time
	// Version is an opaque change token (ETag, cTag, updated_at…). Two
	// entries with equal Version have identical content.
	Version string
	Hashes  Hashes
}

// Link is a direct download URL with its validity window and any headers the
// backend requires on the request (User-Agent, Referer…).
type Link struct {
	URL       string
	ExpiresAt time.Time
	Headers   map[string]string
}

// UploadSession is returned by BeginUpload. When RapidDone is true the backend
// accepted the upload by hash alone and Entry is already final.
type UploadSession struct {
	ID        string
	PartSize  int64
	RapidDone bool
	Entry     *Entry
	// Opaque carries backend state (upload_id, urls…) and is persisted in the
	// journal so an interrupted upload can resume after restart.
	Opaque map[string]string
}

// PartToken identifies one successfully uploaded part.
type PartToken struct {
	Index int
	ETag  string
}

// ChangeOp is the kind of change reported by a delta feed.
type ChangeOp int

const (
	ChangeUpsert ChangeOp = iota
	ChangeDelete
)

// Change is one event from a backend's delta feed.
type Change struct {
	Op       ChangeOp
	ID       string
	ParentID string
	Entry    *Entry // nil for deletes
}

// Tier records whether the backend uses an official API.
type Tier string

const (
	TierOfficial   Tier = "official"
	TierUnofficial Tier = "unofficial"
)

// QPS is the recommended initial request rate per request class.
type QPS struct {
	Meta     float64
	Download float64
	Upload   float64
}

// Caps is the capability matrix. Upper layers never special-case a backend by
// name; they read Caps.
type Caps struct {
	HashTypes []HashType
	// RapidUpload lists the hashes that must be supplied to BeginUpload for
	// the backend to attempt a hash-only (秒传) upload. Empty means none.
	RapidUpload []HashType
	RangeRead   bool
	// StreamList advertises StreamLister for bounded directory enumeration.
	// Provider.List remains available to callers that explicitly need a slice.
	StreamList bool

	PartSize       int64
	MaxParts       int
	UploadParallel int
	// SinglePutMax is the largest file the backend accepts in one request
	// through SinglePutter. Zero means every upload goes through the
	// session protocol. A small file costs one round trip instead of three,
	// which is what bounds the rate of a batch of small files.
	SinglePutMax int64

	ServerMove   bool
	ServerRename bool
	ServerCopy   bool

	// Delta is true when the backend implements ChangeLister.
	Delta bool

	LinkTTL       time.Duration
	LinkHeaders   map[string]string
	LinkShareable bool

	QPS             QPS
	MaxConnsPerHost int
	Tier            Tier
}

// Provider is implemented by every backend.
type Provider interface {
	Name() string
	Capabilities() Caps

	List(ctx context.Context, dirID, cursor string) (entries []Entry, next string, err error)
	Stat(ctx context.Context, id string) (Entry, error)
	ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error)
	DownloadURL(ctx context.Context, id string) (Link, error)

	BeginUpload(ctx context.Context, parentID, name string, size int64, h Hashes) (UploadSession, error)
	UploadPart(ctx context.Context, s UploadSession, idx int, r io.Reader, n int64) (PartToken, error)
	CompleteUpload(ctx context.Context, s UploadSession, parts []PartToken) (Entry, error)

	Mkdir(ctx context.Context, parentID, name string) (Entry, error)
	Rename(ctx context.Context, id, newName string) (Entry, error)
	Move(ctx context.Context, id, newParentID string) (Entry, error)
	Delete(ctx context.Context, id string) error
}

// ChangeLister is implemented by backends with a delta / change feed.
type ChangeLister interface {
	Changes(ctx context.Context, cursor string) (events []Change, next string, err error)
}

// StreamLister enumerates one complete directory without retaining all its
// entries. Calls to visit are serial and stop before ListStream returns. A
// visitor error must stop enumeration and be returned without replaying it.
// An error, even after some entries, means the collection is incomplete and
// must not be published. ErrUnsupported permits fallback only before the
// first visit. Implementations must not retry a partially delivered stream.
type StreamLister interface {
	ListStream(ctx context.Context, dirID string, visit func(Entry) error) error
}

// RangeReaderAt is an optional fast path for ReadRange: the provider fills
// the caller's buffer directly instead of handing back a reader the caller
// then copies out of. A cold sequential read moves every byte through this
// call, and the copy plus the 4 MiB allocation per block it replaces were a
// third of the pipeline. Implementations return ErrUnsupported to fall back.
type RangeReaderAt interface {
	ReadRangeAt(ctx context.Context, id, version string, off int64, buf []byte) (int, error)
}

// SinglePutter is implemented by backends that can store a small file in one
// request, without an upload session.
type SinglePutter interface {
	// PutFile writes size bytes from r as name under parentID, replacing any
	// existing file of that name, and returns the resulting entry.
	PutFile(ctx context.Context, parentID, name string, r io.Reader, size int64, h Hashes) (Entry, error)
}

// ServerCopier is implemented by backends that can copy server-side.
type ServerCopier interface {
	Copy(ctx context.Context, id, newParentID, newName string) (Entry, error)
}

// Sentinel errors. Backends wrap these so internal/net/retry can classify
// failures without knowing the backend.
var (
	ErrNotFound    = errors.New("provider: not found")
	ErrExists      = errors.New("provider: already exists")
	ErrConflict    = errors.New("provider: version conflict")
	ErrRateLimited = errors.New("provider: rate limited")
	ErrRiskControl = errors.New("provider: risk control triggered")
	ErrAuth        = errors.New("provider: authentication required")
	ErrLinkExpired = errors.New("provider: download link expired")
	ErrCursorReset = errors.New("provider: change cursor reset")
	ErrUnsupported = errors.New("provider: operation not supported")
	ErrTransient   = errors.New("provider: transient failure")
	// ErrUnavailable means no backend that holds the data can be reached
	// right now. It is not a failure of the data: the caller should wait and
	// try again rather than give up or spend a retry budget. A storage pool
	// returns it when every member holding a replica is down; the uploader
	// defers instead of dead-lettering, and the kernel sees EHOSTDOWN.
	ErrUnavailable = errors.New("provider: no reachable backend")
)

// CursorResetError asks the caller to invalidate directory freshness and adopt
// a new delta baseline. Cursor is opaque and intentionally omitted from Error.
type CursorResetError struct {
	Cursor string
}

func (e *CursorResetError) Error() string { return ErrCursorReset.Error() }
func (e *CursorResetError) Unwrap() error { return ErrCursorReset }

// RetryAfterError carries a server-supplied wait hint alongside a sentinel.
type RetryAfterError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *RetryAfterError) Error() string { return e.Err.Error() }
func (e *RetryAfterError) Unwrap() error { return e.Err }

// ConfigHTTPClient is the config key under which the daemon passes a
// preconfigured HTTP client to a driver factory. The client already applies
// the proxy rules, the per-remote rate limiter and the circuit breaker, so a
// driver that uses it inherits all of that for free.
//
// A driver factory should call HTTPClientFrom and only build its own client
// when the key is absent (which is the case in unit tests).
const ConfigHTTPClient = "_http_client"

// HTTPClientFrom returns the shared client the daemon put in cfg, if any. The
// concrete type is *httpx.Client; it is returned as any to keep this package
// free of a dependency on httpx, which imports it.
func HTTPClientFrom(cfg map[string]any) (any, bool) {
	v, ok := cfg[ConfigHTTPClient]
	if !ok || v == nil {
		return nil, false
	}
	return v, true
}

// RootOf reports the id a provider lists its root from. Most cloud drives
// identify the root by an opaque id (a file id, or a fixed "root"), which
// the driver exposes through one of these methods; path-based backends
// (webdav, sftp, s3, smb) have none and list the root from "/". The
// instrumented wrapper does not forward optional interfaces, so the backend is
// reached through Unwrap first.
func RootOf(p Provider) string {
	switch d := Unwrap(p).(type) {
	case interface{ RootID() string }:
		return d.RootID()
	case interface{ RootFileID() string }:
		return d.RootFileID()
	case interface{ RootPath() string }:
		return d.RootPath()
	}
	return "/"
}

// ConfigLimiters and ConfigDialer are the config keys under which the daemon
// passes shared machinery to a driver factory, alongside ConfigHTTPClient.
//
// A backend that does not speak HTTP cannot inherit the rate limiter and the
// proxy routing through the HTTP client, so it receives them directly. Without
// this, an SFTP or SMB remote would quietly bypass the rule set that is meant
// to govern every egress in the process.
const (
	ConfigLimiters = "_limiters"
	ConfigDialer   = "_dialer"
)

// DialFunc opens a TCP connection through the configured outbound.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// LimitersFrom returns the shared rate-limit registry the daemon put in cfg.
// The concrete type is *ratelimit.Registry, returned as any to keep this
// package free of that dependency.
func LimitersFrom(cfg map[string]any) (any, bool) {
	v, ok := cfg[ConfigLimiters]
	if !ok || v == nil {
		return nil, false
	}
	return v, true
}

// DialerFrom returns the proxy-aware dialer the daemon put in cfg.
func DialerFrom(cfg map[string]any) (DialFunc, bool) {
	v, ok := cfg[ConfigDialer]
	if !ok || v == nil {
		return nil, false
	}
	d, ok := v.(DialFunc)
	if ok {
		return d, true
	}
	if f, ok := v.(func(context.Context, string, string) (net.Conn, error)); ok {
		return DialFunc(f), true
	}
	return nil, false
}

// Transporter is implemented by providers whose HTTP transport can be replaced
// after construction. The daemon uses it as a second route for injecting the
// proxy-aware, rate-limited client into drivers that build their own.
type Transporter interface {
	// SetTransport replaces the driver's HTTP client. The argument is an
	// *httpx.Client.
	SetTransport(client any)
}

// EnsureVersion fills in a change token when the backend did not supply one.
//
// The block cache keys every cached byte on (remote, id, version). An entry
// with an empty version would make two different contents share a key, so a
// read after a remote edit could return the previous file's bytes. Rather than
// let that happen, fall back to the content hash, then to a size and mtime
// fingerprint, which is what a backend with no ETag can still distinguish.
//
// Drivers should call this on every Entry they construct by hand, in
// particular on the paths where a follow-up Stat failed and the entry is
// assembled from what the call already knew.
func EnsureVersion(e *Entry) {
	if e.Version != "" {
		return
	}
	for _, ht := range []HashType{HashSHA1, HashMD5, HashSHA256} {
		if v, ok := e.Hashes[ht]; ok && v != "" {
			e.Version = v
			return
		}
	}
	if e.Kind == KindDir {
		// A directory has no content to cache, so its id is a stable enough
		// token to keep the tree consistent.
		e.Version = "dir-" + e.ID
		return
	}
	e.Version = fmt.Sprintf("%d-%d", e.Size, e.ModTime.UnixNano())
}
