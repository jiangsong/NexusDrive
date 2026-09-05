package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
)

// DiscardUploadOffline deliberately does not Open a daemon: even disabled
// workers would still construct credential stores, providers and proxies.
// It touches only existing local storage and the explicitly selected upload.
func DiscardUploadOffline(ctx context.Context, cfg *config.Config, id string, confirm bool) error {
	if !confirm || cfg == nil || id == "" || len(cfg.Mounts) == 0 {
		return errors.New("daemon: offline discard requires configuration, an upload id and confirmation")
	}
	dir := cfg.Cache.Dir
	if dir == "" {
		dir = config.ExpandHome("~/.cache/cloudfs")
	}
	for _, p := range []string{filepath.Join(dir, "journal", "journal.db"), filepath.Join(dir, "meta.db")} {
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("daemon: existing upload and metadata databases are required")
		}
	}
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal"), Durability: journal.Durability(cfg.Journal.Durability)})
	if err != nil {
		return err
	}
	defer j.Close()
	if !j.Owner() {
		return errors.New("daemon: storage is owned by another process; use its control endpoint")
	}
	u, err := j.Get(ctx, id)
	if err != nil {
		return err
	}
	if u.State != journal.StateCancelled && u.State != journal.StatePurging || u.Tombstone || u.NeedsPublish || u.Ino == 0 {
		return journal.ErrUploadCleanupState
	}
	// Membership is checked, but no provider factory, auth or proxy is invoked.
	providers := make(map[string]provider.Provider, len(cfg.Remotes))
	for remote := range cfg.Remotes {
		providers[remote] = nil
	}
	bindings, err := remoteAccountBindings(cfg)
	if err != nil {
		return err
	}
	mounts, err := buildMounts(cfg.Mounts[0], providers, bindings)
	if err != nil {
		return err
	}
	s, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{NoIndexMaintenance: true})
	if err != nil {
		return err
	}
	defer s.Close()
	c, err := cache.New(cache.Options{Dir: filepath.Join(dir, "blocks"), BlockSize: int64(cfg.Cache.BlockSize), SubBlockSize: int64(cfg.Cache.SubBlockSize)})
	if err != nil {
		return err
	}
	defer c.Close()
	return vfs.DiscardUploadOffline(ctx, vfs.Options{Meta: s, Cache: c, Mounts: mounts}, j, id, true)
}
