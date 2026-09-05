package sftp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"strings"
	"unicode/utf8"

	psftp "github.com/pkg/sftp"

	"cloudfs/internal/provider"
)

// The directory-only codec implements SFTP v3 (filexfer-02). It owns an
// exclusive, already authenticated subsystem stream, never a pkg/sftp
// client's active channel. Each response is bounded before allocation.
const maxDirectoryPacket = 1 << 20

const (
	dirInit          = 1
	dirVersion       = 2
	dirClose         = 4
	dirOpen          = 11
	dirRead          = 12
	dirRealPath      = 16
	dirStat          = 17
	dirStatus        = 101
	dirHandle        = 102
	dirName          = 104
	dirAttrs         = 105
	dirRequiredAttrs = 1 | 4 | 8 // size, permissions/type, atime/mtime
)

var errDirectoryPacket = errors.New("sftp: malformed directory response")

type directoryWire struct {
	rw io.ReadWriter
	id uint32
}

// scanDirectoryStream takes ownership of rw, including on error. Close must
// be concurrency-safe and unblock IO. Cancellation closes only this exclusive
// stream, and the callback runs serially with backpressure. Nothing retries
// an emitted prefix. Success requires protocol EOF and acknowledged CLOSE;
// transport EOF is always an incomplete listing.
func scanDirectoryStream(ctx context.Context, rw io.ReadWriteCloser, remotePath, parentID string, visit func(provider.Entry) error) (err error) {
	return scanDirectoryResolved(ctx, rw, parentID, visit, func(*directoryWire) (string, error) {
		return remotePath, nil
	})
}

// scanDirectoryAtRoot resolves relative roots inside the exclusive subsystem,
// so cancellation never needs to close a shared file-transfer connection.
func scanDirectoryAtRoot(ctx context.Context, rw io.ReadWriteCloser, root, parentID string, visit func(provider.Entry) error) error {
	return scanDirectoryResolved(ctx, rw, parentID, visit, func(w *directoryWire) (string, error) {
		if !strings.HasPrefix(root, "/") {
			wd, err := w.realpath()
			if err != nil {
				return "", err
			}
			root = path.Join(wd, strings.TrimPrefix(strings.TrimPrefix(root, "~/"), "~"))
		}
		return path.Join(root, path.Clean("/"+strings.TrimPrefix(parentID, "/"))), nil
	})
}

func scanDirectoryResolved(ctx context.Context, rw io.ReadWriteCloser, parentID string, visit func(provider.Entry) error, resolve func(*directoryWire) (string, error)) (err error) {
	if rw == nil {
		return errors.New("sftp: nil directory stream")
	}
	defer rw.Close()
	if visit == nil {
		return errors.New("sftp: invalid directory scan arguments")
	}
	parentID = path.Clean("/" + strings.TrimPrefix(parentID, "/"))
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		rw.Close()
		close(closed)
	})
	defer func() {
		if !stop() {
			<-closed
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	w := &directoryWire{rw: rw}
	if err := w.initialize(); err != nil {
		return err
	}
	remotePath, err := resolve(w)
	if err != nil {
		return err
	}
	if remotePath == "" || strings.ContainsRune(remotePath, 0) || len(remotePath) > maxDirectoryPacket-16 {
		return errors.New("sftp: invalid directory path")
	}
	typ, body, err := w.request(dirOpen, directoryString(nil, remotePath))
	if err != nil {
		return err
	}
	if typ == dirStatus {
		return directoryStatusError(body, false)
	}
	if typ != dirHandle {
		return errDirectoryPacket
	}
	f := directoryFields{b: body}
	handle := f.str()
	if f.done() != nil || len(handle) > 256 {
		return errDirectoryPacket
	}
	encodedHandle := directoryString(nil, string(handle))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		typ, body, err := w.request(dirRead, encodedHandle)
		if err != nil {
			return err
		}
		if typ == dirStatus {
			code, err := directoryStatusCode(body)
			if err != nil {
				return err
			}
			if code != 1 { // Only SSH_FX_EOF completes directory enumeration.
				return directoryStatusError(body, false)
			}
			break
		}
		if typ != dirName {
			return errDirectoryPacket
		}
		f = directoryFields{b: body}
		count := f.u32()
		// Each member needs at least filename/longname lengths and flags.
		if count == 0 || uint64(count) > uint64(len(f.b))/12 {
			return errDirectoryPacket
		}
		for range count {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := string(f.str())
			f.str() // Human-oriented longname is not an identity or metadata.
			attrs, flags := f.attrs()
			if f.err != nil {
				return f.err
			}
			if name == "." || name == ".." {
				continue
			}
			if name == "" || len(name) > 4096 || !utf8.ValidString(name) || strings.ContainsAny(name, "/\x00") {
				return errDirectoryPacket
			}
			if strings.HasPrefix(name, tempPrefix) {
				continue
			}
			if flags&dirRequiredAttrs != dirRequiredAttrs {
				attrs, err = w.stat(path.Join(remotePath, name))
				if err != nil {
					return err
				}
			}
			if attrs.Size > math.MaxInt64 {
				return errDirectoryPacket
			}
			e := provider.Entry{ID: path.Join(parentID, name), ParentID: parentID, Name: name, Size: int64(attrs.Size), ModTime: attrs.ModTime()}
			if attrs.FileMode().IsDir() {
				e.Kind = provider.KindDir
			}
			provider.EnsureVersion(&e)
			if err := visit(e); err != nil {
				return err
			}
		}
		if err := f.done(); err != nil {
			return err
		}
	}
	typ, body, err = w.request(dirClose, encodedHandle)
	if err != nil {
		return err
	}
	if typ != dirStatus {
		return errDirectoryPacket
	}
	return directoryStatusError(body, true)
}

