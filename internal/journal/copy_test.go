package journal

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"cloudfs/internal/provider"
)

func copySpec(size int64) CopySpec {
	return CopySpec{MetaIdentity: "meta-identity", TargetParentIno: 1,
		SourceMount: "/source", SourceRootID: "source-root", SourceAccountBinding: "source-account",
		TargetMount: "/target", TargetRootID: "target-root", TargetAccountBinding: "target-account",
		SourcePath: "/source/file", SourceRemote: "source", SourceID: "file-id", SourceVersion: "v1",
		Size: size, TargetPath: "/target/file", TargetRemote: "target", TargetParentID: "root", Mode: "writeback"}
}

func TestCopySpecRejectsPartialAccountBinding(t *testing.T) {
	j, _, _ := openTest(t)
	spec := copySpec(0)
	c, err := j.BeginCopy(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := c.Job().ID
	c.Close()
	got, err := j.GetCopy(context.Background(), id)
	if err != nil || got.Spec.SourceAccountBinding != spec.SourceAccountBinding ||
		got.Spec.TargetAccountBinding != spec.TargetAccountBinding {
		t.Fatalf("binding round trip: %+v %v", got.Spec, err)
	}
	spec.TargetAccountBinding = ""
	if _, err := j.BeginCopy(context.Background(), spec, nil); err == nil || !strings.Contains(err.Error(), "incomplete copy account binding") {
		t.Fatalf("partial binding accepted: %v", err)
	}
}

func TestCopyCheckpointsResumeAndTransferExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	body := bytes.Repeat([]byte("checkpoint-content"), 20000)
	want := []provider.HashType{provider.HashSHA1, provider.HashSHA256, provider.HashSliceMD5, provider.HashPreSHA1}
	c, err := j.BeginCopy(ctx, copySpec(int64(len(body))), want)
	if err != nil {
		t.Fatal(err)
	}
	id := c.Job().ID
	const prefix = 12345
	if _, err := c.Write(body[:prefix]); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("unconfirmed tail")); err != nil {
		t.Fatal(err)
	}
	c.Close()
	j.Close()
	j, err = Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err := j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	c, err = j.ResumeCopy(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Job().Checkpoint != prefix || c.staging.Size() != prefix {
		t.Fatalf("did not remove uncheckpointed tail: %+v", c.Job())
	}
	if _, err := c.Write(body[prefix:]); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	u, err := c.Submit(ctx, Upload{Remote: "target", RemoteParentID: "root", Name: "file", Ino: 42, NeedsPublish: true})
	if err != nil {
		t.Fatal(err)
	}
	sha := sha1.Sum(body)
	sha256sum := sha256.Sum256(body)
	if u.ID != id || u.Hashes[provider.HashSHA1] != hex.EncodeToString(sha[:]) || u.Hashes[provider.HashSHA256] != hex.EncodeToString(sha256sum[:]) {
		t.Fatalf("handoff/hash mismatch: %+v", u)
	}
	f, err := os.Open(u.BlobPath)
	if err != nil {
		t.Fatal(err)
	}
	allHashes, err := hashFile(f, int64(len(body)), withCRC(want))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range allHashes {
		if u.Hashes[k] != v {
			t.Fatalf("resumed hash %s: %s want %s", k, u.Hashes[k], v)
		}
	}
	if _, err := c.Write([]byte("bad")); err == nil {
		t.Fatal("submitted content writable")
	}
	if _, err := c.Submit(ctx, u); err == nil {
		t.Fatal("duplicate submit accepted")
	}
	c.Close()
	if _, err := j.ResumeCopy(ctx, id); err == nil {
		t.Fatal("submitted job resumed as download")
	}
	if _, err := j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(u.BlobPath); err != nil || !bytes.Equal(b, body) {
		t.Fatalf("recovery deleted upload payload: %v", err)
	}
	if rows, err := j.Claim(ctx, "target", 10); err != nil || len(rows) != 0 {
		t.Fatalf("publication gate bypassed: %v %v", rows, err)
	}
	if err := j.MarkPublished(ctx, id); err != nil {
		t.Fatal(err)
	}
	if rows, err := j.Claim(ctx, "target", 10); err != nil || len(rows) != 1 {
		t.Fatalf("handoff not uploadable: %v %v", rows, err)
	}
}

type copySyncFailure struct{ stagingFile }

func (copySyncFailure) Sync() error { return syscall.EIO }

