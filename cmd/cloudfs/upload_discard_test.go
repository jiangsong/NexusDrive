package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/daemon"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"gopkg.in/yaml.v3"
)

func TestUploadDiscardCLIUsesOwnerOrOnlyTargetedLocalStores(t *testing.T) {
	for _, online := range []bool{false, true} {
		for _, purging := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "offline", true: "online"}[online], map[bool]string{false: "cancelled", true: "purging"}[purging]}, "/"), func(t *testing.T) {
				cfg, p := uploadCLIConfig(t)
				cfg.Remotes["ali"] = config.Remote{Type: "fake"}
				cfg.Mounts[0].Layout["/"] = config.Layout{Remote: "ali", Root: "root", Mode: config.ModeWriteback}
				ctx := t.Context()
				d, err := daemon.Open(ctx, daemon.Options{Config: cfg, RequireOwner: true, NoBackground: true})
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				if _, err := d.FS.WriteFile(ctx, "/retained", []byte("retained"), false); err != nil {
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
				if _, err := d.FS.WriteFile(ctx, "/unrelated", []byte("unrelated"), false); err != nil {
					t.Fatal(err)
				}
				others, err := d.Journal.Pending(ctx)
				if err != nil || len(others) != 1 {
					t.Fatalf("unrelated: %+v %v", others, err)
				}
				other := others[0]
				if err := d.Journal.Defer(ctx, other.ID, 0, "unrelated interrupted claim"); err != nil {
					t.Fatal(err)
				}
				if claimed, err := d.Journal.Claim(ctx, other.Remote, 1); err != nil || len(claimed) != 1 {
					t.Fatalf("claim unrelated: %+v %v", claimed, err)
				}
				other, err = d.Journal.Get(ctx, other.ID)
				if err != nil {
					t.Fatal(err)
				}
				if purging {
					identity, err := d.Meta.Identity(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := d.Journal.BeginUploadCleanup(ctx, u.ID, identity); err != nil {
						t.Fatal(err)
					}
				}
				// Keep the on-disk backend unusable, with an inaccessible secret.
				cfg.Remotes["ali"] = config.Remote{Type: "unregistered-test-provider", Extra: map[string]any{"token": "secretfile:/not/a/real/credential"}}
				data, err := yaml.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, data, 0600); err != nil {
					t.Fatal(err)
				}
				args := []string{"drop", u.ID, "--confirm", "--json", "--config", p}
				if online {
					srv, err := control.NewServer(d.Collector()).Start(ctx, cfg.Control.Socket, "")
					if err != nil {
						t.Fatal(err)
					}
					defer srv.Close()
				} else {
					if err := runUploads(ctx, args, &bytes.Buffer{}); err == nil {
						t.Fatal("offline drop stole active ownership")
					}
					if err := d.Close(); err != nil {
						t.Fatal(err)
					}
				}
				var out bytes.Buffer
				if err := runUploads(ctx, []string{"drop", u.ID, "--config", p}, &out); err == nil {
					t.Fatal("missing confirmation accepted")
				}
				if err := runUploads(ctx, args, &out); err != nil {
					t.Fatal(err)
				}
				var result control.UploadResponse
				if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Discarded != u.ID || result.State != "" {
					t.Fatalf("response: %s %v", out.String(), err)
				}
				if strings.Contains(out.String(), u.BlobPath) {
					t.Fatal("private path exposed")
				}
				ro, err := journal.OpenReadOnly(filepath.Join(cfg.StateDir(), "journal"))
				if err != nil {
					t.Fatal(err)
				}
				defer ro.Close()
				if _, err := ro.Get(ctx, u.ID); !errors.Is(err, journal.ErrNotFound) {
					t.Fatal(err)
				}
				if row, err := ro.Get(ctx, other.ID); err != nil || row.State != other.State || row.Attempt != other.Attempt || row.LastError != other.LastError {
					t.Fatalf("unrelated task recovered or uploaded: %+v %v", row, err)
				}
				if _, err := os.Stat(u.BlobPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if err := runUploads(ctx, args, &bytes.Buffer{}); err == nil {
					t.Fatal("repeated discard fabricated a new success")
				}
			})
		}
	}
}

func TestUploadDiscardCLIInvalidRequestsCreateNoStorage(t *testing.T) {
	cfg, p := uploadCLIConfig(t)
	for _, args := range [][]string{{"drop", "id"}, {"drop", "--confirm"}, {"drop", "id", "--confirm", "--all"}, {"drop", "id", "--confirm", "--limit", "1"}, {"drop", "id", "extra", "--confirm"}} {
		if err := runUploads(t.Context(), append(args, "--config", p), &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %v", args)
		}
		assertNoDaemonStorage(t, cfg, "invalid request")
	}
	if err := runUploads(t.Context(), []string{"drop", "missing", "--confirm", "--config", p}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing local databases accepted")
	}
	if _, err := os.Stat(cfg.Cache.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestUploadDiscardCLINeverFallsBackAfterLostResponse(t *testing.T) {
	cfg, p := uploadCLIConfig(t)
	l, err := net.Listen("unix", cfg.Control.Socket)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})}
	go s.Serve(l)
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := runUploads(ctx, []string{"drop", "id", "--confirm", "--config", p}, &bytes.Buffer{}); err == nil {
		t.Fatal("lost response reported success")
	}
	if calls.Load() != 1 {
		t.Fatalf("mutation replayed %d times", calls.Load())
	}
	if _, err := os.Stat(cfg.Cache.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lost response fell back offline: %v", err)
	}
}

func TestUploadDiscardOfflineDoesNotCreateNewMountNodes(t *testing.T) {
	cfg, p := uploadCLIConfig(t)
	cfg.Remotes["ali"] = config.Remote{Type: "fake"}
	cfg.Mounts[0].Layout["/"] = config.Layout{Remote: "ali", Root: "root"}
	d, err := daemon.Open(t.Context(), daemon.Options{Config: cfg, NoBackground: true, RequireOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.FS.WriteFile(t.Context(), "/retained", []byte("retained"), false); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Journal.Pending(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatal(err)
	}
	u := rows[0]
	if _, err := d.Uploader.Cancel(t.Context(), u.ID); err != nil {
		t.Fatal(err)
	}
	d.Close()
	cfg.Mounts[0].Layout = map[string]config.Layout{"/new-mount": {Remote: "ali", Root: "root"}}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := runUploads(t.Context(), []string{"drop", u.ID, "--confirm", "--config", p}, &bytes.Buffer{}); err == nil {
		t.Fatal("changed mount accepted")
	}
	s, err := meta.Open(filepath.Join(cfg.StateDir(), "meta.db"), meta.Options{NoIndexMaintenance: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Resolve(t.Context(), "/new-mount"); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("offline cleanup rewrote mount metadata: %v", err)
	}
	if _, err := s.Get(t.Context(), u.Ino); err != nil {
		t.Fatal(err)
	}
}
