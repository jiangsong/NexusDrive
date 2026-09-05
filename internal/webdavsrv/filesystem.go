package webdavsrv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"cloudfs/internal/vfs"

	"golang.org/x/net/webdav"
)

var _ webdav.FileSystem = (*davFS)(nil)
var _ webdav.File = (*davFile)(nil)

// davFS projects one VFS subtree as a DAV namespace. writer is nil on a
// read-only endpoint, and every mutating method checks it: the method gate in
// front of the handler already rejects the write verbs, so this is the second
// place the same decision is enforced rather than the only one.
type davFS struct {
	backend Backend
	writer  WriteBackend
	root    string
}

func (f *davFS) resolve(name string) (string, error) {
	if strings.ContainsAny(name, "\x00\\") {
		return "", fs.ErrInvalid
	}
	clean := path.Clean("/" + strings.TrimPrefix(name, "/"))
	if f.root == "/" {
		return clean, nil
	}
	if clean == "/" {
		return f.root, nil
	}
	return path.Join(f.root, strings.TrimPrefix(clean, "/")), nil
}

// parentOf returns the directory holding virtual and the child's name. The
// exported root has no parent inside this namespace, so a request to remove,
// rename or replace it is refused here rather than reaching the VFS.
func (f *davFS) parentOf(ctx context.Context, virtual string) (uint64, string, error) {
	if virtual == f.root || virtual == "/" {
		return 0, "", fs.ErrPermission
	}
	attr, err := f.backend.StatPath(ctx, path.Dir(virtual))
	if err != nil {
		return 0, "", mapError(err)
	}
	if !attr.IsDir {
		return 0, "", syscall.ENOTDIR
	}
	return attr.Ino, path.Base(virtual), nil
}

func (f *davFS) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	if f.writer == nil {
		return fs.ErrPermission
	}
	virtual, err := f.resolve(name)
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: name, Err: err}
	}
	parent, base, err := f.parentOf(ctx, virtual)
	if err != nil {
		return &os.PathError{Op: "mkdir", Path: name, Err: notePermissionDenied(ctx, err)}
	}
	if _, err := f.writer.Mkdir(ctx, parent, base); err != nil {
		return refused(ctx, "mkdir", name, err)
	}
	return nil
}

// writeFlags are the open modes that mean the caller intends to modify.
const writeFlags = os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_EXCL | os.O_TRUNC

func (f *davFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	virtual, err := f.resolve(name)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	if flag&writeFlags == 0 {
		return f.openForRead(ctx, name, virtual)
	}
	if f.writer == nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	// WebDAV has no append verb, and the VFS write path addresses bytes by
	// offset; an O_APPEND open would have no defined meaning here.
	if flag&os.O_APPEND != 0 {
		return nil, &os.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	return f.openForWrite(ctx, name, virtual, flag)
}

func (f *davFS) openForRead(ctx context.Context, name, virtual string) (webdav.File, error) {
	attr, err := f.backend.StatPath(ctx, virtual)
	if err != nil {
		return nil, pathError("open", name, err)
	}
	file := &davFile{ctx: ctx, backend: f.backend, path: virtual, attr: attr}
	if !attr.IsDir {
		file.handle, err = f.backend.Open(ctx, attr.Ino, false)
		if err != nil {
			return nil, pathError("open", name, err)
		}
	}
	return file, nil
}

func (f *davFS) openForWrite(ctx context.Context, name, virtual string, flag int) (webdav.File, error) {
	parent, base, err := f.parentOf(ctx, virtual)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: notePermissionDenied(ctx, err)}
	}
	attr, statErr := f.backend.StatPath(ctx, virtual)
	switch {
	case statErr == nil && flag&os.O_EXCL != 0:
		return nil, &os.PathError{Op: "open", Path: name, Err: fs.ErrExist}
	case statErr == nil && attr.IsDir:
		return nil, &os.PathError{Op: "open", Path: name, Err: syscall.EISDIR}
	case statErr == nil:
		handle, err := f.writer.Open(ctx, attr.Ino, true)
		if err != nil {
			return nil, refused(ctx, "open", name, err)
		}
		if flag&os.O_TRUNC != 0 {
			if err := f.writer.Truncate(ctx, handle, 0); err != nil {
				_ = f.writer.Release(ctx, handle)
				return nil, refused(ctx, "open", name, err)
			}
			attr.Size = 0
		}
		return &davFile{ctx: ctx, backend: f.backend, writer: f.writer, path: virtual,
			attr: attr, handle: handle, write: true, size: attr.Size}, nil
	case errors.Is(mapError(statErr), fs.ErrNotExist) && flag&os.O_CREATE != 0:
		handle, err := f.writer.Create(ctx, parent, base)
		if err != nil {
			return nil, refused(ctx, "open", name, err)
		}
		created := f.backend.HandleAttr(ctx, handle)
		return &davFile{ctx: ctx, backend: f.backend, writer: f.writer, path: virtual,
			attr: created, handle: handle, write: true}, nil
	default:
		return nil, pathError("open", name, statErr)
	}
}

