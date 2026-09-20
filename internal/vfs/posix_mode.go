package vfs

import "context"

// Chmod records the permission bits chmod(2) set on a node.
//
// No backend has a POSIX permission model, so the mode is held here and never
// sent anywhere — which makes this filesystem the only place the bit exists.
// Every directory refresh therefore carries the default mode rather than the
// stored one, and the meta store is what keeps that default from landing on a
// node whose mode somebody chose. Without it an executable script loses its
// exec bit the moment its directory goes stale, and git reports a mode-only
// diff over a checkout nothing touched.
func (f *FS) Chmod(ctx context.Context, ino uint64, mode uint32) error {
	if err := f.localAttrWritable(ctx, ino); err != nil {
		return err
	}
	return f.meta.SetMode(ctx, ino, mode)
}
