package config

import (
	"os"
	"path/filepath"
)

// DefaultRoot is the single directory a default installation keeps everything
// in: the configuration, the databases, the journal, the credential fallback
// files, the control socket and — unless it is pointed elsewhere — the block
// cache.
//
// One root is a product decision. The alternative in use before this was the
// XDG split, with the configuration under ~/.config and everything else under
// ~/.cache, and it put the journal in a directory the operating system, backup
// tools and "free up disk space" utilities all treat as disposable. The
// journal holds bytes that have been written and not yet uploaded, so treating
// it as disposable loses a person's files.
const DefaultRoot = "~/.cloudfs"

// DefaultConfigPath is where the configuration lives when nothing says
// otherwise. CLOUDFS_CONFIG wins, and is expanded: it reaches the process
// unexpanded from a launchd plist or a systemd unit, and a literal "~" would
// name a directory nothing else in the package would ever find.
func DefaultConfigPath() string {
	if p := os.Getenv("CLOUDFS_CONFIG"); p != "" {
		return ExpandHome(p)
	}
	return ExpandHome(filepath.Join(DefaultRoot, "config.yaml"))
}

// StateDir is the directory holding everything that cannot be reproduced from
// the remote side: meta.db, the journal, the agent database, the pool indexes
// and the credential fallback files.
//
// It follows the configuration file and is deliberately not configurable.
// cache.dir is configurable, because a block cache is large and reproducible
// and has every reason to live on another disk; state has neither property, so
// letting it travel with cache.dir would mean an external disk could be
// unplugged with unuploaded data on it.
func (c *Config) StateDir() string {
	if c != nil && c.SourcePath != "" {
		return filepath.Dir(c.SourcePath)
	}
	return ExpandHome(DefaultRoot)
}

// BlockCacheDir is where the block cache lives. Only blocks go here.
func (c *Config) BlockCacheDir() string {
	if c != nil && c.Cache.Dir != "" {
		return ExpandHome(c.Cache.Dir)
	}
	return filepath.Join(c.StateDir(), "cache")
}
