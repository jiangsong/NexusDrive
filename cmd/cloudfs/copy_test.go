package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	"cloudfs/internal/journal"
	"gopkg.in/yaml.v3"
)

func TestCopyCLIOnlineUsesExistingOwner(t *testing.T) {
	cfg, p := uploadCLIConfig(t)
	// Leave the on-disk provider unusable: an online call must not build it.
	cfg.Remotes["ali"] = config.Remote{Type: "fake"}
	ctx := context.Background()
	d, err := daemon.Open(ctx, daemon.Options{Config: cfg, RequireOwner: true, NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.FS.WriteFile(ctx, "/source", []byte("online"), false); err != nil {
		t.Fatal(err)
	}
	srv, err := control.NewServer(&control.Collector{FS: d.FS}).Start(ctx, cfg.Control.Socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	var out bytes.Buffer
	if err := runCopy(ctx, []string{"/source", "/dest", "--json", "--config", p}, &out); err != nil {
		t.Fatal(err)
	}
	var result control.CopyResponse
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.File.Size != 6 {
		t.Fatalf("output=%s err=%v", out.String(), err)
	}
	got, err := d.FS.ReadFileRange(ctx, "/dest", 0, 6)
	if err != nil || string(got) != "online" {
		t.Fatalf("read=%q %v", got, err)
	}
}

func TestCopyCLIOfflinePersistsWithoutStartingUploads(t *testing.T) {
	cfg, p := uploadCLIConfig(t)
	cfg.Remotes["ali"] = config.Remote{Type: "fake"}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	d, err := daemon.Open(ctx, daemon.Options{Config: cfg, RequireOwner: true, NoBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.FS.WriteFile(ctx, "/source", []byte("offline"), false); err != nil {
		d.Close()
		t.Fatal(err)
	}
	d.Close()
	var out bytes.Buffer
	if err := runCopy(ctx, []string{"/source", "/dest", "--config", p}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "journaled locally") {
		t.Fatal(out.String())
	}
	j, err := journal.OpenReadOnly(filepath.Join(cfg.Cache.Dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	rows, _, err := j.ListActive(ctx, "", 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	for _, row := range rows {
		if row.State != journal.StatePending {
			t.Fatalf("offline copy started upload: %+v", row)
		}
		b, err := os.ReadFile(row.BlobPath)
		if err != nil || string(b) != "offline" {
			t.Fatalf("blob=%q %v", b, err)
		}
	}
}

func TestCopyCLIRefusesUnavailableOwner(t *testing.T) {
	cfg, p := uploadCLIConfig(t)
	j, _ := uploadCLIJournal(t, cfg)
	defer j.Close()
	err := runCopy(context.Background(), []string{"/a", "/b", "--config", p}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "owned by another process") {
		t.Fatalf("owner=%v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Cache.Dir, "meta.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("opened metadata before ownership check")
	}
}
