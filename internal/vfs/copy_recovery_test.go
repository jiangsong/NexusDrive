package vfs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
)

func copyRecoveryBody() []byte {
	return bytes.Repeat([]byte("checkpoint-content!"), 120000)
}

func TestCopyPreparationCrashHelper(t *testing.T) {
	dir := os.Getenv("CLOUDFS_TEST_COPY_PREPARATION_DIR")
	if dir == "" {
		return
	}
	e := publicationEnv(t, dir)
	e.fake.Seed("source", copyRecoveryBody())
	stop := func() error {
		fmt.Fprintln(os.Stdout, "copy-ready")
		select {} // Only the parent terminates this process.
	}
	if os.Getenv("CLOUDFS_TEST_COPY_PREPARATION_PHASE") == "cleanup" {
		e.fs.copyCheckpointFault = func(journal.CopyJob) error { return errors.New("stop preparation for cleanup test") }
		if _, err := e.fs.Copy(context.Background(), "/ali/source", "/ali/dest"); err == nil {
			t.Fatal("expected failed preparation")
		}
		jobs, err := e.j.CopyJobs(context.Background())
		if err != nil || len(jobs) != 1 {
			t.Fatalf("cleanup jobs: %+v %v", jobs, err)
		}
		e.fs.copyCleanupFault = stop
		err = e.fs.ForgetCopy(context.Background(), jobs[0].ID)
		t.Fatalf("cleanup helper should have been killed: %v", err)
	}
	if os.Getenv("CLOUDFS_TEST_COPY_PREPARATION_PHASE") == "bind" {
		e.fs.copyBindFault = stop
	} else {
		e.fs.copyCheckpointFault = func(job journal.CopyJob) error {
			if job.Checkpoint == 1<<20 {
				return stop()
			}
			return nil
		}
	}
	_, err := e.fs.Copy(context.Background(), "/ali/source", "/ali/dest")
	t.Fatalf("helper should have been killed: %v", err)
}

func killCopyPreparation(t *testing.T, phase string) string {
	t.Helper()
	dir := t.TempDir()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-test.run=^TestCopyPreparationCrashHelper$")
	cmd.Env = append(os.Environ(), "CLOUDFS_TEST_COPY_PREPARATION_DIR="+dir, "CLOUDFS_TEST_COPY_PREPARATION_PHASE="+phase)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, readErr := bufio.NewReader(stdout).ReadString('\n')
	killErr := cmd.Process.Kill()
	waitErr := cmd.Wait()
	if readErr != nil || line != "copy-ready\n" || killErr != nil || waitErr == nil {
		t.Fatalf("child: line=%q read=%v kill=%v wait=%v", line, readErr, killErr, waitErr)
	}
	return dir
}

func recoverCopyPreparation(t *testing.T, dir string) (*env, journal.CopyJob) {
	t.Helper()
	e := publicationEnv(t, dir)
	ctx := context.Background()
	if _, err := e.j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
		t.Fatal(err)
	}
	jobs, err := e.j.CopyJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%+v %v", jobs, err)
	}
	return e, jobs[0]
}

