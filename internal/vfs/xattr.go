package vfs

import (
	"context"

	"cloudfs/internal/config"
	"cloudfs/internal/meta"
)

// Extended attributes, held locally for a node and never sent to a backend.
//
// They are here rather than in the FUSE adapter because the decision is a
// filesystem one: which subtrees accept writes, and what a value costs. The
// adapter only turns the answers into errnos.
//
// Why they are stored at all: macOS attaches them to everything it copies,
// and copyfile(3) — the engine behind the Finder, cp -p and ditto — treats a
// refused setxattr as a failed copy. Why they are not uploaded: no backend
// has a place for a com.apple.* value, and the kernel's own fallback is an
// AppleDouble "._" sidecar, which would put a junk file on the cloud drive
// for every copied file and show it on every other device.

// SetXattr stores one extended attribute on a node.
func (f *FS) SetXattr(ctx context.Context, ino uint64, name string, value []byte) error {
	if err := f.xattrWritable(ctx, ino); err != nil {
		return err
	}
	return f.meta.SetXattr(ctx, ino, name, value)
}

// Xattr reads one, reporting meta.ErrNoXattr when the node has none by that
// name.
func (f *FS) Xattr(ctx context.Context, ino uint64, name string) ([]byte, error) {
	return f.meta.Xattr(ctx, ino, name)
}

// XattrNames lists the attributes held for a node.
func (f *FS) XattrNames(ctx context.Context, ino uint64) ([]string, error) {
	return f.meta.XattrNames(ctx, ino)
}

// RemoveXattr drops one.
func (f *FS) RemoveXattr(ctx context.Context, ino uint64, name string) error {
	if err := f.xattrWritable(ctx, ino); err != nil {
		return err
	}
	return f.meta.RemoveXattr(ctx, ino, name)
}

// xattrWritable refuses a change inside a read-only subtree. The mode means
// nothing in it changes, and an attribute is a change to the file as the
// person sees it — the Finder shows tags and the quarantine flag.
func (f *FS) xattrWritable(ctx context.Context, ino uint64) error {
	m, _, err := f.MountForIno(ctx, ino)
	if err != nil {
		return err
	}
	if m.Mode == config.ModeReadonly {
		return ErrReadOnly
	}
	return nil
}

// xattrMissing reports the "no such attribute" case for callers that map it
// to an errno.
func xattrMissing(err error) bool { return err == meta.ErrNoXattr }
