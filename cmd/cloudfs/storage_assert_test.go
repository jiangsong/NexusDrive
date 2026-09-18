package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"cloudfs/internal/config"
)

// assertNoDaemonStorage fails when a command that is supposed to stay
// read-only, or to reject its arguments, has created local storage anyway.
//
// It checks both roots. Before the state directory was split out of the cache
// directory, one os.Stat on cache.dir covered everything; now the journal and
// the databases live beside the configuration file, in a directory that
// already exists, so an assertion on cache.dir alone would quietly stop
// catching a command that opens the journal.
func assertNoDaemonStorage(t *testing.T, cfg *config.Config, what string) {
	t.Helper()
	paths := []string{cfg.BlockCacheDir()}
	for _, name := range []string{"journal", "meta.db", "agent", "index.db"} {
		paths = append(paths, filepath.Join(cfg.StateDir(), name))
	}
	for _, p := range paths {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s created %s: %v", what, filepath.Base(p), err)
		}
	}
}