func (f *davFS) RemoveAll(ctx context.Context, name string) error {
	if f.writer == nil {
		return fs.ErrPermission
	}
	virtual, err := f.resolve(name)
	if err != nil {
		return &os.PathError{Op: "remove", Path: name, Err: err}
	}
	parent, base, err := f.parentOf(ctx, virtual)
	if err != nil {
		return &os.PathError{Op: "remove", Path: name, Err: notePermissionDenied(ctx, err)}
	}
	// Recursive: DELETE on a collection is defined to take the whole subtree,
	// and a client that meant otherwise has no way to say so.
	if err := f.writer.Remove(ctx, parent, base, true); err != nil {
		return refused(ctx, "remove", name, err)
	}
	return nil
}

func (f *davFS) Rename(ctx context.Context, oldName, newName string) error {
	if f.writer == nil {
		return fs.ErrPermission
	}
	oldVirtual, err := f.resolve(oldName)
	if err != nil {
		return &os.PathError{Op: "rename", Path: oldName, Err: err}
	}
	newVirtual, err := f.resolve(newName)
	if err != nil {
		return &os.PathError{Op: "rename", Path: newName, Err: err}
	}
	oldParent, oldBase, err := f.parentOf(ctx, oldVirtual)
	if err != nil {
		return &os.PathError{Op: "rename", Path: oldName, Err: err}
	}
	newParent, newBase, err := f.parentOf(ctx, newVirtual)
	if err != nil {
		return &os.PathError{Op: "rename", Path: newName, Err: err}
	}
	if err := f.writer.Rename(ctx, oldParent, oldBase, newParent, newBase); err != nil {
		return refused(ctx, "rename", oldName, err)
	}
	return nil
}

func (f *davFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	virtual, err := f.resolve(name)
	if err != nil {
		return nil, &os.PathError{Op: "stat", Path: name, Err: err}
	}
	attr, err := f.backend.StatPath(ctx, virtual)
	if err != nil {
		return nil, pathError("stat", name, err)
	}
	return fileInfo{attr: attr, fallbackName: path.Base(name)}, nil
}

type davFile struct {
	mu      sync.Mutex
	ctx     context.Context
	backend Backend
	writer  WriteBackend
	path    string
	attr    vfs.Attr
	handle  *vfs.Handle
	offset  int64
	size    int64
	write   bool
	dir     []vfs.Attr
	dirPos  int
	closed  bool
}

// Close releases the handle. For a write handle this is where the VFS commits
// the staged bytes, so its error is the one that tells a client the PUT did
// not land — it must not be swallowed.
func (f *davFile) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	handle := f.handle
	f.mu.Unlock()
	if handle != nil {
		return f.backend.Release(f.ctx, handle)
	}
	return nil
}

func (f *davFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	if f.attr.IsDir {
		return 0, &os.PathError{Op: "read", Path: f.path, Err: syscall.EISDIR}
	}
	n, err := f.backend.Read(f.ctx, f.handle, p, f.offset)
	f.offset += int64(n)
	return n, mapError(err)
}

func (f *davFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	if !f.write || f.writer == nil {
		return 0, fs.ErrPermission
	}
	n, err := f.writer.Write(f.ctx, f.handle, p, f.offset)
	f.offset += int64(n)
	if f.offset > f.size {
		f.size = f.offset
	}
	return n, mapError(err)
}

