package fusefs

import (
	"context"
	"sync"

	"github.com/hanwen/go-fuse/v2/fs"
)

// kernelNodes maps a VFS inode to the kernel nodes built for it.
//
// go-fuse numbers kernel nodes itself, in lookup order, and the number is
// what the kernel identifies an inode by in a notification; StableAttr.Ino
// is only the inode number shown to stat(2). The two agree while every
// inode is looked up in the order the VFS created it and diverge as soon
// as one is created behind the kernel's back (an MCP write, a control API
// call, a listing the VFS made on its own). A notification sent with the
// VFS inode then reaches a different inode or none, and the kernel keeps
// serving a directory stream or a dentry the VFS has already changed. So
// the callbacks go through the *fs.Inode go-fuse holds for the inode,
// whose NotifyContent and NotifyEntry carry the kernel's own number.
//
// Which node that is cannot be known where it is built: go-fuse resolves a
// lookup of an inode it already holds to the existing node after Lookup
// returns, and the one just built is dropped without ever meeting the
// kernel. Every candidate is tracked and told apart at notification time:
// a node the kernel holds has a lookup or a parent, a dropped one reports
// Forgotten() from the start and is pruned. A node the kernel forgets
// removes itself.
type kernelNodes struct {
	mu    sync.Mutex
	byIno map[uint64][]*node
}

// track records n as a candidate for its inode. Dropped candidates
// recorded earlier are pruned here too, except the newest, which may be a
// concurrent lookup go-fuse has not resolved yet, so the list stays at the
// live node plus at most two in flight.
func (k *kernelNodes) track(n *node) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.byIno == nil {
		k.byIno = map[uint64][]*node{}
	}
	old := k.byIno[n.ino]
	kept := old[:0:0]
	for i, c := range old {
		if i == len(old)-1 || !c.Forgotten() {
			kept = append(kept, c)
		}
	}
	k.byIno[n.ino] = append(kept, n)
}

// forget removes n, which the kernel no longer holds.
func (k *kernelNodes) forget(n *node) {
	k.mu.Lock()
	defer k.mu.Unlock()
	old := k.byIno[n.ino]
	kept := old[:0:0]
	for _, c := range old {
		if c != n {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		delete(k.byIno, n.ino)
	} else {
		k.byIno[n.ino] = kept
	}
}

// live returns the nodes the kernel holds for ino, pruning the ones
// go-fuse dropped.
func (k *kernelNodes) live(ino uint64) []*node {
	k.mu.Lock()
	defer k.mu.Unlock()
	old := k.byIno[ino]
	kept := old[:0:0]
	for _, c := range old {
		if !c.Forgotten() {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		delete(k.byIno, ino)
	} else {
		k.byIno[ino] = kept
	}
	return append([]*node(nil), kept...)
}

// newInode builds the kernel node for a VFS inode under parent and tracks
// it; every path that hands the kernel a new inode goes through here.
func (n *node) newInode(ctx context.Context, ino uint64, isDir bool) *fs.Inode {
	child, stable := n.root.newNode(ino, isDir)
	inode := n.NewInode(ctx, child, stable)
	n.root.kernel.track(child)
	return inode
}

// OnForget drops the node from the registry once the kernel has forgotten
// it, so a later lookup's node takes its place.
func (n *node) OnForget() { n.root.kernel.forget(n) }

var _ fs.NodeOnForgetter = (*node)(nil)

// notifyContent tells the kernel to drop what it caches for ino: the
// attributes, the page cache of a file, the directory stream of a
// directory. It is a no-op for an inode the kernel does not hold.
func (r *Root) notifyContent(ino uint64) {
	for _, n := range r.kernel.live(ino) {
		_ = n.NotifyContent(0, -1)
	}
}

// notifyEntry drops the kernel's dentry for name under parent.
func (r *Root) notifyEntry(parent uint64, name string) {
	for _, n := range r.kernel.live(parent) {
		_ = n.NotifyEntry(name)
	}
}
