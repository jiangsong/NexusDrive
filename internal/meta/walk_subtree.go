package meta

import (
	"context"
	"errors"
	"path"
)

// SkipDir, returned from a WalkSubtree visit of a directory, keeps the walk
// from descending into it. Returning it for a file changes nothing.
var SkipDir = errors.New("meta: skip this directory")

// walkPageSize bounds how many children one ChildrenPage call materializes.
// A directory of any size is therefore walked in constant memory.
const walkPageSize = 500

// WalkSubtree visits every descendant of ino depth-first in name order, reading
// children 500 at a time so a large tree never sits in memory. base is the
// virtual path of ino; visit receives each node with its virtual path. Returning
// SkipDir from a directory's visit skips its children. Any other error stops
// the walk and is returned as is.
//
// Pages are independent snapshots: a child renamed to sort before the cursor
// while the walk is in progress is missed, and one renamed to sort after it is
// seen twice. Callers that need a fence reconcile against the store afterwards
// instead of asking the walk for isolation it cannot give without holding the
// whole listing.
func (s *Store) WalkSubtree(ctx context.Context, ino uint64, base string, visit func(n Node, p string) error) error {
	after := ""
	for {
		page, err := s.ChildrenPage(ctx, ino, after, 0, walkPageSize)
		if err != nil {
			return err
		}
		for _, n := range page {
			p := path.Join(base, n.Name)
			err := visit(n, p)
			switch {
			case err == nil:
				if n.IsDir() {
					if err := s.WalkSubtree(ctx, n.Ino, p, visit); err != nil {
						return err
					}
				}
			case errors.Is(err, SkipDir):
				// Skipping a directory omits its children; on a file there is
				// nothing to omit.
			default:
				return err
			}
		}
		if len(page) < walkPageSize {
			return nil
		}
		after = page[len(page)-1].Name
	}
}
