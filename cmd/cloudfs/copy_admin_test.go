package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	"cloudfs/internal/journal"
	"gopkg.in/yaml.v3"
)

func TestCopyAdminCLIOnlineAndOfflineIsolation(t *testing.T) {
	for _, online := range []bool{false, true} {
		t.Run(map[bool]string{false: "offline", true: "online"}[online], func(t *testing.T) {
			cfg, p := uploadCLIConfig(t)
			cfg.Remotes["ali"] = config.Remote{Type: "fake"}
			cfg.Mounts[0].Layout["/"] = config.Layout{Remote: "ali", Root: "root", Mode: config.ModeWriteback}
			ctx := context.Background()
			d, err := daemon.Open(ctx, daemon.Options{Config: cfg, RequireOwner: true, NoBackground: true})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			identity, err := d.Meta.Identity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			parent, err := d.Meta.Resolve(ctx, "/")
			if err != nil {
				t.Fatal(err)
			}
			accountBinding, err := config.EffectiveAccountBinding(d.Config.Remotes["ali"])
			if err != nil {
				t.Fatal(err)
			}
			c, err := d.Journal.BeginCopy(ctx, journal.CopySpec{MetaIdentity: identity, TargetParentIno: parent.Ino,
				SourceMount: "/", SourceRootID: "root", SourceAccountBinding: accountBinding,
				TargetMount: "/", TargetRootID: "root", TargetAccountBinding: accountBinding,
				SourcePath: "/source", SourceRemote: "ali", SourceID: "source",
				TargetPath: "/dest", TargetRemote: "ali", TargetParentID: "root", Mode: "writeback"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Checkpoint(ctx); err != nil {
				t.Fatal(err)
			}
			id := c.Job().ID
			c.Close()
			if online {
				srv, err := control.NewServer(d.Collector()).Start(ctx, cfg.Control.Socket, "")
				if err != nil {
					t.Fatal(err)
				}
				defer srv.Close()
			} else {
				// A second process must not alter the live owner's preparation.
				if err := runCopies(ctx, []string{"cancel", id, "--config", p}, &bytes.Buffer{}); err == nil {
					t.Fatal("offline cancellation took live ownership")
				}
				d.Close()
			}
			var out bytes.Buffer
			// On-disk provider is still unusable. Offline cancellation does not
			// authenticate it; online commands use the existing daemon.
			if err := runCopies(ctx, []string{"cancel", id, "--config", p, "--json"}, &out); err != nil {
				t.Fatal(err)
			}
			var result control.CopiesResponse
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result.Copies) != 1 || result.Copies[0].State != journal.CopyCancelled {
				t.Fatalf("cancel: %s %v", out.String(), err)
			}
			if !online {
				b, err := yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			out.Reset()
			if err := runCopies(ctx, []string{"retry", id, "--config", p, "--json"}, &out); err != nil {
				t.Fatal(err)
			}
			ro, err := journal.OpenReadOnly(filepath.Join(cfg.Cache.Dir, "journal"))
			if err != nil {
				t.Fatal(err)
			}
			defer ro.Close()
			job, err := ro.GetCopy(ctx, id)
			if err != nil || job.State != journal.CopyReady {
				t.Fatalf("retry ran background work: %+v %v", job, err)
			}
			rows, _, err := ro.ListActive(ctx, "", 10)
			if err != nil || len(rows) != 0 {
				t.Fatalf("retry uploaded synchronously: %+v %v", rows, err)
			}
			if err := runCopies(ctx, []string{"forget", id, "--confirm", "--config", p}, &bytes.Buffer{}); err == nil {
				t.Fatal("forgot active preparation")
			}
			if err := runCopies(ctx, []string{"cancel", id, "--config", p}, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if err := runCopies(ctx, []string{"forget", id, "--config", p}, &bytes.Buffer{}); err == nil {
				t.Fatal("forgot without explicit confirmation")
			}
			job, err = ro.GetCopy(ctx, id)
			if err != nil || job.State != journal.CopyCancelled {
				t.Fatalf("unconfirmed cleanup changed state: %+v %v", job, err)
			}
			out.Reset()
			if err := runCopies(ctx, []string{"forget", id, "--confirm", "--config", p, "--json"}, &out); err != nil {
				t.Fatal(err)
			}
			result = control.CopiesResponse{}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Forgotten != id || len(result.Copies) != 0 {
				t.Fatalf("forget: %s %v", out.String(), err)
			}
			if _, err := ro.GetCopy(ctx, id); !errors.Is(err, journal.ErrNotFound) {
				t.Fatalf("cleanup retained history: %v", err)
			}
			if _, err := os.Stat(filepath.Join(cfg.Cache.Dir, "journal", "copies", id+".part")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cleanup retained private payload: %v", err)
			}
		})
	}
}
