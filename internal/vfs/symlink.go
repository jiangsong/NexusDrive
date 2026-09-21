package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"

	"cloudfs/internal/provider"
)

// The wire format matches rclone: the object holds the target verbatim.
// This suffix is reserved in the virtual namespace, including directories.
const symlinkSuffix = ".rclonelink"
const maxLinkTarget = 4095

func remoteName(name string, kind provider.Kind) string {
	if kind == provider.KindSymlink {
		return name + symlinkSuffix
	}
	return name
}

func checkLinkName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || strings.HasSuffix(name, symlinkSuffix) {
		return fmt.Errorf("vfs: invalid or reserved link name %q: %w", name, syscall.EINVAL)
	}
	return nil
}

// Symlink stores the target without resolving it. Relative, absolute and
// dangling targets have the same semantics as local filesystem links.
func (f *FS) Symlink(ctx context.Context, parent uint64, name, target string) (Attr, error) {
	if target == "" || strings.ContainsRune(target, 0) {
		return Attr{}, syscall.EINVAL
	}
	if len(target) > maxLinkTarget || len(name)+len(symlinkSuffix) > 255 {
		return Attr{}, syscall.ENAMETOOLONG
	}
	h, err := f.create(ctx, parent, name, provider.KindSymlink)
	if err != nil {
		return Attr{}, err
	}
	defer f.Release(context.Background(), h)
	written, err := f.Write(ctx, h, []byte(target), 0)
	if err == nil && written != len(target) {
		err = io.ErrShortWrite
	}
	if err != nil {
		// Nothing was acknowledged or journaled. Do not let Release commit
		// a partial target, or leave an unusable link occupying the name.
		h.mu.Lock()
		h.writer.dirty = false
		h.mu.Unlock()
		cleanup := context.Background()
		releaseErr := f.Release(cleanup, h)
		removeErr := f.meta.Remove(cleanup, h.Ino)
		f.invalidateEntryFrom(cleanup, parent, name)
		return Attr{}, errors.Join(err, releaseErr, removeErr)
	}
	if err = f.Sync(ctx, h); err != nil {
		return Attr{}, err
	}
	return f.Stat(ctx, h.Ino)
}

// Readlink reads the link object, never its target. Kernel path traversal
// resolves it; non-kernel adapters must not follow it outside their scope.
func (f *FS) Readlink(ctx context.Context, ino uint64) ([]byte, error) {
	h, err := f.Open(ctx, ino, false)
	if err != nil {
		return nil, err
	}
	defer f.Release(context.Background(), h)
	if h.Node.Kind != provider.KindSymlink {
		return nil, syscall.EINVAL
	}
	if h.Node.Size < 1 || h.Node.Size > maxLinkTarget {
		return nil, syscall.EINVAL
	}
	b := make([]byte, h.Node.Size)
	n, err := f.Read(ctx, h, b, 0)
	if err != nil {
		return nil, err
	}
	if n != len(b) || strings.ContainsRune(string(b), 0) {
		return nil, syscall.EINVAL
	}
	return b, nil
}
