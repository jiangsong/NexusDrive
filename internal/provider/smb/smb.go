// Package smb implements the Provider interface over SMB2/3, the protocol a
// NAS or a Windows share speaks. It is the last of the generic protocols the
// design lists alongside WebDAV and SFTP (docs/DESIGN.md §4.1).
//
// Like SFTP, SMB has no per-file id, no server-computed content hash and no
// change feed: the path is the identity and the capability matrix says so,
// which is what makes the VFS fall back to a size-and-mtime fingerprint and to
// TTL refresh instead of a delta cursor.
//
// One behaviour differs from SFTP and shapes the write path: SMB2's rename
// carries a ReplaceIfExists flag that the client library leaves clear, so a
// rename onto an existing name fails instead of replacing it. Publishing an
// upload therefore has to unlink the destination first, which opens a window
// where the name does not exist. That window is deliberate and documented
// rather than hidden behind a retry.
package smb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
)

// RootID is the id of the configured root directory.
const RootID = "/"

// tempPrefix marks in-flight uploads. They are hidden from listings so a
// partially written file never appears in the mount as if it were real.
const tempPrefix = ".cloudfs-upload."

// fileSystem is the subset of an SMB share this driver uses. go-smb2's
// *smb2.Share satisfies it through the adapter in conn.go; tests supply an
// in-memory tree, which is what lets the path, staging and error-mapping
// logic be exercised without a server.
type fileSystem interface {
	Stat(name string) (os.FileInfo, error)
	ReadDir(name string) ([]os.FileInfo, error)
	Mkdir(name string, perm os.FileMode) error
	Remove(name string) error
	Rename(oldpath, newpath string) error
	Open(name string) (fileHandle, error)
	OpenFile(name string, flag int, perm os.FileMode) (fileHandle, error)
	Create(name string) (fileHandle, error)
}

// fileHandle is the subset of *smb2.File this driver uses.
type fileHandle interface {
	io.Closer
	io.ReaderAt
	io.WriterAt
	Readdir(n int) ([]os.FileInfo, error)
	Stat() (os.FileInfo, error)
}

// runner gives the driver a share to work on. Production dials and remounts as
// needed; tests hand over a fixed in-memory tree.
type runner interface {
	// use runs fn against a mounted share, applying the class rate limit.
	use(ctx context.Context, class ratelimit.Class, fn func(fileSystem) error) error
	Close() error
}

// Provider is an SMB backend.
type Provider struct {
	name string
	caps provider.Caps
	// root is the directory inside the share that is exposed, "" for the
	// share root.
	root string

	share   runner
	handles *handleCache
}

// Options configures a Provider.
type Options struct {
	// Name is the remote name used in metadata and cache keys.
	Name string
	// Host and Port address the server. Port defaults to 445.
	Host string
	Port int
	// Share is the share name to mount, without the \\server\ prefix.
	Share string
	// User, Password and Domain authenticate with NTLM. Hash, when set, is
	// the NTLM hash and replaces Password.
	User, Password, Domain string
	Hash                   []byte
	// Root is a directory inside the share to expose. Empty means the share
	// root.
	Root string
	// PartSize is the upload chunk size; Concurrency bounds parallel parts.
	PartSize    int64
	Concurrency int
	// Dial opens the TCP connection. The daemon passes one that applies the
	// proxy rules; nil dials directly.
	Dial provider.DialFunc
	// Limiters, when set, applies the shared token buckets to this backend.
	Limiters *ratelimit.Registry
	// IdleTimeout drops an unused connection after this long. Zero means 2
	// minutes; negative keeps it forever.
	IdleTimeout time.Duration
	// share replaces the connection entirely; tests use it.
	share runner
}

