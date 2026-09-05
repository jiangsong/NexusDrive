package vfs

import (
	"context"
	"errors"

	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

// collectDirectory feeds the same TEMP collector from either a streaming
// backend or its paginated compatibility interface. Never replay after even
// one entry was delivered, and never swallow a local collector failure.
func collectDirectory(ctx context.Context, p provider.Provider, id string, listing *meta.DirListing, visit func(provider.Entry) error) error {
	if stream, ok := p.(provider.StreamLister); ok && p.Capabilities().StreamList {
		var delivered bool
		var visitErr error
		err := stream.ListStream(ctx, id, func(e provider.Entry) error {
			delivered = true
			if visitErr == nil {
				visitErr = visit(e)
			}
			return visitErr
		})
		if visitErr != nil {
			return visitErr
		}
		if err == nil {
			return ctx.Err()
		}
		if delivered || !errors.Is(err, provider.ErrUnsupported) {
			return mapProviderErr(err)
		}
	}
	cursor := ""
	for {
		if err := listing.RecordCursor(ctx, cursor); err != nil {
			return err
		}
		entries, next, err := p.List(ctx, id, cursor)
		if err != nil {
			return mapProviderErr(err)
		}
		for _, e := range entries {
			if err := visit(e); err != nil {
				return err
			}
		}
		if next == "" {
			return ctx.Err()
		}
		cursor = next
	}
}
