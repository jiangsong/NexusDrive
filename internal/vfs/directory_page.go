package vfs

import (
	"context"
	"errors"
	"path"
	"sync"

	"cloudfs/internal/meta"
)

const MaxDirectoryPageSize = 1000

// Directory refresh waiters share a lazy full-list result. Page/lookup-only
// waiters never materialize it, while concurrent full ReadDir callers retain
// the old singleflight sharing instead of each reloading all children.
type directoryRefresh struct {
	once  sync.Once
	nodes []meta.Node
	err   error
}

func (r *directoryRefresh) children(ctx context.Context, store *meta.Store, ino uint64) ([]meta.Node, error) {
	r.once.Do(func() { r.nodes, r.err = store.Children(ctx, ino) })
	return r.nodes, r.err
}

type DirectoryPageOptions struct {
	// After is the last returned name, exclusively. Offset supports old
	// numeric cursors only; it cannot be combined with After.
	After  string
	Offset int
	Limit  int
	// Count requests an index count for clients retaining a total field.
	// Total and Entries need not describe the same concurrent tree version.
	Count bool
}

type DirectoryPage struct {
	Entries []Attr
	HasMore bool
	Total   int
}

// ReadDirPagePath keeps cached directory reads bounded and applies the same
// TTL, stale-on-error, mount and local-write protection policy as ReadDirPath.
// A cold/expired refresh spools provider pages before one atomic merge, but
// providers may return a large single page. Pagination is not a cross-request
// snapshot or a hard process-memory/disk budget.
func (f *FS) ReadDirPagePath(ctx context.Context, p string, opt DirectoryPageOptions) (DirectoryPage, error) {
	var out DirectoryPage
	if opt.Limit < 1 || opt.Limit > MaxDirectoryPageSize || opt.Offset < 0 || (opt.After != "" && opt.Offset != 0) {
		return out, errors.New("vfs: invalid directory page")
	}
	n, err := f.resolve(ctx, p)
	if err != nil {
		return out, err
	}
	if !n.IsDir() {
		return out, ErrNotDir
	}
	if err := f.freshenDir(ctx, n.Ino); err != nil {
		return out, err
	}
	nodes, err := f.meta.ChildrenPage(ctx, n.Ino, opt.After, opt.Offset, opt.Limit+1)
	if err != nil {
		return out, err
	}
	if len(nodes) > opt.Limit {
		out.HasMore = true
		nodes = nodes[:opt.Limit]
	}
	dir := path.Clean("/" + p)
	out.Entries = make([]Attr, 0, len(nodes))
	for _, node := range nodes {
		out.Entries = append(out.Entries, f.attrAt(ctx, node, path.Join(dir, node.Name)))
	}
	if opt.Count {
		out.Total, err = f.meta.ChildrenCount(ctx, n.Ino)
	}
	return out, err
}