// New builds an SMB provider. It does not connect; the first call does.
func New(opt Options) (*Provider, error) {
	if opt.share == nil {
		if opt.Host == "" {
			return nil, errors.New("smb: host is required")
		}
		if opt.Share == "" {
			return nil, errors.New("smb: share is required")
		}
		if opt.User == "" {
			return nil, errors.New("smb: user is required; anonymous sessions are not supported")
		}
		if strings.ContainsAny(opt.Share, `\/`) {
			return nil, errors.New("smb: share must be a bare share name, without a server prefix")
		}
	}
	for field, value := range map[string]string{
		"host": opt.Host, "share": opt.Share, "user": opt.User, "domain": opt.Domain,
	} {
		if strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("smb: %s contains an unsafe character", field)
		}
	}
	if opt.Port < 0 || opt.Port > 65535 {
		return nil, errors.New("smb: port is out of range")
	}
	root, err := cleanRoot(opt.Root)
	if err != nil {
		return nil, err
	}
	partSize := opt.PartSize
	if partSize <= 0 {
		partSize = 4 << 20
	}
	if partSize > 1<<30 {
		return nil, errors.New("smb: part_size must be at most 1 GiB")
	}
	conc := opt.Concurrency
	if conc <= 0 {
		conc = 4
	}
	p := &Provider{
		name: opt.Name, root: root, share: opt.share, handles: newHandleCache(),
		caps: provider.Caps{
			// Naming: what the drive refuses in a name, so a pool never places
			// a replica the drive would then reject.
			Naming: provider.Naming{CaseInsensitive: true, MaxNameBytes: 255, ForbiddenRunes: provider.WindowsForbiddenRunes, ReservedNames: provider.WindowsReservedNames, NoTrailingDotSpace: true},
			// SMB exposes no content hash, so the VFS fingerprints on size
			// and mtime through provider.EnsureVersion.
			HashTypes: nil, RapidUpload: nil,
			RangeRead: true, StreamList: true,

			PartSize: partSize, MaxParts: 100000, UploadParallel: conc,
			// One part fits in a single create-write-rename round.
			SinglePutMax: partSize,

			ServerMove: true, ServerRename: true,
			// SMB2 has FSCTL_SRV_COPYCHUNK, but the client library does not
			// expose it; claiming server copy would make the VFS choose a
			// path that cannot run.
			ServerCopy: false,
			Delta:      false,

			LinkTTL: 0,
			// A URL would need this process's SMB session, so no other
			// process could follow one.
			LinkShareable: false,
			// SMB has no server-side request quota and no risk control: the
			// limit is the link and the server's own concurrency, not a
			// rate. These ceilings exist so the AIMD limiter only backs off
			// when the server actually pushes back.
			QPS:             provider.QPS{Meta: 4096, Download: 4096, Upload: 2048},
			MaxConnsPerHost: conc,
			Tier:            provider.TierOfficial,
		},
	}
	if p.share == nil {
		p.share = newSession(sessionOptions{
			Host: opt.Host, Port: opt.Port, Share: opt.Share,
			User: opt.User, Password: opt.Password, Domain: opt.Domain, Hash: opt.Hash,
			Dial: opt.Dial, Limiters: opt.Limiters, Remote: opt.Name,
			IdleTimeout: opt.IdleTimeout, OnDrop: p.handles.purge,
		})
	}
	return p, nil
}

