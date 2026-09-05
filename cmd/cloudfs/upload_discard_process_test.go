package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"gopkg.in/yaml.v3"
)

// The production executable has no injected cleanup hook. A real competing
// SQLite writer holds metadata publication while the CLI/daemon persist their
// journal intent. Killing both processes then exercises ordinary startup.
func TestUploadDiscardProductionProcessKillAndRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "cloudfs")
	if b, err := exec.CommandContext(ctx, "go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, b)
	}
	var remoteCalls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteCalls.Add(1)
		if r.Method != "PROPFIND" {
			http.Error(w, "unexpected remote mutation", 500)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(207)
		fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:"></d:multistatus>`)
	}))
	defer remote.Close()
	cfg, p := uploadCLIConfig(t)
	cfg.Remotes = map[string]config.Remote{"ali": {Type: "webdav", Extra: map[string]any{"url": remote.URL}}}
	cfg.Mounts[0].Layout = map[string]config.Layout{"/": {Remote: "ali", Root: "/", Mode: config.ModeWriteback}}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	d, err := daemon.Open(ctx, daemon.Options{Config: cfg, NoBackground: true, RequireOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.FS.WriteFile(ctx, "/retained", []byte("production retained content"), false); err != nil {
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
	n, err := d.Meta.Get(ctx, u.Ino)
	if err != nil {
		t.Fatal(err)
	}
	key := cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	before := remoteCalls.Load()
	startOwner := func() func() {
		cmd := exec.CommandContext(ctx, bin, "mcp", "--stdio", "--config", p)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout = io.Discard
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		var once sync.Once
		stop := func() { once.Do(func() { stdin.Close(); cmd.Process.Kill(); <-done }) }
		t.Cleanup(stop)
		for {
			select {
			case <-done:
				t.Fatalf("owner exited: %s", stderr.String())
			default:
			}
			_, online, err := control.CallUploads(ctx, cfg.Control.Socket, "", control.UploadRequest{Action: "list", Limit: 10})
			if err != nil {
				stop()
				t.Fatalf("owner control: %v %s", err, stderr.String())
			}
			if online {
				return stop
			}
			select {
			case <-ctx.Done():
				stop()
				t.Fatal("owner startup timed out")
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	stopOwner := startOwner()
	ro, err := journal.OpenReadOnly(filepath.Join(cfg.Cache.Dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	store, err := meta.Open(filepath.Join(cfg.Cache.Dir, "meta.db"), meta.Options{NoIndexMaintenance: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE copy_cleanup_fence SET value=value WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	drop := exec.CommandContext(ctx, bin, "uploads", "drop", u.ID, "--confirm", "--json", "--config", p)
	var dropOut bytes.Buffer
	drop.Stdout, drop.Stderr = &dropOut, &dropOut
	if err := drop.Start(); err != nil {
		t.Fatal(err)
	}
	dropDone := make(chan error, 1)
	go func() { dropDone <- drop.Wait() }()
	for {
		row, err := ro.Get(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.State == journal.StatePurging {
			break
		}
		select {
		case err := <-dropDone:
			t.Fatalf("drop exited before durable intent: %v %s", err, dropOut.String())
		case <-ctx.Done():
			drop.Process.Kill()
			<-dropDone
			t.Fatal("drop never recorded intent")
		case <-time.After(time.Millisecond):
		}
	}
	if _, err := store.Get(ctx, u.Ino); err != nil {
		t.Fatalf("metadata barrier was not held: %v", err)
	}
	if err := drop.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-dropDone; err == nil {
		t.Fatal("drop command was not killed")
	}
	stopOwner()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if row, err := ro.Get(ctx, u.ID); err != nil || row.State != journal.StatePurging {
		t.Fatalf("kill lost intent: %+v %v", row, err)
	}
	if b, err := os.ReadFile(u.BlobPath); err != nil || string(b) != "production retained content" {
		t.Fatalf("early payload deletion: %q %v", b, err)
	}
	stopRestarted := startOwner()
	if _, err := ro.Get(ctx, u.ID); !errors.Is(err, journal.ErrNotFound) {
		t.Fatalf("startup did not finish cleanup: %v", err)
	}
	if _, err := os.Stat(u.BlobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("payload survived cleanup: %v", err)
	}
	stopRestarted()
	store, err = meta.Open(filepath.Join(cfg.Cache.Dir, "meta.db"), meta.Options{NoIndexMaintenance: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Get(ctx, u.Ino); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("startup republished deleted node: %v", err)
	}
	c, err := cache.New(cache.Options{Dir: filepath.Join(cfg.Cache.Dir, "blocks"), BlockSize: int64(cfg.Cache.BlockSize)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if lease, err := c.OpenWhole(key); err == nil {
		lease.Close()
		t.Fatal("startup republished discarded cache")
	}
	if remoteCalls.Load() != before {
		t.Fatalf("cleanup touched remote: before=%d after=%d", before, remoteCalls.Load())
	}
}
