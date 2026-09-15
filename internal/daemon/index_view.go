package daemon

import (
	"context"

	"cloudfs/internal/control"
	"cloudfs/internal/index"
)

// indexControl adapts the indexer to the control plane. The Indexer already
// has every method the routes call; what it lacks is the identity the
// doctor compares with meta, which lives on its store.
type indexControl struct {
	*index.Indexer
}

func (v indexControl) Identity(ctx context.Context) (string, error) {
	return v.Store().Identity(ctx)
}

// indexView keeps a missing indexer a nil interface, the way Export and
// Agent do: /index/status answers {"enabled":false} and the other index
// routes 404 on the strength of that field alone.
func (d *Daemon) indexView() control.IndexControl {
	if d.Index == nil {
		return nil
	}
	return indexControl{d.Index}
}
