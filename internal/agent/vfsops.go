package agent

import (
	"context"
	"path"

	"cloudfs/internal/cache"
	"cloudfs/internal/vfs"
)

// vfsOps adapts a *vfs.FS to FSOps: the by-path calls the VFS already has
// pass through, and the ones it only offers by parent inode (Mkdir,
// Remove, Rename) resolve the parent first, the way the MCP tools do.
type vfsOps struct{ fs *vfs.FS }

// VFSOps returns the FSOps view of a VFS. The MCP server and the control
// plane both run rollbacks through it.
func VFSOps(fs *vfs.FS) FSOps {
	if fs == nil {
		return nil
	}
	return vfsOps{fs: fs}
}

func (v vfsOps) StatPath(ctx context.Context, p string) (FileInfo, error) {
	n, err := v.fs.Meta().Resolve(ctx, p)
	if err == nil {
		return FileInfo{IsDir: n.IsDir(), Size: n.Size, Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}, nil
	}
	// Not in meta yet: let the VFS list the parents, then read the node
	// back for its remote id, which Attr does not carry.
	a, err := v.fs.StatPath(ctx, p)
	if err != nil {
		return FileInfo{}, err
	}
	info := FileInfo{IsDir: a.IsDir, Size: a.Size, Remote: a.Remote, Version: a.Version}
	if n, err := v.fs.Meta().Resolve(ctx, p); err == nil {
		info.RemoteID = n.RemoteID
	}
	return info, nil
}

func (v vfsOps) ReadFileRange(ctx context.Context, p string, off, length int64) ([]byte, error) {
	return v.fs.ReadFileRange(ctx, p, off, length)
}

func (v vfsOps) WriteFile(ctx context.Context, p string, data []byte, appendMode bool) (FileInfo, error) {
	a, err := v.fs.WriteFile(ctx, p, data, appendMode)
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Size: a.Size, Remote: a.Remote, Version: a.Version}, nil
}

func (v vfsOps) Mkdir(ctx context.Context, p string) error {
	parent, err := v.fs.StatPath(ctx, path.Dir(p))
	if err != nil {
		return err
	}
	_, err = v.fs.Mkdir(ctx, parent.Ino, path.Base(p))
	return err
}

func (v vfsOps) Remove(ctx context.Context, p string, recursive bool) error {
	parent, err := v.fs.StatPath(ctx, path.Dir(p))
	if err != nil {
		return err
	}
	return v.fs.Remove(ctx, parent.Ino, path.Base(p), recursive)
}

func (v vfsOps) Rename(ctx context.Context, from, to string) error {
	src, err := v.fs.StatPath(ctx, path.Dir(from))
	if err != nil {
		return err
	}
	dst, err := v.fs.StatPath(ctx, path.Dir(to))
	if err != nil {
		return err
	}
	return v.fs.Rename(ctx, src.Ino, path.Base(from), dst.Ino, path.Base(to))
}

// HydratedPath is the block cache's merged file for p's current content,
// keyed the way the read path keys it. A file the upload queue still holds
// (a cloudfs-local: id) is keyed the same way, since its content is the
// journal blob the cache links to.
func (v vfsOps) HydratedPath(ctx context.Context, p string) (string, bool) {
	n, err := v.fs.Meta().Resolve(ctx, p)
	if err != nil || n.IsDir() || n.RemoteID == "" {
		return "", false
	}
	return v.fs.Cache().HydratedPath(cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version})
}
