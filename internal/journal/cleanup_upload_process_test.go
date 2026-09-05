package journal

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
)

func TestUploadCleanupCrashHelper(t *testing.T) {
	dir := os.Getenv("CLOUDFS_TEST_UPLOAD_CLEANUP_DIR")
	if dir == "" {
		return
	}
	phase := os.Getenv("CLOUDFS_TEST_UPLOAD_CLEANUP_PHASE")
	j, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	u := stage(t, j, "discard-after-crash", []byte("owned until cleanup"))
	ctx := context.Background()
	if err := j.Commit(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := j.RequestCancel(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := j.BeginUploadCleanup(ctx, u.ID, cleanupTestIdentity); err != nil {
		t.Fatal(err)
	}
	stop := func() { fmt.Fprintln(os.Stdout, "upload-cleanup:"+u.ID); select {} }
	if phase == "intent" {
		stop()
	}
	j.uploadCleanupFault = func(at string) error {
		if phase == at {
			stop()
		}
		return nil
	}
	if err := j.FinishUploadCleanup(ctx, u.ID, cleanupTestIdentity); err != nil {
		t.Fatal(err)
	}
	t.Fatal("cleanup crash phase was not reached")
}

func TestUploadCleanupResumesAfterProcessKillAtEveryDurablePhase(t *testing.T) {
	for _, phase := range []string{"intent", "unlinked", "finalize"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			bin, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "-test.run=^TestUploadCleanupCrashHelper$")
			cmd.Env = append(os.Environ(), "CLOUDFS_TEST_UPLOAD_CLEANUP_DIR="+dir, "CLOUDFS_TEST_UPLOAD_CLEANUP_PHASE="+phase)
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
			if err != nil || !strings.HasPrefix(line, "upload-cleanup:") {
				t.Fatalf("helper signal=%q err=%v", line, err)
			}
			id := strings.TrimSpace(strings.TrimPrefix(line, "upload-cleanup:"))
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("helper was not killed")
			}
			j, err := Open(Options{Dir: dir})
			if err != nil {
				t.Fatal(err)
			}
			defer j.Close()
			if _, err := j.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			u, err := j.Get(ctx, id)
			if err != nil || u.State != StatePurging {
				t.Fatalf("restart lost cleanup state: %+v %v", u, err)
			}
			if phase == "intent" {
				if b, err := os.ReadFile(u.BlobPath); err != nil || string(b) != "owned until cleanup" {
					t.Fatalf("early recovery discarded data: %q %v", b, err)
				}
			} else if _, err := os.Stat(u.BlobPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("crash did not follow unlink: %v", err)
			}
			if rows, err := j.Claim(ctx, u.Remote, 1); err != nil || len(rows) != 0 {
				t.Fatalf("cleanup resumed upload: %+v %v", rows, err)
			}
			if err := j.FinishUploadCleanup(ctx, id, cleanupTestIdentity); err != nil {
				t.Fatal(err)
			}
			if _, err := j.Get(ctx, id); !errors.Is(err, ErrNotFound) {
				t.Fatal(err)
			}
			if err := j.Commit(ctx, u); !errors.Is(err, ErrUploadPurging) {
				t.Fatalf("restart lost discard fence: %v", err)
			}
		})
	}
}