func TestCopyCheckpointDoesNotAdvanceOnSyncOrDatabaseFailure(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	c, err := j.BeginCopy(ctx, copySpec(6), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	original := c.staging.f
	c.staging.f = copySyncFailure{original}
	if err := c.Checkpoint(ctx); !errors.Is(err, syscall.EIO) {
		t.Fatalf("sync=%v", err)
	}
	c.staging.f = original
	job, err := j.GetCopy(ctx, c.Job().ID)
	if err != nil || job.Checkpoint != 0 {
		t.Fatalf("advanced failed sync: %+v %v", job, err)
	}
	if _, err := j.db.Exec(`CREATE TRIGGER fail_checkpoint BEFORE UPDATE ON copy_jobs BEGIN SELECT RAISE(ABORT, 'checkpoint failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err == nil {
		t.Fatal("database failure ignored")
	}
	if c.Job().Checkpoint != 0 {
		t.Fatal("in-memory checkpoint advanced after rollback")
	}
	if _, err := j.db.Exec(`DROP TRIGGER fail_checkpoint`); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCopySubmitRollsBackBothRecords(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	c, err := j.BeginCopy(ctx, copySpec(0), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := j.db.Exec(`CREATE TRIGGER fail_copy_upload BEFORE INSERT ON uploads BEGIN SELECT RAISE(ABORT, 'upload failure'); END`); err != nil {
		t.Fatal(err)
	}
	u := Upload{Remote: "target", RemoteParentID: "root", Name: "file"}
	if _, err := c.Submit(ctx, u); err == nil {
		t.Fatal("submit failure ignored")
	}
	if job, err := j.GetCopy(ctx, c.Job().ID); err != nil || job.State != CopyReady {
		t.Fatalf("lost ready checkpoint: %+v %v", job, err)
	}
	if _, err := j.Get(ctx, c.Job().ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial upload inserted: %v", err)
	}
	if _, err := j.db.Exec(`DROP TRIGGER fail_copy_upload`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Submit(ctx, u); err != nil {
		t.Fatal(err)
	}
}

func TestCopyCheckpointCorruptionAndSymlinkArePreservedNotResumed(t *testing.T) {
	for _, kind := range []string{"truncate", "corrupt", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			j, _, _ := openTest(t)
			ctx := context.Background()
			c, err := j.BeginCopy(ctx, copySpec(6), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Write([]byte("abc")); err != nil {
				t.Fatal(err)
			}
			if err := c.Checkpoint(ctx); err != nil {
				t.Fatal(err)
			}
			id := c.Job().ID
			p := c.staging.Path
			c.Close()
			switch kind {
			case "truncate":
				err = os.Truncate(p, 1)
			case "corrupt":
				err = os.WriteFile(p, []byte("bad-tail"), 0600)
			case "symlink":
				target := filepath.Join(t.TempDir(), "outside")
				if err := os.Rename(p, target); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(target, p)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := j.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := j.ResumeCopy(ctx, id); !errors.Is(err, ErrCopyCorrupt) {
				t.Fatalf("resumed corrupt checkpoint: %v", err)
			}
			if _, err := os.Lstat(p); err != nil {
				t.Fatal("deleted corrupt payload", err)
			}
			if err := j.FailCopy(ctx, id, "checkpoint requires inspection"); err != nil {
				t.Fatal(err)
			}
			if _, err := j.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(p); err != nil {
				t.Fatal("deleted failed copy payload", err)
			}
		})
	}
}

func TestCopyDestinationAndHandleHaveExclusiveOwners(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	c, err := j.BeginCopy(ctx, copySpec(6), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := j.BeginCopy(ctx, copySpec(6), nil); !errors.Is(err, ErrCopyBusy) {
		t.Fatalf("duplicate destination: %v", err)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if other, err := j.ResumeCopy(ctx, c.Job().ID); !errors.Is(err, ErrCopyBusy) {
				if other != nil {
					other.Close()
				}
				t.Errorf("parallel resume: %v", err)
			}
		}()
	}
	wg.Wait()
	if _, err := c.Write(make([]byte, 7)); err == nil {
		t.Fatal("accepted oversize content")
	}
	if files, err := os.ReadDir(filepath.Join(j.dir, "copies")); err != nil || len(files) != 1 {
		t.Fatalf("duplicate left orphan: %v %v", files, err)
	}
}

func TestCopyRecoveryKeepsOwnedPreparationsButCleansOrphans(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	c, err := j.BeginCopy(ctx, copySpec(3), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	p := c.staging.Path
	c.Close()
	ordinary, err := j.NewStaging(nil)
	if err != nil {
		t.Fatal(err)
	}
	ordinary.f.Close()
	orphan := filepath.Join(j.dir, "copies", "orphan")
	if err := os.WriteFile(orphan, []byte("unowned"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(j.dir, "copies", "unowned-symlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "a" {
		t.Fatalf("deleted owned preparation: %q %v", b, err)
	}
	for _, p := range []string{ordinary.Path, orphan} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("orphan remained: %s %v", p, err)
		}
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "untouched" {
		t.Fatal("followed orphan symlink", err)
	}
}

func TestCopyPreparationUsesSpaceAdmissionAndRequiresOwner(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(Options{Dir: dir, ReserveSpace: func(p string, n int64) (func(), error) {
		if filepath.Base(p) != "copies" || n != 3 {
			t.Fatalf("wrong copy reservation: %s %d", p, n)
		}
		return nil, syscall.ENOSPC
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx := context.Background()
	c, err := j.BeginCopy(ctx, copySpec(3), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if n, err := c.Write([]byte("abc")); n != 0 || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("admission=%d %v", n, err)
	}
	if c.staging.Size() != 0 || c.Job().Checkpoint != 0 {
		t.Fatal("failed write advanced preparation")
	}
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if jobs, err := ro.CopyJobs(ctx); err != nil || len(jobs) != 1 {
		t.Fatalf("read-only copy inspection: %v %v", jobs, err)
	}
	if _, err := ro.BeginCopy(ctx, copySpec(3), nil); err == nil {
		t.Fatal("read-only created preparation")
	}
	if _, err := ro.ResumeCopy(ctx, c.Job().ID); err == nil {
		t.Fatal("read-only resumed preparation")
	}
}

func TestDroppingUploadDoesNotDeletePreparationOwner(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	c, err := j.BeginCopy(ctx, copySpec(3), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	// Exercise the general blob ownership check with a separate queue row.
	u := Upload{ID: NewID(), Remote: "target", RemoteParentID: "root", Name: "other", BlobPath: c.staging.Path, Size: 3}
	if err := j.Commit(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := j.Drop(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(c.staging.Path); err != nil || string(b) != "abc" {
		t.Fatalf("deleted preparation-owned bytes: %q %v", b, err)
	}
	submitted, err := c.Submit(ctx, Upload{Remote: "target", RemoteParentID: "root", Name: "file"})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := j.Drop(ctx, submitted.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(submitted.BlobPath); !os.IsNotExist(err) {
		t.Fatalf("submitted intent retained released payload: %v", err)
	}
}

func TestCopyPayloadPathsSurviveRelativeToAbsoluteJournalReopen(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, dir)
	if err != nil {
		t.Fatal(err)
	}
	j, err := Open(Options{Dir: rel})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, err := j.BeginCopy(ctx, copySpec(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	u, err := c.Submit(ctx, Upload{Remote: "target", RemoteParentID: "root", Name: "file"})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	j.Close()
	if !filepath.IsAbs(u.BlobPath) {
		t.Fatal("persisted relative payload path")
	}
	j, err = Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err := j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(u.BlobPath); err != nil || string(b) != "x" {
		t.Fatalf("lost relative-root copy: %q %v", b, err)
	}
}

func TestCopyFromWholeNeverTruncatesSharedInode(t *testing.T) {
	j, _, _ := openTest(t)
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "immutable-source")
	if err := os.WriteFile(p, []byte("immutable"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := j.BeginCopyFromWhole(ctx, copySpec(9), f, []provider.HashType{provider.HashSHA1})
	if err != nil {
		t.Fatal(err)
	}
	id := c.Job().ID
	a, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(c.staging.Path)
	if err != nil || !os.SameFile(a, b) || c.Job().State != CopyReady {
		t.Fatalf("not a shared ready inode: %v", err)
	}
	c.Close()
	if _, err := j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	c, err = j.ResumeCopy(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("bad")); err == nil {
		t.Fatal("shared checkpoint became writable")
	}
	if err := c.staging.Truncate(0); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("shared staging truncate=%v", err)
	}
	u, err := c.Submit(ctx, Upload{Remote: "target", RemoteParentID: "root", Name: "file"})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := j.Drop(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(p); err != nil || string(got) != "immutable" {
		t.Fatalf("source changed after resume/drop: %q %v", got, err)
	}
}

func TestCopyPreparationCrashHelper(t *testing.T) {
	dir := os.Getenv("CLOUDFS_TEST_COPY_PREPARATION_DIR")
	if dir == "" {
		return
	}
	j, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	c, err := j.BeginCopy(context.Background(), copySpec(6), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("bad")); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "copy-checkpoint-ready")
	select {}
}

func TestCopyPreparationSurvivesProcessKill(t *testing.T) {
	dir := t.TempDir()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-test.run=^TestCopyPreparationCrashHelper$")
	cmd.Env = append(os.Environ(), "CLOUDFS_TEST_COPY_PREPARATION_DIR="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "copy-checkpoint-ready\n" {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("helper=%q %v", line, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		cmd.Wait()
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
	jobs, err := j.CopyJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("lost intent: %v %v", jobs, err)
	}
	c, err := j.ResumeCopy(ctx, jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Job().Checkpoint != 3 {
		t.Fatalf("offset=%d", c.Job().Checkpoint)
	}
	if _, err := c.Write([]byte("def")); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	u, err := c.Submit(ctx, Upload{Remote: "target", RemoteParentID: "root", Name: "file"})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(u.BlobPath); err != nil || string(b) != "abcdef" {
		t.Fatalf("resumed payload=%q %v", b, err)
	}
}
