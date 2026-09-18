package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/config"
)

// State goes beside the configuration; only blocks follow cache.dir.
//
// cache.dir is a setting the console invites a person to point at another
// disk, because a block cache is large and can always be fetched again. The
// journal cannot: it holds bytes that have been written and not yet uploaded.
// Opening the daemon with cache.dir on a different root has to leave meta.db,
// the journal, the agent store and the pool indexes where the configuration
// is, or pulling that disk out takes unuploaded files with it.
func TestOpenKeepsStateBesideTheConfigWhenTheCacheLivesElsewhere(t *testing.T) {
	stateDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "external-disk")
	path := filepath.Join(stateDir, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(baseConfig, cacheDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test", RequireOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	for _, name := range []string{"journal", "meta.db", "agent"} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); err != nil {
			t.Errorf("%s is not in the state directory: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(cacheDir, name)); err == nil {
			t.Errorf("%s was created under cache.dir; unplugging that disk would take it along", name)
		}
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "blocks")); err != nil {
		t.Errorf("blocks are not under cache.dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "blocks")); err == nil {
		t.Error("blocks were created in the state directory; cache.dir was ignored")
	}
}

// A configuration that names no cache directory keeps everything, blocks
// included, under the one root the product documents.
func TestOpenWithNoCacheDirKeepsEverythingUnderOneRoot(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, "config.yaml")
	body := "remotes:\n  demo: { type: fake }\nmounts:\n  - path: /mnt/cloud\n    layout:\n      /demo: { remote: demo, root: root, mode: writeback }\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test", RequireOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, name := range []string{"journal", "meta.db", "agent", filepath.Join("cache", "blocks")} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); err != nil {
			t.Errorf("%s is not under the state root: %v", name, err)
		}
	}
}
