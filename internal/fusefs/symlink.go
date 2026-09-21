//go:build !windows

package fusefs

import (
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

var _ fs.NodeSymlinker = (*node)(nil)
var _ fs.NodeReadlinker = (*node)(nil)

func (n *node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.root.count(opSymlink)
	ctx, cancel := durable(ctx)
	defer cancel()
	defer n.root.forgetEntry(n.vfsIno(), name)
	at, err := n.root.opt.FS.Symlink(ctx, n.vfsIno(), name, target)
	if err != nil {
		return nil, errno(err)
	}
	return n.replyEntry(ctx, at, out), 0
}

func (n *node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	n.root.count(opReadlink)
	target, err := n.root.opt.FS.Readlink(ctx, n.vfsIno())
	return target, errno(err)
}
