package daemon

import (
	"errors"
	"fmt"
	"sort"

	"cloudfs/internal/config"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
)

// buildMounts assembles one vfs.Mount per layout prefix. cachePolicy is the
// config's global cache.policy default and blockSize its cache.block_size;
// both feed config.ResolveCachePolicy so each mount carries its effective,
// fully-defaulted CachePolicy (presets and layout.<prefix>.cache overrides
// already applied — config.Validate already checked this resolves without
// error, so a failure here would mean cfg was never validated).
func buildMounts(m config.Mount, providers map[string]provider.Provider, bindings map[string]string, cachePolicy config.CachePolicy, blockSize int64) ([]vfs.Mount, error) {
	var out []vfs.Mount
	prefixes := make([]string, 0, len(m.Layout))
	for prefix := range m.Layout {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	for _, prefix := range prefixes {
		l := m.Layout[prefix]
		p, ok := providers[l.Remote]
		if !ok {
			return nil, fmt.Errorf("daemon: mount %s references unknown remote %q", prefix, l.Remote)
		}
		root := l.Root
		if root == "" {
			// An unset root means "the remote's own root". Discover it the same
			// way CheckAccount does, so a config that checks out also mounts;
			// path-based backends fall back to "/".
			root = providerRoot(p)
		}
		resolved, err := config.ResolveCachePolicy(cachePolicy, l.Cache, blockSize)
		if err != nil {
			return nil, fmt.Errorf("daemon: mount %s: %w", prefix, err)
		}
		out = append(out, vfs.Mount{
			Prefix: prefix, Remote: l.Remote, RootID: root, AccountBinding: bindings[l.Remote], Provider: p,
			Mode: l.Mode, DirTTL: l.DirTTL, Pin: l.Pin,
			Policy: vfs.CachePolicy{
				SmallFileWhole:     resolved.SmallFileWhole,
				SmallFileThreshold: int64(resolved.SmallFileThreshold),
				DirReadahead:       resolved.DirReadahead,
				ReadaheadMax:       int64(resolved.ReadaheadMax),
				ReadaheadRequest:   int64(resolved.ReadaheadRequest),
				ReadaheadLead:      resolved.ReadaheadLead,
			},
		})
	}
	if len(out) == 0 {
		return nil, errors.New("daemon: the mount has an empty layout")
	}
	return out, nil
}