func (f *davFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = f.offset + offset
	case io.SeekEnd:
		next = f.currentSize() + offset
	default:
		return 0, fs.ErrInvalid
	}
	if next < 0 {
		return 0, fs.ErrInvalid
	}
	f.offset = next
	return next, nil
}

// currentSize is the size a client should see. While a write is open the
// committed attr is stale, so the bytes accepted so far are what counts.
func (f *davFile) currentSize() int64 {
	if f.write && f.size > f.attr.Size {
		return f.size
	}
	return f.attr.Size
}

func (f *davFile) Readdir(count int) ([]os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, fs.ErrClosed
	}
	if !f.attr.IsDir {
		return nil, &os.PathError{Op: "readdir", Path: f.path, Err: syscall.ENOTDIR}
	}
	if f.dir == nil {
		entries, err := f.backend.ReadDirPath(f.ctx, f.path)
		if err != nil {
			return nil, pathError("readdir", f.path, err)
		}
		f.dir = entries
	}
	if f.dirPos >= len(f.dir) {
		if count > 0 {
			return nil, io.EOF
		}
		return []os.FileInfo{}, nil
	}
	end := len(f.dir)
	if count > 0 && f.dirPos+count < end {
		end = f.dirPos + count
	}
	out := make([]os.FileInfo, 0, end-f.dirPos)
	for _, attr := range f.dir[f.dirPos:end] {
		out = append(out, fileInfo{attr: attr})
	}
	f.dirPos = end
	return out, nil
}

func (f *davFile) Stat() (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, fs.ErrClosed
	}
	attr := f.attr
	attr.Size = f.currentSize()
	return fileInfo{attr: attr, fallbackName: path.Base(f.path), uncommitted: f.write}, nil
}

type fileInfo struct {
	attr         vfs.Attr
	fallbackName string
	// uncommitted marks a file whose open write has not been committed, so the
	// version on the node still describes the previous content.
	uncommitted bool
}

func (i fileInfo) Name() string {
	if i.attr.Name != "" {
		return i.attr.Name
	}
	return i.fallbackName
}
func (i fileInfo) Size() int64 { return i.attr.Size }
func (i fileInfo) Mode() os.FileMode {
	mode := os.FileMode(i.attr.Mode) & os.ModePerm
	if i.attr.IsDir {
		if mode == 0 {
			mode = 0o555
		}
		return mode | os.ModeDir
	}
	if mode == 0 {
		mode = 0o444
	}
	return mode
}
func (i fileInfo) ModTime() time.Time { return i.attr.MTime }
func (i fileInfo) IsDir() bool        { return i.attr.IsDir }
func (i fileInfo) Sys() any           { return nil }

// ETag hashes the provider version instead of exposing its opaque value: some
// providers use signed or account-specific identifiers. The hash still gives
// WebDAV clients a stable conditional-read validator.
//
// A file with an open, uncommitted write has no usable validator: the node
// still carries the previous content's version, and handing that out would
// label new bytes with the old file's ETag. No ETag is better than a wrong
// one, so none is emitted until the write commits.
func (i fileInfo) ETag(context.Context) (string, error) {
	if i.attr.IsDir || i.attr.Version == "" || i.uncommitted {
		return "", webdav.ErrNotImplemented
	}
	sum := sha256.Sum256([]byte(i.attr.Remote + "\x00" + i.attr.Version))
	return `"` + hex.EncodeToString(sum[:16]) + `"`, nil
}

func pathError(op, name string, err error) error {
	return &os.PathError{Op: op, Path: name, Err: mapError(err)}
}

// refused is pathError for a mutating call: it also records a policy refusal,
// so the status the DAV handler picks can be corrected on the way out.
func refused(ctx context.Context, op, name string, err error) error {
	mapped := mapError(err)
	notePermissionDenied(ctx, mapped)
	return &os.PathError{Op: op, Path: name, Err: mapped}
}

func mapError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, vfs.ErrNotFound):
		return fs.ErrNotExist
	case errors.Is(err, vfs.ErrExists):
		return fs.ErrExist
	case errors.Is(err, vfs.ErrReadOnly):
		return fs.ErrPermission
	case errors.Is(err, vfs.ErrIsDir):
		return syscall.EISDIR
	case errors.Is(err, vfs.ErrNotDir):
		return syscall.ENOTDIR
	default:
		return err
	}
}