func TestCopyPreparationResumesAfterProcessKill(t *testing.T) {
	for _, phase := range []string{"download", "bind"} {
		t.Run(phase, func(t *testing.T) {
			dir := killCopyPreparation(t, phase)
			e, job := recoverCopyPreparation(t, dir)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			body := copyRecoveryBody()
			e.fake.Seed("source", body)
			var boundIno uint64
			if phase == "download" {
				if job.State != journal.CopyPreparing || job.Checkpoint != 1<<20 {
					t.Fatalf("lost checkpoint: %+v", job)
				}
				if _, err := e.store.Resolve(ctx, "/ali/dest"); !errors.Is(err, meta.ErrNotFound) {
					t.Fatalf("published partial download: %v", err)
				}
			} else {
				n, err := e.store.Resolve(ctx, "/ali/dest")
				if err != nil || job.State != journal.CopyReady {
					t.Fatalf("binding missing: %+v %+v %v", n, job, err)
				}
				boundIno = n.Ino
			}
			// Exercise the actual background scheduler, not only ResumeCopy.
			e.fs.StartCopies(ctx, time.Millisecond)
			defer e.fs.StopCopies()
			for {
				current, err := e.j.GetCopy(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.State == journal.CopySubmitted {
					// Submission owns the bytes but publication still has its
					// own gate. Stopping the worker here can cancel that gate.
					rows, _, err := e.j.ListActive(ctx, "", 10)
					if err != nil {
						t.Fatal(err)
					}
					if len(rows) == 1 && rows[0].ID == job.ID && !rows[0].NeedsPublish {
						break
					}
				}
				if ctx.Err() != nil {
					t.Fatalf("recovery stalled: %+v %s", current, e.fs.CopyWarning())
				}
				time.Sleep(time.Millisecond)
			}
			e.fs.StopCopies()
			wantDownload := int64(len(body)) - job.Checkpoint
			if got := e.fake.ReadBytes(); got != wantDownload {
				t.Fatalf("downloaded %d bytes, want only remaining %d", got, wantDownload)
			}
			got, err := e.fs.ReadFileRange(ctx, "/ali/dest", 0, int64(len(body)))
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("recovered content: size=%d %v", len(got), err)
			}
			n, err := e.store.Resolve(ctx, "/ali/dest")
			if err != nil || boundIno != 0 && n.Ino != boundIno {
				t.Fatalf("replaced bound inode: %+v %v", n, err)
			}
			rows, _, err := e.j.ListActive(ctx, "", 10)
			if err != nil || len(rows) != 1 || rows[0].ID != job.ID || rows[0].NeedsPublish {
				t.Fatalf("upload handoff: %+v %v", rows, err)
			}
			if _, err := e.up.Flush(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCopyRecoveryCancellationAfterSubmissionKeepsPublicationRecoverable(t *testing.T) {
	dir := killCopyPreparation(t, "bind")
	e, job := recoverCopyPreparation(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Deterministically reproduce shutdown between Submit and MarkPublished.
	e.fs.commitFault = func() error { cancel(); return nil }
	if _, err := e.fs.ResumeCopy(ctx, job.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("publication did not observe shutdown: %v", err)
	}
	e.fs.commitFault = nil
	check := context.Background()
	current, err := e.j.GetCopy(check, job.ID)
	if err != nil || current.State != journal.CopySubmitted {
		t.Fatalf("shutdown lost upload handoff: %+v %v", current, err)
	}
	rows, _, err := e.j.ListActive(check, "", 10)
	if err != nil || len(rows) != 1 || !rows[0].NeedsPublish {
		t.Fatalf("shutdown removed publication gate: %+v %v", rows, err)
	}
	if rows, err := e.j.Claim(check, "ali", 10); err != nil || len(rows) != 0 {
		t.Fatalf("claimed unpublished upload: %+v %v", rows, err)
	}
	// Replay startup recovery twice: no re-download or duplicate upload.
	for range 2 {
		if err := e.fs.RecoverPublications(check, e.j); err != nil {
			t.Fatal(err)
		}
	}
	rows, _, err = e.j.ListActive(check, "", 10)
	if err != nil || len(rows) != 1 || rows[0].ID != job.ID || rows[0].NeedsPublish {
		t.Fatalf("recovery did not open original gate: %+v %v", rows, err)
	}
	got, err := e.fs.ReadFileRange(check, "/ali/dest", 0, job.Spec.Size)
	if err != nil || !bytes.Equal(got, copyRecoveryBody()) || e.fake.ReadBytes() != 0 {
		t.Fatalf("recovery changed or re-downloaded content: %v", err)
	}
	if _, err := e.up.Flush(check); err != nil {
		t.Fatal(err)
	}
}

func TestCopyRecoveryRefusesChangedSource(t *testing.T) {
	dir := killCopyPreparation(t, "download")
	e, job := recoverCopyPreparation(t, dir)
	body := copyRecoveryBody()
	body[len(body)-1] ^= 1 // Same ID and length, different version.
	e.fake.Seed("source", body)
	ctx := context.Background()
	if _, err := e.fs.ResumeCopy(ctx, job.ID); !errors.Is(err, ErrCopySourceChanged) {
		t.Fatalf("resumed changed source: %v", err)
	}
	e.fs.resumeCopyJobs(ctx)
	current, err := e.j.GetCopy(ctx, job.ID)
	if err != nil || current.State != journal.CopyFailed || current.Checkpoint != job.Checkpoint {
		t.Fatalf("unsafe job not retained as failed: %+v %v", current, err)
	}
	payload, err := os.ReadFile(filepath.Join(dir, "journal", "copies", job.ID+".part"))
	if err != nil || !bytes.Equal(payload, body[:job.Checkpoint]) {
		t.Fatalf("checkpoint lost: size=%d %v", len(payload), err)
	}
	if e.fake.ReadBytes() != 0 {
		t.Fatal("downloaded changed source")
	}
	if _, err := e.store.Resolve(ctx, "/ali/dest"); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("published changed source: %v", err)
	}
}

func TestCopyRecoveryDoesNotResurrectRemovedBinding(t *testing.T) {
	dir := killCopyPreparation(t, "bind")
	e, job := recoverCopyPreparation(t, dir)
	ctx := context.Background()
	n, err := e.store.Resolve(ctx, "/ali/dest")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Remove(ctx, n.ParentIno, n.Name, false); err != nil {
		t.Fatal(err)
	}
	e.fs.resumeCopyJobs(ctx)
	if _, err := e.store.Resolve(ctx, "/ali/dest"); !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("resurrected deleted target: %v", err)
	}
	current, err := e.j.GetCopy(ctx, job.ID)
	if err != nil || current.State != journal.CopyFailed {
		t.Fatalf("deleted binding still scheduled: %+v %v", current, err)
	}
	if rows, _, err := e.j.ListActive(ctx, "", 10); err != nil || len(rows) != 0 {
		t.Fatalf("deleted target uploaded: %+v %v", rows, err)
	}
}

func TestCopyRecoveryBindingFailureKeepsReferencedContent(t *testing.T) {
	dir := killCopyPreparation(t, "bind")
	e, job := recoverCopyPreparation(t, dir)
	ctx := context.Background()
	n, err := e.store.Resolve(ctx, "/ali/dest")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an independently changed metadata tree. Normal VFS rename
	// refuses an unfinished publication, but recovery must also preserve
	// content when that invariant no longer holds in persisted metadata.
	if err := e.store.Rename(ctx, n.Ino, n.ParentIno, "moved"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.ResumeCopy(ctx, job.ID); !errors.Is(err, meta.ErrCopyTargetChanged) {
		t.Fatalf("accepted changed binding: %v", err)
	}
	body := copyRecoveryBody()
	got, err := e.fs.ReadFileRange(ctx, "/ali/moved", 0, int64(len(body)))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("failed recovery removed referenced content: size=%d %v", len(got), err)
	}
}

func TestCopyRecoveryChecksBindingsAndCancellation(t *testing.T) {
	for _, scenario := range []string{"source-mount", "target-mount", "readonly", "metadata", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			dir := killCopyPreparation(t, "download")
			e, job := recoverCopyPreparation(t, dir)
			e.fake.Seed("source", copyRecoveryBody())
			ctx := context.Background()
			want := ErrCopyBindingChanged
			switch scenario {
			case "source-mount":
				// The same mount is both source and destination in this fixture.
				job.Spec.SourceRootID = "another-root"
				// Persist a distinct job with an incompatible source binding.
				if err := e.j.FailCopy(ctx, job.ID, "test source binding"); err != nil {
					t.Fatal(err)
				}
				c, err := e.j.BeginCopy(ctx, job.Spec, job.Want)
				if err != nil {
					t.Fatal(err)
				}
				job = c.Job()
				c.Close()
			case "target-mount":
				e.fs.mounts[0].RootID = "another-root"
			case "readonly":
				e.fs.mounts[0].Mode = config.ModeReadonly
				want = ErrReadOnly
			case "metadata":
				other, err := meta.Open(filepath.Join(t.TempDir(), "fresh.db"), meta.Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
				e.fs.meta = other
			case "cancel":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
				want = context.Canceled
			}
			if _, err := e.fs.ResumeCopy(ctx, job.ID); !errors.Is(err, want) {
				t.Fatalf("recovery accepted %s: %v, want %v", scenario, err, want)
			}
			current, err := e.j.GetCopy(context.Background(), job.ID)
			if err != nil || current.State != job.State || current.Checkpoint != job.Checkpoint {
				t.Fatalf("failed attempt altered intent: %+v %v", current, err)
			}
			if e.fake.ReadBytes() != 0 {
				t.Fatal("unsafe recovery downloaded bytes")
			}
		})
	}
}