// cleanRoot normalises the in-share root and refuses one that walks upwards.
//
// path.Clean would quietly absorb a leading "..", turning "../other" into
// "/other" — a different share directory than the one that was asked for. A
// root that cannot mean what it says is rejected rather than reinterpreted.
func cleanRoot(raw string) (string, error) {
	raw = strings.ReplaceAll(strings.TrimSpace(raw), `\`, "/")
	if raw == "" || raw == "." || raw == "/" {
		return "", nil
	}
	if hasDotDot(raw) {
		return "", errors.New("smb: root must not contain a .. component")
	}
	clean := path.Clean("/" + strings.TrimPrefix(raw, "/"))
	if clean == "/" {
		return "", nil
	}
	return strings.TrimPrefix(clean, "/"), nil
}

// hasDotDot reports whether any component walks upwards. Both separators are
// checked because an SMB path may arrive written either way.
func hasDotDot(raw string) bool {
	for _, part := range strings.Split(strings.ReplaceAll(raw, `\`, "/"), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func (p *Provider) Name() string                { return p.name }
func (p *Provider) RootID() string              { return RootID }
func (p *Provider) Capabilities() provider.Caps { return p.caps }

// Close releases cached handles and the session.
func (p *Provider) Close() error {
	p.handles.closeAll()
	if p.share != nil {
		return p.share.Close()
	}
	return nil
}

// HandleStats reports remote handles opened and closed by the read path.
func (p *Provider) HandleStats() (opens, closes int64) { return p.handles.stats() }

// rel turns a provider id into a share-relative path. SMB names are relative
// to the share, so a leading separator is never sent, and a component that
// walks upwards is rejected rather than normalised into an escape.
func (p *Provider) rel(id string) (string, error) {
	if hasDotDot(id) {
		// path.Clean would absorb these into a different, still-valid path.
		// Nothing above this layer produces one, so an id carrying ".." is a
		// bug or an attempt to reach outside the mount; either way it is
		// refused rather than silently redirected.
		return "", fmt.Errorf("smb: path %q contains a .. component", id)
	}
	raw := strings.ReplaceAll(id, `\`, "/")
	clean := path.Clean("/" + strings.TrimPrefix(raw, "/"))
	rel := strings.TrimPrefix(clean, "/")
	if p.root == "" {
		return rel, nil
	}
	if rel == "" {
		return p.root, nil
	}
	return p.root + "/" + rel, nil
}

// id normalises a provider id for use as an Entry identity.
func normalizeID(id string) string {
	return path.Clean("/" + strings.TrimPrefix(strings.ReplaceAll(id, `\`, "/"), "/"))
}

// List returns one page of a directory. SMB pagination is a server-side
// cursor bound to an open handle, which cannot survive between calls, so the
// slice form returns everything at once and the cursor stays empty.
func (p *Provider) List(ctx context.Context, dirID, cursor string) ([]provider.Entry, string, error) {
	var out []provider.Entry
	err := p.ListStream(ctx, dirID, func(e provider.Entry) error {
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return out, "", nil
}

// listBatch is how many directory entries one Readdir call asks for. It bounds
// what a single response holds without turning a large directory into one
// request per name.
const listBatch = 512

// ListStream enumerates a directory through an open handle, delivering each
// batch as it arrives instead of accumulating the whole directory.
func (p *Provider) ListStream(ctx context.Context, dirID string, visit func(provider.Entry) error) error {
	if visit == nil {
		return errors.New("smb: list visitor is nil")
	}
	rel, err := p.rel(dirID)
	if err != nil {
		return err
	}
	parent := normalizeID(dirID)
	return p.share.use(ctx, ratelimit.Meta, func(fs fileSystem) error {
		dir, err := fs.Open(rel)
		if err != nil {
			return err
		}
		defer dir.Close()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			infos, err := dir.Readdir(listBatch)
			for _, fi := range infos {
				if fi.Name() == "." || fi.Name() == ".." {
					continue
				}
				if strings.HasPrefix(fi.Name(), tempPrefix) {
					continue // an upload of ours that has not been renamed into place
				}
				if err := visit(entryFrom(parent, fi)); err != nil {
					return err
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if len(infos) == 0 {
				return nil
			}
		}
	})
}

func (p *Provider) Stat(ctx context.Context, id string) (provider.Entry, error) {
	rel, err := p.rel(id)
	if err != nil {
		return provider.Entry{}, err
	}
	clean := normalizeID(id)
	var fi os.FileInfo
	err = p.share.use(ctx, ratelimit.Meta, func(fs fileSystem) error {
		var e error
		fi, e = fs.Stat(rel)
		return e
	})
	if err != nil {
		return provider.Entry{}, mapErr(err)
	}
	if clean == "/" {
		e := provider.Entry{ID: RootID, Kind: provider.KindDir, ModTime: fi.ModTime()}
		provider.EnsureVersion(&e)
		return e, nil
	}
	return entryFrom(path.Dir(clean), fi), nil
}

// ReadRange returns n bytes from off. n <= 0 means to the end of the file.
func (p *Provider) ReadRange(ctx context.Context, id, version string, off, n int64) (io.ReadCloser, error) {
	want := n
	if want <= 0 {
		e, err := p.Stat(ctx, id)
		if err != nil {
			return nil, err
		}
		want = e.Size - off
		if want < 0 {
			want = 0
		}
	}
	buf := make([]byte, want)
	got, err := p.ReadRangeAt(ctx, id, version, off, buf)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(buf[:got])), nil
}

// ReadRangeAt fills buf from off on a cached handle. Opening an SMB file is a
// full CREATE round trip, so a block read that had to open its own handle
// would spend as much time on the open as on the data.
//
// A short answer is followed up rather than treated as the end: a server that
// caps a read below the requested length replies with fewer bytes, and only a
// zero-length answer at a live offset means end of file.
func (p *Provider) ReadRangeAt(ctx context.Context, id, version string, off int64, buf []byte) (int, error) {
	rel, err := p.rel(id)
	if err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, errors.New("smb: negative read offset")
	}
	got := 0
	err = p.share.use(ctx, ratelimit.Download, func(fs fileSystem) error {
		got = 0
		h, e := p.handles.lease(fs, rel, version)
		if e != nil {
			return e
		}
		defer h.release()
		for got < len(buf) {
			if e := ctx.Err(); e != nil {
				return e
			}
			n, readErr := h.file.ReadAt(buf[got:], off+int64(got))
			got += n
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				p.handles.drop(rel)
				return readErr
			}
			if n == 0 {
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return got, mapErr(err)
	}
	return got, nil
}

// DownloadURL is unsupported: a URL would carry this process's SMB session.
func (p *Provider) DownloadURL(ctx context.Context, id string) (provider.Link, error) {
	return provider.Link{}, fmt.Errorf("%w: smb has no shareable download URL", provider.ErrUnsupported)
}

func (p *Provider) stagingPaths(parentID, name string) (target, temp string, err error) {
	if !safeName(name) {
		return "", "", fmt.Errorf("smb: unsafe child name %q", name)
	}
	parent := normalizeID(parentID)
	target = path.Join(parent, name)
	temp = path.Join(parent, fmt.Sprintf("%s%s.%d", tempPrefix, name, time.Now().UnixNano()))
	return target, temp, nil
}

// safeName rejects names SMB cannot store, plus the separators and control
// characters that would let a name reach outside its directory.
func safeName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 {
		return false
	}
	if strings.ContainsAny(name, `/\:*?"<>|`+"\x00") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (p *Provider) BeginUpload(ctx context.Context, parentID, name string, size int64, _ provider.Hashes) (provider.UploadSession, error) {
	target, temp, err := p.stagingPaths(parentID, name)
	if err != nil {
		return provider.UploadSession{}, err
	}
	relTemp, err := p.rel(temp)
	if err != nil {
		return provider.UploadSession{}, err
	}
	err = p.share.use(ctx, ratelimit.Upload, func(fs fileSystem) error {
		f, e := fs.Create(relTemp)
		if e != nil {
			return e
		}
		return f.Close()
	})
	if err != nil {
		return provider.UploadSession{}, mapErr(err)
	}
	return provider.UploadSession{
		ID: temp, PartSize: p.caps.PartSize,
		Opaque: map[string]string{
			"temp": temp, "target": target, "size": strconv.FormatInt(size, 10),
		},
	}, nil
}

// UploadPart writes one chunk at its own offset, so parts may arrive in any
// order and a resumed upload can skip the ones already recorded.
func (p *Provider) UploadPart(ctx context.Context, s provider.UploadSession, idx int, r io.Reader, n int64) (provider.PartToken, error) {
	temp := s.Opaque["temp"]
	if temp == "" {
		return provider.PartToken{}, fmt.Errorf("%w: upload session has no staging path", provider.ErrConflict)
	}
	if idx < 0 {
		return provider.PartToken{}, errors.New("smb: invalid part index")
	}
	relTemp, err := p.rel(temp)
	if err != nil {
		return provider.PartToken{}, err
	}
	partSize := s.PartSize
	if partSize <= 0 {
		partSize = p.caps.PartSize
	}
	if n < 0 || n > partSize {
		return provider.PartToken{}, fmt.Errorf("smb: part %d declares %d bytes, above the part size %d", idx, n, partSize)
	}
	chunk, err := readExact(r, n)
	if err != nil {
		return provider.PartToken{}, err
	}
	off := int64(idx) * partSize
	var written int
	err = p.share.use(ctx, ratelimit.Upload, func(fs fileSystem) error {
		f, e := fs.OpenFile(relTemp, os.O_WRONLY, 0o644)
		if e != nil {
			return e
		}
		written, e = f.WriteAt(chunk, off)
		if cerr := f.Close(); e == nil {
			e = cerr
		}
		return e
	})
	if err != nil {
		return provider.PartToken{}, mapErr(err)
	}
	// A short write that the server did not report as an error would
	// otherwise be committed into a truncated file that looks successful.
	if int64(written) != n {
		return provider.PartToken{}, fmt.Errorf("%w: part %d wrote %d of %d bytes",
			provider.ErrTransient, idx, written, n)
	}
	return provider.PartToken{Index: idx, ETag: strconv.FormatInt(n, 10)}, nil
}

// CompleteUpload verifies the staged size and publishes it.
//
// SMB2's rename leaves ReplaceIfExists clear in this client, so an existing
// destination has to be unlinked first. Between the unlink and the rename the
// name does not exist; a reader in that window sees ENOENT rather than the old
// content. There is no way to close it with the operations the protocol
// exposes here, so it is stated instead of papered over.
func (p *Provider) CompleteUpload(ctx context.Context, s provider.UploadSession, parts []provider.PartToken) (provider.Entry, error) {
	temp, target := s.Opaque["temp"], s.Opaque["target"]
	if temp == "" || target == "" {
		return provider.Entry{}, fmt.Errorf("%w: upload session is missing its paths", provider.ErrConflict)
	}
	relTemp, err := p.rel(temp)
	if err != nil {
		return provider.Entry{}, err
	}
	relTarget, err := p.rel(target)
	if err != nil {
		return provider.Entry{}, err
	}
	want, _ := strconv.ParseInt(s.Opaque["size"], 10, 64)
	// A cached read handle would keep serving the file being replaced, and
	// the server refuses to unlink a name this process still holds open.
	p.handles.drop(relTarget)
	var fi os.FileInfo
	err = p.share.use(ctx, ratelimit.Upload, func(fs fileSystem) error {
		st, e := fs.Stat(relTemp)
		if e != nil {
			return e
		}
		if want > 0 && st.Size() != want {
			return fmt.Errorf("%w: staged file is %d bytes, expected %d",
				provider.ErrTransient, st.Size(), want)
		}
		if e := publish(fs, relTemp, relTarget); e != nil {
			return e
		}
		fi, e = fs.Stat(relTarget)
		return e
	})
	if err != nil {
		return provider.Entry{}, mapErr(err)
	}
	return entryFrom(path.Dir(normalizeID(target)), fi), nil
}

// publish moves a staged file onto its final name, unlinking an existing one
// first because this client's rename does not replace.
func publish(fs fileSystem, from, to string) error {
	if err := fs.Rename(from, to); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := fs.Remove(to); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fs.Rename(from, to)
}

// PutFile stores a small file through the same staging-and-publish path, so a
// reader never sees a half-written file under the final name.
func (p *Provider) PutFile(ctx context.Context, parentID, name string, r io.Reader, size int64, _ provider.Hashes) (provider.Entry, error) {
	target, temp, err := p.stagingPaths(parentID, name)
	if err != nil {
		return provider.Entry{}, err
	}
	if size < 0 {
		return provider.Entry{}, errors.New("smb: negative upload size")
	}
	content, err := readExact(r, size)
	if err != nil {
		return provider.Entry{}, err
	}
	relTemp, err := p.rel(temp)
	if err != nil {
		return provider.Entry{}, err
	}
	relTarget, err := p.rel(target)
	if err != nil {
		return provider.Entry{}, err
	}
	p.handles.drop(relTarget)
	var fi os.FileInfo
	err = p.share.use(ctx, ratelimit.Upload, func(fs fileSystem) error {
		f, e := fs.Create(relTemp)
		if e != nil {
			return e
		}
		written := 0
		if len(content) > 0 {
			written, e = f.WriteAt(content, 0)
		}
		if cerr := f.Close(); e == nil {
			e = cerr
		}
		if e == nil && written != len(content) {
			e = fmt.Errorf("%w: wrote %d of %d bytes", provider.ErrTransient, written, len(content))
		}
		if e != nil {
			_ = fs.Remove(relTemp)
			return e
		}
		if e := publish(fs, relTemp, relTarget); e != nil {
			_ = fs.Remove(relTemp)
			return e
		}
		fi, e = fs.Stat(relTarget)
		return e
	})
	if err != nil {
		return provider.Entry{}, mapErr(err)
	}
	return entryFrom(normalizeID(parentID), fi), nil
}

// AbortUpload removes the staging file of an upload that will not complete.
func (p *Provider) AbortUpload(ctx context.Context, s provider.UploadSession) error {
	temp := s.Opaque["temp"]
	if temp == "" {
		return nil
	}
	rel, err := p.rel(temp)
	if err != nil {
		return err
	}
	err = p.share.use(ctx, ratelimit.Upload, func(fs fileSystem) error {
		e := fs.Remove(rel)
		if errors.Is(e, os.ErrNotExist) {
			return nil
		}
		return e
	})
	return mapErr(err)
}

func readExact(r io.Reader, n int64) ([]byte, error) {
	if n < 0 || n > 1<<30 || (r == nil && n != 0) {
		return nil, errors.New("smb: invalid request body")
	}
	if r == nil || n == 0 {
		return []byte{}, nil
	}
	b, err := io.ReadAll(io.LimitReader(r, n+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != n {
		return nil, fmt.Errorf("smb: request body has %d bytes, want %d", len(b), n)
	}
	return b, nil
}

func (p *Provider) Mkdir(ctx context.Context, parentID, name string) (provider.Entry, error) {
	if !safeName(name) {
		return provider.Entry{}, fmt.Errorf("smb: unsafe directory name %q", name)
	}
	parent := normalizeID(parentID)
	target := path.Join(parent, name)
	rel, err := p.rel(target)
	if err != nil {
		return provider.Entry{}, err
	}
	var fi os.FileInfo
	err = p.share.use(ctx, ratelimit.Meta, func(fs fileSystem) error {
		if e := fs.Mkdir(rel, 0o755); e != nil {
			return e
		}
		var e error
		fi, e = fs.Stat(rel)
		return e
	})
	if err != nil {
		return provider.Entry{}, mapErr(err)
	}
	return entryFrom(parent, fi), nil
}

func (p *Provider) Rename(ctx context.Context, id, newName string) (provider.Entry, error) {
	if !safeName(newName) {
		return provider.Entry{}, fmt.Errorf("smb: unsafe name %q", newName)
	}
	clean := normalizeID(id)
	if clean == "/" {
		return provider.Entry{}, errors.New("smb: cannot rename the share root")
	}
	return p.moveTo(ctx, id, path.Join(path.Dir(clean), newName))
}

func (p *Provider) Move(ctx context.Context, id, newParentID string) (provider.Entry, error) {
	clean := normalizeID(id)
	if clean == "/" {
		return provider.Entry{}, errors.New("smb: cannot move the share root")
	}
	return p.moveTo(ctx, id, path.Join(normalizeID(newParentID), path.Base(clean)))
}

func (p *Provider) moveTo(ctx context.Context, id, target string) (provider.Entry, error) {
	relFrom, err := p.rel(id)
	if err != nil {
		return provider.Entry{}, err
	}
	relTo, err := p.rel(target)
	if err != nil {
		return provider.Entry{}, err
	}
	if relFrom == relTo {
		return p.Stat(ctx, target)
	}
	p.handles.drop(relFrom)
	p.handles.drop(relTo)
	var fi os.FileInfo
	err = p.share.use(ctx, ratelimit.Meta, func(fs fileSystem) error {
		// A move must not silently swallow whatever is already at the
		// destination, so unlike an upload publish this does not unlink.
		if e := fs.Rename(relFrom, relTo); e != nil {
			return e
		}
		var e error
		fi, e = fs.Stat(relTo)
		return e
	})
	if err != nil {
		return provider.Entry{}, mapErr(err)
	}
	return entryFrom(path.Dir(normalizeID(target)), fi), nil
}

func (p *Provider) Delete(ctx context.Context, id string) error {
	clean := normalizeID(id)
	if clean == "/" {
		return errors.New("smb: refusing to delete the share root")
	}
	rel, err := p.rel(id)
	if err != nil {
		return err
	}
	p.handles.drop(rel)
	err = p.share.use(ctx, ratelimit.Meta, func(fs fileSystem) error {
		e := fs.Remove(rel)
		if e == nil || errors.Is(e, os.ErrNotExist) {
			return e
		}
		fi, se := fs.Stat(rel)
		if se != nil {
			return se
		}
		if !fi.IsDir() {
			return e
		}
		return removeTree(fs, rel, 0)
	})
	return mapErr(err)
}

// maxDeleteDepth bounds recursion so a symlink loop or a server that reports a
// directory as its own child cannot spin here forever.
const maxDeleteDepth = 128

// removeTree deletes a directory depth-first, since SMB only removes empty
// directories.
func removeTree(fs fileSystem, rel string, depth int) error {
	if depth > maxDeleteDepth {
		return fmt.Errorf("smb: directory nesting exceeds %d levels under %q", maxDeleteDepth, rel)
	}
	infos, err := fs.ReadDir(rel)
	if err != nil {
		return err
	}
	for _, fi := range infos {
		if fi.Name() == "." || fi.Name() == ".." {
			continue
		}
		child := rel + "/" + fi.Name()
		if fi.IsDir() {
			if err := removeTree(fs, child, depth+1); err != nil {
				return err
			}
			continue
		}
		if err := fs.Remove(child); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return fs.Remove(rel)
}

// entryFrom converts a stat result into an Entry.
func entryFrom(parent string, fi os.FileInfo) provider.Entry {
	kind := provider.KindFile
	size := fi.Size()
	if fi.IsDir() {
		kind, size = provider.KindDir, 0
	}
	id := path.Join(parent, fi.Name())
	if parent == "" {
		id = "/" + fi.Name()
	}
	e := provider.Entry{
		ID: id, ParentID: parent, Name: fi.Name(),
		Kind: kind, Size: size, ModTime: fi.ModTime(),
	}
	// SMB reports no change token, so the fingerprint is size and mtime.
	provider.EnsureVersion(&e)
	return e
}

// mapErr translates SMB failures into the sentinels internal/net/retry
// classifies, so the upload queue and the VFS behave the same on every
// backend. The client library already maps the NTSTATUS values that matter
// onto the os sentinels.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{
		provider.ErrNotFound, provider.ErrExists, provider.ErrTransient,
		provider.ErrAuth, provider.ErrConflict, provider.ErrUnsupported,
	} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%w: %v", provider.ErrNotFound, err)
	case errors.Is(err, os.ErrExist):
		return fmt.Errorf("%w: %v", provider.ErrExists, err)
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("%w: %v", provider.ErrAuth, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return fmt.Errorf("%w: %v", provider.ErrTransient, err)
	}
	if isConnectionLoss(err) {
		return fmt.Errorf("%w: %v", provider.ErrTransient, err)
	}
	return err
}

// isConnectionLoss reports a dropped session, which is worth retrying on a
// fresh connection rather than surfacing as a permanent failure.
func isConnectionLoss(err error) bool {
	msg := err.Error()
	for _, marker := range []string{
		"connection error", "broken pipe", "connection reset",
		"use of closed network connection", "unexpected EOF",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// handleCache keeps one open read handle per path. Opening an SMB file is a
// full CREATE round trip, so re-opening for every 64 KiB sub-block read would
// triple the cost of a cold random read.
//
// A handle is pinned while a read is using it: dropping it on a rename or a
// delete must not close a file another goroutine is mid-read on, so the close
// happens when the last reader releases it.
type handleCache struct {
	mu      sync.Mutex
	entries map[string]*cachedHandle
	opens   int64
	closes  int64
}

type cachedHandle struct {
	cache   *handleCache
	file    fileHandle
	version string
	refs    int
	evicted bool
}

func newHandleCache() *handleCache {
	return &handleCache{entries: make(map[string]*cachedHandle)}
}

// lease returns a pinned handle for rel, opening one if needed. A handle whose
// version no longer matches is replaced: it refers to content that has been
// overwritten.
func (c *handleCache) lease(fs fileSystem, rel, version string) (*cachedHandle, error) {
	c.mu.Lock()
	if h, ok := c.entries[rel]; ok {
		if h.version == version {
			h.refs++
			c.mu.Unlock()
			return h, nil
		}
		stale := c.evictLocked(rel)
		c.mu.Unlock()
		closeAll(stale)
	} else {
		c.mu.Unlock()
	}

	f, err := fs.Open(rel)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.opens++
	// Another goroutine may have opened the same path meanwhile; keep one and
	// close the loser rather than leaking a handle.
	if existing, ok := c.entries[rel]; ok && existing.version == version {
		existing.refs++
		c.closes++
		c.mu.Unlock()
		f.Close()
		return existing, nil
	}
	h := &cachedHandle{cache: c, file: f, version: version, refs: 1}
	c.entries[rel] = h
	c.mu.Unlock()
	return h, nil
}

func (h *cachedHandle) release() {
	c := h.cache
	c.mu.Lock()
	file := c.retireLocked(h)
	c.mu.Unlock()
	if file != nil {
		file.Close()
	}
}

// retireLocked closes an evicted handle once no reader holds it, returning the
// file so the caller can close it outside the lock. Callers hold c.mu.
func (c *handleCache) retireLocked(h *cachedHandle) fileHandle {
	h.refs--
	if h.refs > 0 || !h.evicted || h.file == nil {
		return nil
	}
	file := h.file
	h.file = nil
	c.closes++
	return file
}

// evictLocked removes a path from the cache and returns any handle that is
// free to close. Callers hold c.mu.
func (c *handleCache) evictLocked(rel string) []fileHandle {
	h, ok := c.entries[rel]
	if !ok {
		return nil
	}
	delete(c.entries, rel)
	h.evicted = true
	if h.refs > 0 || h.file == nil {
		// A reader still holds it; release closes it when the last one leaves.
		return nil
	}
	file := h.file
	h.file = nil
	c.closes++
	return []fileHandle{file}
}

func closeAll(files []fileHandle) {
	for _, f := range files {
		f.Close()
	}
}

// drop evicts the handle for a path that is about to be replaced or removed.
func (c *handleCache) drop(rel string) {
	c.mu.Lock()
	stale := c.evictLocked(rel)
	c.mu.Unlock()
	closeAll(stale)
}

// purge drops every handle; the session calls it when a connection is lost,
// because the handles it held are gone with it.
func (c *handleCache) purge() {
	c.mu.Lock()
	var stale []fileHandle
	for key := range c.entries {
		stale = append(stale, c.evictLocked(key)...)
	}
	c.mu.Unlock()
	closeAll(stale)
}

func (c *handleCache) closeAll() { c.purge() }

func (c *handleCache) stats() (int64, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opens, c.closes
}

var (
	_ provider.Provider      = (*Provider)(nil)
	_ provider.StreamLister  = (*Provider)(nil)
	_ provider.SinglePutter  = (*Provider)(nil)
	_ provider.RangeReaderAt = (*Provider)(nil)
)
