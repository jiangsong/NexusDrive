package vfs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/test/fakeprovider"
)

func TestCopyPublicationFaultIsGatedAndRecoverable(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("source", []byte("recover this copy"))
	fault := errors.New("publication fault")
	e.fs.commitFault = func() error { return fault }
	if _, err := e.fs.Copy(ctx, "/ali/source", "/ali/dest"); !errors.Is(err, fault) {
		t.Fatalf("fault=%v", err)
	}
	if rows, err := e.j.Claim(ctx, "ali", 10); err != nil || len(rows) != 0 {
		t.Fatalf("worker raced metadata: %v %v", rows, err)
	}
	e.fs.commitFault = nil
	if _, err := e.j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
			t.Fatal(err)
		}
		got, err := e.fs.ReadFileRange(ctx, "/ali/dest", 0, 17)
		if err != nil || string(got) != "recover this copy" {
			t.Fatalf("recovery=%q %v", got, err)
		}
	}
	if _, err := e.up.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	n, err := e.store.Resolve(ctx, "/ali/dest")
	if err != nil || IsLocalOnly(n.RemoteID) || n.Dirty {
		t.Fatalf("upload did not adopt recovered version: %+v %v", n, err)
	}
}

func TestFailedCachePublicationDoesNotExposeNewMetadata(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("source", []byte("new copy"))
	// The cache directory cannot accept a new complete-file link. The
	// durable journal must retain the bytes without publishing their key.
	dir := filepath.Join(filepath.Dir(filepath.Dir(e.j.StagingDir())), "cache", "hydrated")
	moved := dir + ".saved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Copy(ctx, "/ali/source", "/ali/dest"); err == nil {
		t.Fatal("cache failure was ignored")
	}
	n, err := e.store.Resolve(ctx, "/ali/dest")
	if !errors.Is(err, meta.ErrNotFound) {
		t.Fatalf("exposed unavailable content: %+v %v", n, err)
	}
	rows, err := e.j.CopyJobs(ctx)
	if err != nil || len(rows) != 1 || rows[0].State != journal.CopyFailed || rows[0].Checkpoint != 8 {
		t.Fatalf("lost durable intent: %v %v", rows, err)
	}
	payload := filepath.Join(filepath.Dir(e.j.StagingDir()), "copies", rows[0].ID+".part")
	if b, err := os.ReadFile(payload); err != nil || string(b) != "new copy" {
		t.Fatalf("failed copy payload=%q %v", b, err)
	}
	if claimed, err := e.j.Claim(ctx, "ali", 10); err != nil || len(claimed) != 0 {
		t.Fatalf("claimed failed publication: %v %v", claimed, err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := e.j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Copy(ctx, "/ali/source", "/ali/dest"); err != nil {
		t.Fatal(err)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/dest", 0, 8)
	if err != nil || string(got) != "new copy" {
		t.Fatalf("repair=%q %v", got, err)
	}
}

func TestPublicationRecoveryDoesNotRecreateDeletedInode(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("source", []byte("old"))
	e.fs.commitFault = func() error { return errors.New("crash before metadata") }
	if _, err := e.fs.Copy(ctx, "/ali/source", "/ali/dest"); err == nil {
		t.Fatal("fault not injected")
	}
	n, err := e.store.Resolve(ctx, "/ali/dest")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.Remove(ctx, n.Ino); err != nil {
		t.Fatal(err)
	}
	e.fs.commitFault = nil
	if _, err := e.fs.WriteFile(ctx, "/ali/dest", []byte("replacement"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
		t.Fatal(err)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/dest", 0, 11)
	if err != nil || string(got) != "replacement" {
		t.Fatalf("recreated deleted version: %q %v", got, err)
	}
	rows, _, err := e.j.ListActive(ctx, "", 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("queue after missing target: %v %v", rows, err)
	}
	for _, row := range rows {
		if row.Ino == n.Ino {
			if row.State != journal.StateDead {
				t.Fatalf("missing target was made uploadable: %+v", row)
			}
			data, err := os.ReadFile(row.BlobPath)
			if err != nil || string(data) != "old" {
				t.Fatalf("lost unreferenced durable payload: %q %v", data, err)
			}
		}
	}
}

func TestPublicationRecoveryRebuildsLostCacheLinkAfterAcknowledgment(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.WriteFile(ctx, "/ali/file", []byte("only journal remains"), false); err != nil {
		t.Fatal(err)
	}
	n, err := e.store.Resolve(ctx, "/ali/file")
	if err != nil {
		t.Fatal(err)
	}
	key := cache.FileKey{Remote: n.Remote, RemoteID: n.RemoteID, Version: n.Version}
	e.cache.Forget(key)
	if _, err := e.j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	before := e.fake.TotalCalls()
	if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
		t.Fatal(err)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/file", 0, 20)
	if err != nil || string(got) != "only journal remains" {
		t.Fatalf("read after cache loss=%q %v", got, err)
	}
	if e.fake.TotalCalls() != before {
		t.Fatal("recovery downloaded local-only content")
	}
}

func TestPublicationRecoveryPreservesAcknowledgedPayloadWithMissingMetadata(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.WriteFile(ctx, "/ali/file", []byte("acknowledged"), false); err != nil {
		t.Fatal(err)
	}
	n, err := e.store.Resolve(ctx, "/ali/file")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.Remove(ctx, n.Ino); err != nil {
		t.Fatal(err)
	}
	if _, err := e.j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
		t.Fatal(err)
	}
	rows, _, err := e.j.ListActive(ctx, "", 10)
	if err != nil || len(rows) != 1 || rows[0].State != journal.StateDead {
		t.Fatalf("missing metadata discarded acknowledged data: %v %v", rows, err)
	}
	b, err := os.ReadFile(rows[0].BlobPath)
	if err != nil || string(b) != "acknowledged" {
		t.Fatalf("lost payload: %q %v", b, err)
	}
}

// publicationEnv uses explicit paths so the parent can reconstruct every
// component after killing a child. Nothing from the child's heap is reused.
func publicationEnv(t *testing.T, dir string) *env {
	t.Helper()
	s, err := meta.Open(filepath.Join(dir, "meta.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c, err := cache.New(cache.Options{Dir: filepath.Join(dir, "cache"), BlockSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	j, err := journal.Open(journal.Options{Dir: filepath.Join(dir, "journal")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	p := fakeprovider.New("ali")
	f, err := New(Options{Meta: s, Cache: c, Mounts: []Mount{{Prefix: "/ali", Remote: "ali", RootID: fakeprovider.RootID, Provider: p, Mode: config.ModeWriteback}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	u, err := upload.New(upload.Options{Journal: j, Providers: func(name string) (provider.Provider, bool) { return p, name == "ali" }, Hooks: f.UploadHooks()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(u.Stop)
	f.SetWriteBackend(j, u)
	return &env{fs: f, fake: p, store: s, cache: c, j: j, up: u}
}

func TestPublicationCrashHelper(t *testing.T) {
	dir := os.Getenv("CLOUDFS_TEST_PUBLICATION_CRASH_DIR")
	if dir == "" {
		return
	}
	e := publicationEnv(t, dir)
	e.fake.Seed("source", []byte("survives kill"))
	e.fs.commitFault = func() error {
		fmt.Fprintln(os.Stdout, "publication-ready")
		select {} // the parent kills us after durable journal commit
	}
	_, err := e.fs.Copy(context.Background(), "/ali/source", "/ali/dest")
	t.Fatalf("helper should have been killed: %v", err)
}

func TestCopyPublicationSurvivesProcessKill(t *testing.T) {
	dir := t.TempDir()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-test.run=^TestPublicationCrashHelper$")
	cmd.Env = append(os.Environ(), "CLOUDFS_TEST_PUBLICATION_CRASH_DIR="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "publication-ready\n" {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("helper readiness: %q %v", line, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		cmd.Wait()
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("helper exited successfully instead of being killed")
	}
	e := publicationEnv(t, dir)
	if _, err := e.j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, err := e.j.Claim(ctx, "ali", 10); err != nil || len(rows) != 0 {
		t.Fatalf("restart skipped publication barrier: %v %v", rows, err)
	}
	if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
		t.Fatal(err)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/dest", 0, 13)
	if err != nil || string(got) != "survives kill" {
		t.Fatalf("after kill: %q %v", got, err)
	}
	if e.fake.TotalCalls() != 0 {
		t.Fatal("recovery needed a provider call")
	}
	if _, err := e.up.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	n, err := e.store.Resolve(ctx, "/ali/dest")
	if err != nil || n.Dirty || IsLocalOnly(n.RemoteID) {
		t.Fatalf("not uploaded: %+v %v", n, err)
	}
}
