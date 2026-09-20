package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// SetCacheBudget rewrites cache.max_size and cache.min_free in the file at
// configPath, leaving every other node as it was. max_size 0 means no cap;
// min_free 0 means no disk headroom is kept — the console explains both
// before it offers them. Sizes are written in the human form the file
// otherwise uses ("5.0GiB"), which ParseSize reads back exactly for
// whole-MiB values and to within a mebibyte otherwise.
func SetCacheBudget(configPath string, maxSize, minFree Size) error {
	if maxSize < 0 || minFree < 0 {
		return fmt.Errorf("config: cache budget must not be negative")
	}
	return editConfig(configPath, false, func(root *yaml.Node, c *Config) error {
		candidate := *c
		candidate.Cache.MaxSize = maxSize
		candidate.Cache.MinFree = minFree
		if err := candidate.Validate(); err != nil {
			return err
		}
		cache, err := ensureMapping(root, "cache")
		if err != nil {
			return err
		}
		setNode(cache, "max_size", scalar(maxSize.String()))
		setNode(cache, "min_free", scalar(minFree.String()))
		return nil
	})
}
