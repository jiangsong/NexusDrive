package daemon

import (
	"errors"
	"os"
	"testing"

	"cloudfs/internal/cache"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
)

func TestStartupCompletesUploadCleanupBeforePublicationAndWorkers(t *testing.T) {
	for _, phase := range []string{"intent", "metadata", "cache"} {
		t.Run(phase, func(t *testing.T) {
			cfg, _ := writeConfig(t, baseConfig)
			ctx := t.Context()
			d, err := Open(ctx, Options{Config: cfg, NoBackground: true})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if _, err := d.FS.WriteFile(ctx, "/demo/retained", []byte("retained"), false); err != nil {
				t.Fatal(err)
			}
			rows, err := d.Journal.Pending(ctx)
			if err != nil || len(rows) != 1 {
				t.Fatalf("queue: %+v %v", rows, err)
			}
			u := rows[0]
			if _, err := d.Uploader.Cancel(ctx, u.ID); err != nil {
				t.Fatal(err)
			}
			identity, err := d.Meta.Identity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Journal.BeginUploadCleanup(ctx, u.ID, identity); err != nil {
				t.Fatal(err)
			}
			n, err := d.Meta.Get(ctx, u.Ino)
			if err != nil {
				t.Fatal(err)
			}
			key := cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
			if phase != "intent" {
				if err := d.Meta.RemoveLocalVersion(ctx, meta.LocalVersionCleanup{StoreIdentity: identity, Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version, Size: n.Size, Nodes: []meta.Node{n}}); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "cache" {
				if err := d.Cache.ForgetChecked(key); err != nil {
					t.Fatal(err)
				}
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			// Background workers may start only after all three stores agree.
			d, err = Open(ctx, Options{Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if _, err := d.Meta.Get(ctx, u.Ino); !errors.Is(err, meta.ErrNotFound) {
				t.Fatalf("startup republished node: %v", err)
			}
			if _, err := d.Journal.Get(ctx, u.ID); !errors.Is(err, journal.ErrNotFound) {
				t.Fatalf("startup retained queue: %v", err)
			}
			if _, err := os.Stat(u.BlobPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("startup retained payload: %v", err)
			}
			if err := d.Journal.Commit(ctx, u); !errors.Is(err, journal.ErrUploadPurging) {
				t.Fatalf("startup lost permanent fence: %v", err)
			}
			if stats := d.CallStats["demo"]; stats == nil || stats.Total() != 0 {
				t.Fatalf("cleanup caused provider requests: %+v", stats)
			}
		})
	}
}
