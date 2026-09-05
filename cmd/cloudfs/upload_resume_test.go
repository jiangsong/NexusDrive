package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	"cloudfs/internal/journal"
	"gopkg.in/yaml.v3"
)

func TestUploadResumeCLIUsesOwnerOrQueuesOfflineWithoutUploads(t *testing.T) {
	for _, online := range []bool{false, true} {
		t.Run(map[bool]string{false: "offline", true: "online"}[online], func(t *testing.T) {
			cfg, path := uploadCLIConfig(t)
			cfg.Remotes["ali"] = config.Remote{Type: "fake"}
			cfg.Mounts[0].Layout["/"] = config.Layout{Remote: "ali", Root: "root", Mode: config.ModeWriteback}
			ctx := context.Background()
			d, err := daemon.Open(ctx, daemon.Options{Config: cfg, RequireOwner: true, NoBackground: true})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if _, err := d.FS.WriteFile(ctx, "/resume.txt", []byte("retained"), false); err != nil {
				t.Fatal(err)
			}
			rows, err := d.Journal.Pending(ctx)
			if err != nil || len(rows) != 1 {
				t.Fatalf("queue: %+v %v", rows, err)
			}
			id := rows[0].ID
			if _, err := d.Uploader.Cancel(ctx, id); err != nil {
				t.Fatal(err)
			}
			if online {
				s, err := control.NewServer(d.Collector()).Start(ctx, cfg.Control.Socket, "")
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				// On-disk provider remains invalid: online resume must use owner.
			} else {
				d.Close()
				data, err := yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			if err := runUploads(ctx, []string{"resume", id, "--config", path}, &out); err == nil {
				t.Fatal("missing confirmation accepted")
			}
			if err := runUploads(ctx, []string{"resume", id, "--confirm", "--json", "--config", path}, &out); err != nil {
				t.Fatal(err)
			}
			var result control.UploadResponse
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.State != journal.StatePending {
				t.Fatalf("response: %s %v", out.String(), err)
			}
			ro, err := journal.OpenReadOnly(filepath.Join(cfg.Cache.Dir, "journal"))
			if err != nil {
				t.Fatal(err)
			}
			defer ro.Close()
			u, err := ro.Get(ctx, id)
			if err != nil || u.State != journal.StatePending || u.Attempt != 0 || len(u.Session) != 0 {
				t.Fatalf("unexpected transfer: %+v %v", u, err)
			}
		})
	}
}
