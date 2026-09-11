package daemon

import (
	"fmt"
	"os"
	"strconv"

	"cloudfs/internal/config"
)

// readAheadBlocks is the sequential prefetch window in blocks; the
// CLOUDFS_READAHEAD_BLOCKS environment variable overrides it for experiments.
func readAheadBlocks() int {
	if v := os.Getenv("CLOUDFS_READAHEAD_BLOCKS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 16
}

// prefetchDepth is how many directory levels below a listed directory are
// listed ahead of time; CLOUDFS_PREFETCH_DEPTH overrides it for experiments.
func prefetchDepth() int {
	if v := os.Getenv("CLOUDFS_PREFETCH_DEPTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 2
}

// readaheadRequest is how many bytes a coalesced readahead range request may
// cover; zero (the default) lets vfs derive it per mount from the provider's
// capabilities. CLOUDFS_READAHEAD_REQUEST overrides it for experiments, as a
// byte count or a size string ("256KiB", "1MiB"). vfs.New rejects a non-zero
// value that is not a positive multiple of the cache's block size, so the
// only error returned here is a malformed environment value.
func readaheadRequest() (int64, error) {
	v := os.Getenv("CLOUDFS_READAHEAD_REQUEST")
	if v == "" {
		return 0, nil
	}
	size, err := config.ParseSize(v)
	if err != nil {
		return 0, fmt.Errorf("daemon: CLOUDFS_READAHEAD_REQUEST: %w", err)
	}
	return int64(size), nil
}