// REALPATH "." is the v3 working-directory operation used by pkg/sftp.Getwd.
// It returns exactly one NAME; attributes may be absent for this operation.
func (w *directoryWire) realpath() (string, error) {
	typ, body, err := w.request(dirRealPath, directoryString(nil, "."))
	if err != nil {
		return "", err
	}
	if typ == dirStatus {
		return "", directoryStatusError(body, false)
	}
	if typ != dirName {
		return "", errDirectoryPacket
	}
	f := directoryFields{b: body}
	count := f.u32()
	name := string(f.str())
	f.str()
	f.attrs()
	if f.done() != nil || count != 1 || !path.IsAbs(name) || !utf8.ValidString(name) || strings.ContainsRune(name, 0) || len(name) > maxDirectoryPacket-16 {
		return "", errDirectoryPacket
	}
	return path.Clean(name), nil
}

func (w *directoryWire) initialize() error {
	if err := writeDirectoryPacket(w.rw, []byte{dirInit, 0, 0, 0, 3}); err != nil {
		return err
	}
	b, err := readDirectoryPacket(w.rw)
	if err != nil {
		return err
	}
	if len(b) < 5 || b[0] != dirVersion {
		return errDirectoryPacket
	}
	f := directoryFields{b: b[1:]}
	if f.u32() != 3 {
		return fmt.Errorf("sftp: directory stream requires version 3: %w", provider.ErrUnsupported)
	}
	for len(f.b) > 0 && f.err == nil {
		f.str() // extension name
		f.str() // extension data
	}
	return f.done()
}

func (w *directoryWire) request(typ byte, payload []byte) (byte, []byte, error) {
	if w.id == math.MaxUint32 {
		return 0, nil, errors.New("sftp: directory request IDs exhausted")
	}
	w.id++
	b := []byte{typ}
	b = binary.BigEndian.AppendUint32(b, w.id)
	b = append(b, payload...)
	if err := writeDirectoryPacket(w.rw, b); err != nil {
		return 0, nil, err
	}
	b, err := readDirectoryPacket(w.rw)
	if err != nil {
		return 0, nil, err
	}
	if len(b) < 5 || binary.BigEndian.Uint32(b[1:5]) != w.id {
		return 0, nil, errDirectoryPacket
	}
	return b[0], b[5:], nil
}

func (w *directoryWire) stat(name string) (psftp.FileStat, error) {
	typ, body, err := w.request(dirStat, directoryString(nil, name))
	if err != nil {
		return psftp.FileStat{}, err
	}
	if typ == dirStatus {
		return psftp.FileStat{}, directoryStatusError(body, false)
	}
	if typ != dirAttrs {
		return psftp.FileStat{}, errDirectoryPacket
	}
	f := directoryFields{b: body}
	attrs, flags := f.attrs()
	if f.done() != nil || flags&dirRequiredAttrs != dirRequiredAttrs {
		return psftp.FileStat{}, errDirectoryPacket
	}
	return attrs, nil
}

func readDirectoryPacket(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("sftp: incomplete directory packet header: %w", err)
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > maxDirectoryPacket {
		return nil, errDirectoryPacket
	}
	b := make([]byte, int(n))
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, fmt.Errorf("sftp: incomplete directory packet: %w", err)
	}
	return b, nil
}

func writeDirectoryPacket(w io.Writer, payload []byte) error {
	if len(payload) == 0 || len(payload) > maxDirectoryPacket {
		return errDirectoryPacket
	}
	b := binary.BigEndian.AppendUint32(nil, uint32(len(payload)))
	b = append(b, payload...)
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}

func directoryString(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

type directoryFields struct {
	b   []byte
	err error
}

func (f *directoryFields) take(n uint32) []byte {
	if f.err != nil || uint64(n) > uint64(len(f.b)) {
		f.err = errDirectoryPacket
		return nil
	}
	b := f.b[:n]
	f.b = f.b[n:]
	return b
}

func (f *directoryFields) u32() uint32 {
	b := f.take(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func (f *directoryFields) str() []byte { return f.take(f.u32()) }

func (f *directoryFields) done() error {
	if f.err != nil || len(f.b) != 0 {
		return errDirectoryPacket
	}
	return nil
}

func (f *directoryFields) attrs() (psftp.FileStat, uint32) {
	flags := f.u32()
	var a psftp.FileStat
	if flags&^uint32(0x8000000f) != 0 {
		f.err = errDirectoryPacket
		return a, flags
	}
	if flags&1 != 0 {
		b := f.take(8)
		if b != nil {
			a.Size = binary.BigEndian.Uint64(b)
		}
	}
	if flags&2 != 0 {
		a.UID, a.GID = f.u32(), f.u32()
	}
	if flags&4 != 0 {
		a.Mode = f.u32()
	}
	if flags&8 != 0 {
		a.Atime, a.Mtime = f.u32(), f.u32()
	}
	if flags&0x80000000 != 0 {
		count := f.u32()
		if uint64(count) > uint64(len(f.b))/8 {
			f.err = errDirectoryPacket
			return a, flags
		}
		for range count {
			f.str()
			f.str()
			if f.err != nil {
				break
			}
		}
	}
	return a, flags
}

func directoryStatusCode(b []byte) (uint32, error) {
	f := directoryFields{b: b}
	code := f.u32()
	f.str() // Do not persist arbitrary server messages or language tags.
	f.str()
	return code, f.done()
}

func directoryStatusError(b []byte, allowOK bool) error {
	code, err := directoryStatusCode(b)
	if err != nil {
		return err
	}
	if code == 0 {
		if allowOK {
			return nil
		}
		return errDirectoryPacket // OK cannot stand in for HANDLE/NAME/ATTRS.
	}
	return mapErr(&psftp.StatusError{Code: code})
}
