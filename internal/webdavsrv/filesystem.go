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

var _ webdav.FileSystem = (*readOnlyFS)(nil)
var _ webdav.File = (*davFile)(nil)

type readOnlyFS struct {
	backend Backend
	root    string
}

func (f *readOnlyFS) resolve(name string) (string, error) {
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

func (f *readOnlyFS) Mkdir(context.Context, string, os.FileMode) error {
	return fs.ErrPermission
}

func (f *readOnlyFS) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_EXCL|os.O_TRUNC) != 0 {
		return nil, &os.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	virtual, err := f.resolve(name)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
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

func (f *readOnlyFS) RemoveAll(context.Context, string) error { return fs.ErrPermission }
func (f *readOnlyFS) Rename(context.Context, string, string) error {
	return fs.ErrPermission
}

func (f *readOnlyFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
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
	path    string
	attr    vfs.Attr
	handle  *vfs.Handle
	offset  int64
	dir     []vfs.Attr
	dirPos  int
	closed  bool
}

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

func (f *davFile) Write([]byte) (int, error) { return 0, fs.ErrPermission }

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
		next = f.attr.Size + offset
	default:
		return 0, fs.ErrInvalid
	}
	if next < 0 {
		return 0, fs.ErrInvalid
	}
	f.offset = next
	return next, nil
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
	return fileInfo{attr: f.attr, fallbackName: path.Base(f.path)}, nil
}

type fileInfo struct {
	attr         vfs.Attr
	fallbackName string
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
func (i fileInfo) ETag(context.Context) (string, error) {
	if i.attr.IsDir || i.attr.Version == "" {
		return "", webdav.ErrNotImplemented
	}
	sum := sha256.Sum256([]byte(i.attr.Remote + "\x00" + i.attr.Version))
	return `"` + hex.EncodeToString(sum[:16]) + `"`, nil
}

func pathError(op, name string, err error) error {
	return &os.PathError{Op: op, Path: name, Err: mapError(err)}
}

func mapError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, vfs.ErrNotFound):
		return fs.ErrNotExist
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
