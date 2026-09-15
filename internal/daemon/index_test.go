package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/control"
	"cloudfs/internal/i18n"
	"cloudfs/internal/index"
)

func TestIndexDisabledCreatesNoIndexDB(t *testing.T) {
	cfg, cacheDir := writeConfig(t, baseConfig)
	if cfg.Index.Enabled {
		t.Fatal("index.enabled must default to false")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Index != nil {
		d.Close()
		t.Fatal("an indexer was built with index.enabled false")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "index.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("index.db after a disabled run: %v", err)
	}
}

func TestIndexEnabledRunsTheIndexerOnlyInTheOwner(t *testing.T) {
	cfg, cacheDir := writeConfig(t, baseConfig+"index:\n  enabled: true\n")
	if !cfg.Index.Enabled {
		t.Fatal("index.enabled was not read")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner, err := Open(ctx, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if owner.Index == nil || !owner.Index.Store().Owner() {
		t.Fatal("the first process must own the index and run the worker")
	}
	if !owner.Index.Progress().Running {
		t.Fatal("the owner's worker was not started")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "index.db")); err != nil {
		t.Fatalf("index.db: %v", err)
	}
	other, err := Open(ctx, Options{Config: cfg, Version: "status"})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.Index == nil {
		t.Fatal("the second process must still be able to search")
	}
	if other.Index.Store().Owner() || other.Index.Progress().Running {
		t.Fatal("the second process must not run a worker")
	}
	if _, err := other.Index.Search(ctx, index.SearchQuery{Query: "anything"}); err != nil {
		t.Fatalf("search through the non-owner: %v", err)
	}
	st, err := other.Index.Status(ctx, "")
	if err != nil || st.Pending != 0 || st.Docs.OK != 0 {
		t.Fatalf("the non-owner queued or extracted something: %+v %v", st, err)
	}
}

// TestIndexReachesTheControlPlaneAndTheDoctor: with an index the collector
// serves /index/*, /status carries the index line, and the doctor reports
// on index.db and its binding to this metadata store. Without one, every
// field stays nil so the routes answer "disabled".
func TestIndexReachesTheControlPlaneAndTheDoctor(t *testing.T) {
	cfg, _ := writeConfig(t, baseConfig+"index:\n  enabled: true\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := Open(ctx, Options{Config: cfg, Version: "test", NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	col := d.Collector()
	if col.Index == nil {
		t.Fatal("the collector has no index view")
	}
	st := col.Collect(ctx, i18n.EN)
	if st.Index == nil || !st.Index.Enabled {
		t.Fatalf("status lacks the index line: %+v", st.Index)
	}
	if id, err := col.Index.Identity(ctx); err != nil || id == "" {
		t.Fatalf("identity: %q %v", id, err)
	}
	names := map[string]control.Level{}
	for _, c := range col.Doctor.Run(ctx) {
		names[c.Name] = c.Level
	}
	if names["index_db"] != control.LevelOK || names["index_identity"] != control.LevelOK {
		t.Fatalf("doctor index checks: %v", names)
	}

	off, _ := writeConfig(t, baseConfig)
	plain, err := Open(ctx, Options{Config: off, Version: "test", NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	pc := plain.Collector()
	if pc.Index != nil || pc.Doctor.Index != nil || pc.Collect(ctx, i18n.EN).Index != nil {
		t.Fatal("a daemon without an index still wired one into the control plane")
	}
}
