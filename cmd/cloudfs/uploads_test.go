package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
	"cloudfs/internal/journal"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/test/fakeprovider"
	"gopkg.in/yaml.v3"
)

func uploadCLIConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	// A deliberately unusable provider proves list/retry use only the queue.
	cfg := &config.Config{Cache: config.Cache{Dir: filepath.Join(dir, "cache"), BlockSize: 4 << 20, SubBlockSize: 16 << 10},
		Remotes: map[string]config.Remote{"ali": {Type: "unregistered-test-provider"}},
		Mounts:  []config.Mount{{Path: filepath.Join(dir, "mount"), Layout: map[string]config.Layout{"/": {Remote: "ali"}}}},
	}
	sockdir, err := os.MkdirTemp("", "cfs-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockdir) })
	cfg.Control.Socket = filepath.Join(sockdir, "c.sock")
	b, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded, path
}

func uploadCLIJournal(t *testing.T, cfg *config.Config) (*journal.Journal, journal.Upload) {
	t.Helper()
	j, err := journal.Open(journal.Options{Dir: filepath.Join(cfg.StateDir(), "journal")})
	if err != nil {
		t.Fatal(err)
	}
	s, err := j.NewStaging([]provider.HashType{provider.HashSHA1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteAt([]byte("durable"), 0); err != nil {
		t.Fatal(err)
	}
	h, err := s.Hashes()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := j.CommitStaging(s, h)
	if err != nil {
		t.Fatal(err)
	}
	u := journal.Upload{ID: journal.NewID(), StagingID: s.ID, Remote: "ali", RemoteParentID: fakeprovider.RootID, Name: "a.txt", BlobPath: blob, Size: 7, Hashes: h}
	if err := j.Commit(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return j, u
}

func TestUploadsOfflineListDoesNotRecoverOrBuildProviders(t *testing.T) {
	cfg, path := uploadCLIConfig(t)
	j, u := uploadCLIJournal(t, cfg)
	defer j.Close()
	ctx := context.Background()
	if _, err := j.Claim(ctx, "ali", 1); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runUploads(ctx, []string{"list", "--json", "--config", path}, &out); err != nil {
		t.Fatal(err)
	}
	var result control.UploadResponse
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Uploads) != 1 || result.Uploads[0].ID != u.ID || result.Uploads[0].State != journal.StateUploading {
		t.Fatalf("list = %s", out.String())
	}
	row, err := j.Get(ctx, u.ID)
	if err != nil || row.State != journal.StateUploading || row.LastError != "" {
		t.Fatalf("read changed queue: %+v, %v", row, err)
	}
}

func TestUploadsOfflineAbsentListCreatesNothing(t *testing.T) {
	cfg, path := uploadCLIConfig(t)
	var out bytes.Buffer
	if err := runUploads(context.Background(), []string{"list", "--config", path}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.Cache.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read created cache: %v", err)
	}
}

func TestUploadsOfflineRetryRequiresOwnershipNotCredentials(t *testing.T) {
	cfg, path := uploadCLIConfig(t)
	j, u := uploadCLIJournal(t, cfg)
	ctx := context.Background()
	if err := j.Fail(ctx, u.ID, errors.New("offline")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	args := []string{"retry", u.ID, "--config", path}
	if err := runUploads(ctx, args, &out); err == nil {
		t.Fatal("mutated another owner's queue")
	}
	if row, _ := j.Get(ctx, u.ID); row.State != journal.StateDead {
		t.Fatal("failed command changed row")
	}
	j.Close()
	if err := runUploads(ctx, args, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "requeued 1") {
		t.Fatalf("retry output: %s", out.String())
	}
	ro, err := journal.OpenReadOnly(filepath.Join(cfg.StateDir(), "journal"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	row, err := ro.Get(ctx, u.ID)
	if err != nil || row.State != journal.StatePending {
		t.Fatalf("retry = %+v, %v", row, err)
	}
}

func TestUploadsOnlineRetryAndFlushUseOwner(t *testing.T) {
	cfg, path := uploadCLIConfig(t)
	j, u := uploadCLIJournal(t, cfg)
	defer j.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fake := fakeprovider.New("ali")
	up, err := upload.New(upload.Options{Journal: j, Providers: func(string) (provider.Provider, bool) { return fake, true }, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer up.Stop()
	if err := j.Fail(ctx, u.ID, errors.New("offline")); err != nil {
		t.Fatal(err)
	}
	s, err := control.NewServer(&control.Collector{Journal: j, FlushUploads: up.Flush}).Start(ctx, cfg.Control.Socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var out bytes.Buffer
	if err := runUploads(ctx, []string{"retry", u.ID, "--config", path}, &out); err != nil {
		t.Fatal(err)
	}
	// The original flush used DrainAll, which returned before this delay.
	if err := j.Defer(ctx, u.ID, 100*time.Millisecond, "settling"); err != nil {
		t.Fatal(err)
	}
	if err := runUploads(ctx, []string{"flush", "--timeout", "3s", "--config", path}, &out); err != nil {
		t.Fatal(err)
	}
	row, err := j.Get(ctx, u.ID)
	if err != nil || row.State != journal.StateDone {
		t.Fatalf("flush returned before completion: %+v, %v", row, err)
	}
	if fake.Calls("BeginUpload") != 1 {
		t.Fatalf("uploads = %d", fake.Calls("BeginUpload"))
	}
}

func TestUploadsFlagsFailBeforeOpeningState(t *testing.T) {
	for _, args := range [][]string{{"resume", "id"}, {"resume", "id", "--confirm", "--all"}, {"cancel"}, {"cancel", "id", "--all"}, {"cancel", "id", "--limit", "1"}, {"retry", "id", "--all"}, {"flush", "--timeout", "0s"}, {"list", "--limit", "-1"}, {"unknown"}, {"flush", "unexpected"}, {"list", "--jsoon"}} {
		var out bytes.Buffer
		if err := runUploads(context.Background(), args, &out); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestUploadsCancelOnlineAndOfflineRetainsContent(t *testing.T) {
	for _, online := range []bool{false, true} {
		t.Run(fmt.Sprint(online), func(t *testing.T) {
			cfg, path := uploadCLIConfig(t)
			j, u := uploadCLIJournal(t, cfg)
			ctx := context.Background()
			if online {
				defer j.Close()
				fake := fakeprovider.New("ali")
				up, err := upload.New(upload.Options{Journal: j, Providers: func(string) (provider.Provider, bool) { return fake, true }})
				if err != nil {
					t.Fatal(err)
				}
				defer up.Stop()
				r, err := control.NewServer(&control.Collector{Journal: j, CancelUpload: up.Cancel}).Start(ctx, cfg.Control.Socket, "")
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
			} else {
				if err := j.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			if err := runUploads(ctx, []string{"cancel", u.ID, "--json", "--config", path}, &out); err != nil {
				t.Fatal(err)
			}
			var result control.UploadResponse
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.State != journal.StateCancelled || !strings.Contains(result.Warning, "does not undo") {
				t.Fatalf("misleading result: %+v", result)
			}
			if _, err := os.Stat(u.BlobPath); err != nil {
				t.Fatalf("content lost: %v", err)
			}
		})
	}
}
