package vfs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
)

func TestVFSUploadCleanupCrashHelper(t *testing.T) {
	dir := os.Getenv("CLOUDFS_TEST_VFS_UPLOAD_CLEANUP_DIR")
	if dir == "" {
		return
	}
	e := publicationEnv(t, dir)
	ctx := context.Background()
	if _, err := e.fs.WriteFile(ctx, "/ali/retained", []byte("crash-safe cleanup"), false); err != nil {
		t.Fatal(err)
	}
	rows, err := e.j.Pending(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queue: %+v %v", rows, err)
	}
	u := rows[0]
	if _, err := e.up.Cancel(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	phase := os.Getenv("CLOUDFS_TEST_VFS_UPLOAD_CLEANUP_PHASE")
	e.fs.uploadCleanupFault = func(at string) error {
		if at == phase {
			fmt.Fprintln(os.Stdout, "vfs-cleanup:"+u.ID)
			select {}
		}
		return nil
	}
	if err := e.fs.DiscardUpload(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	t.Fatal("cleanup crash boundary was not reached")
}

func TestVFSUploadCleanupContinuesAfterProcessKill(t *testing.T) {
	for _, phase := range []string{"intent", "metadata", "cache"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			bin, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "-test.run=^TestVFSUploadCleanupCrashHelper$")
			cmd.Env = append(os.Environ(), "CLOUDFS_TEST_VFS_UPLOAD_CLEANUP_DIR="+dir, "CLOUDFS_TEST_VFS_UPLOAD_CLEANUP_PHASE="+phase)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { cmd.Process.Kill(); cmd.Wait() }()
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "vfs-cleanup:") {
				t.Fatalf("helper signal=%q %v", line, err)
			}
			id := strings.TrimSpace(strings.TrimPrefix(line, "vfs-cleanup:"))
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("helper was not killed")
			}
			e := publicationEnv(t, dir)
			if _, err := e.j.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			u, err := e.j.Get(ctx, id)
			if err != nil || u.State != journal.StatePurging {
				t.Fatalf("intent lost: %+v %v", u, err)
			}
			if b, err := os.ReadFile(u.BlobPath); err != nil || string(b) != "crash-safe cleanup" {
				t.Fatalf("early payload loss: %q %v", b, err)
			}
			_, nodeErr := e.store.Get(ctx, u.Ino)
			if phase == "intent" && nodeErr != nil || phase != "intent" && !errors.Is(nodeErr, meta.ErrNotFound) {
				t.Fatalf("wrong durable boundary: %v", nodeErr)
			}
			for range 2 {
				if err := e.fs.RecoverUploadCleanups(ctx); err != nil {
					t.Fatal(err)
				}
				if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.j.Get(ctx, id); !errors.Is(err, journal.ErrNotFound) {
				t.Fatal(err)
			}
			if _, err := e.store.Get(ctx, u.Ino); !errors.Is(err, meta.ErrNotFound) {
				t.Fatalf("recovery revived node: %v", err)
			}
			if _, err := os.Stat(u.BlobPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("payload survived cleanup: %v", err)
			}
			if err := e.j.Commit(ctx, u); !errors.Is(err, journal.ErrUploadPurging) {
				t.Fatalf("discarded ID revived: %v", err)
			}
			if e.fake.TotalCalls() != 0 {
				t.Fatal("recovery issued remote requests")
			}
		})
	}
}
