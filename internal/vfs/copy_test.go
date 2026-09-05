package vfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/provider"
	"cloudfs/internal/upload"
	"cloudfs/test/fakeprovider"
)

func TestCopyStreamsAndJournalsWithoutChangingSource(t *testing.T) {
	e := newEnv(t, envOpt{})
	body := bytes.Repeat([]byte("copy-me"), 300)
	e.fake.Seed("source", body)
	a, err := e.fs.Copy(context.Background(), "/ali/source", "/ali/dest")
	if err != nil || a.Size != int64(len(body)) {
		t.Fatalf("copy: %+v %v", a, err)
	}
	got, err := e.fs.ReadFileRange(context.Background(), "/ali/dest", 0, int64(len(body)))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("dest: %q %v", got, err)
	}
	rows, _, err := e.j.ListActive(context.Background(), "", 10)
	if err != nil || len(rows) != 1 || rows[0].Name != "dest" {
		t.Fatalf("queue: %v %v", rows, err)
	}
	jobs, err := e.j.CopyJobs(context.Background())
	if err != nil || len(jobs) != 1 || jobs[0].Spec.SourceAccountBinding != e.fs.mounts[0].AccountBinding ||
		jobs[0].Spec.TargetAccountBinding != e.fs.mounts[0].AccountBinding {
		t.Fatalf("copy account binding: %+v %v", jobs, err)
	}
	if _, err := e.fs.Copy(context.Background(), "/ali/source", "/ali/dest"); !errors.Is(err, ErrExists) {
		t.Fatalf("clobber: %v", err)
	}
	if _, err := e.up.DrainAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err = e.fs.ReadFileRange(context.Background(), "/ali/source", 0, int64(len(body)))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("source changed", err)
	}
}

func TestCopyRecoveryRejectsSourceAndTargetAccountChangesBeforeProviderIO(t *testing.T) {
	for _, changed := range []string{"source", "target"} {
		t.Run(changed, func(t *testing.T) {
			e := newEnv(t, envOpt{})
			ctx := context.Background()
			target := fakeprovider.New("other")
			e.fs.mounts = append(e.fs.mounts, Mount{Prefix: "/other", Remote: "other", RootID: fakeprovider.RootID,
				AccountBinding: "target-account", Provider: target, Mode: config.ModeWriteback})
			if err := e.fs.ensureMountDirs(ctx); err != nil {
				t.Fatal(err)
			}
			e.fake.Seed("source", bytes.Repeat([]byte("s"), 2<<20))
			e.fs.copyCheckpointFault = func(journal.CopyJob) error { return errors.New("stop after checkpoint") }
			if _, err := e.fs.Copy(ctx, "/ali/source", "/other/dest"); err == nil {
				t.Fatal("copy preparation did not stop")
			}
			jobs, err := e.j.CopyJobs(ctx)
			if err != nil || len(jobs) != 1 || jobs[0].State != journal.CopyFailed {
				t.Fatalf("jobs: %+v %v", jobs, err)
			}
			e.fs.copyCheckpointFault = nil
			if changed == "source" {
				e.fs.mounts[0].AccountBinding = "changed-source-account"
			} else {
				e.fs.mounts[1].AccountBinding = "changed-target-account"
			}
			sourceCalls, targetCalls := e.fake.TotalCalls(), target.TotalCalls()
			err = e.fs.RetryCopy(ctx, jobs[0].ID)
			if changed == "source" && err == nil {
				_, err = e.fs.ResumeCopy(ctx, jobs[0].ID)
			}
			if !errors.Is(err, ErrCopyBindingChanged) {
				t.Fatalf("account change accepted: %v", err)
			}
			if e.fake.TotalCalls() != sourceCalls || target.TotalCalls() != targetCalls {
				t.Fatal("account binding rejection queried a provider")
			}
		})
	}
}

func TestCopyPendingLocalContentReusesJournalInode(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.WriteFile(ctx, "/ali/source", []byte("pending"), false); err != nil {
		t.Fatal(err)
	}
	before := e.fake.Calls("ReadRange")
	if _, err := e.fs.Copy(ctx, "/ali/source", "/ali/dest"); err != nil {
		t.Fatal(err)
	}
	if e.fake.Calls("ReadRange") != before {
		t.Fatal("downloaded local-only data")
	}
	rows, _, err := e.j.ListActive(ctx, "", 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows: %v %v", rows, err)
	}
	a, _ := os.Stat(rows[0].BlobPath)
	b, _ := os.Stat(rows[1].BlobPath)
	if !os.SameFile(a, b) {
		t.Fatal("did not share immutable data")
	}
	if files, _ := os.ReadDir(e.j.StagingDir()); len(files) != 0 {
		t.Fatalf("left linked staging aliases: %v", files)
	}
}

func TestCopyFailureBeforePublicationLeavesNoDestination(t *testing.T) {
	e := newEnv(t, envOpt{})
	e.fake.Seed("source", []byte("body"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.fs.Copy(ctx, "/ali/source", "/ali/dest"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := e.fs.StatPath(context.Background(), "/ali/dest"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel left target: %v", err)
	}
	e.j.SetSpaceReserver(func(string, int64) (func(), error) { return nil, cache.ErrNoSpace })
	if _, err := e.fs.Copy(context.Background(), "/ali/source", "/ali/dest"); !errors.Is(err, cache.ErrNoSpace) {
		t.Fatalf("space: %v", err)
	}
	if _, err := e.fs.StatPath(context.Background(), "/ali/dest"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed download left target: %v", err)
	}
}

type nativeCopier struct {
	*fakeprovider.Fake
	copies int
	err    error
	body   []byte
	entry  *provider.Entry
}

func (p *nativeCopier) Capabilities() provider.Caps {
	c := p.Fake.Capabilities()
	c.ServerCopy = true
	return c
}
func (p *nativeCopier) Copy(ctx context.Context, id, parent, name string) (provider.Entry, error) {
	p.copies++
	if p.err != nil {
		return provider.Entry{}, p.err
	}
	if p.entry != nil {
		return *p.entry, nil
	}
	return p.Fake.Seed(name, p.body), nil
}

func TestCopyRejectsMalformedNativeSuccessWithoutReplay(t *testing.T) {
	for _, entry := range []provider.Entry{
		{},
		{ID: "directory", Kind: provider.KindDir},
		{ID: "negative", Kind: provider.KindFile, Size: -1},
	} {
		e := newEnv(t, envOpt{})
		p := &nativeCopier{Fake: e.fake, entry: &entry}
		e.fs.mounts[0].Provider = p
		e.fake.Seed("source", []byte("body"))
		if _, err := e.fs.Copy(context.Background(), "/ali/source", "/ali/dest"); err == nil {
			t.Fatal("accepted malformed result")
		}
		if p.copies != 1 || p.Calls("ReadRange") != 0 {
			t.Fatal("replayed malformed success")
		}
		if rows, _, err := e.j.ListActive(context.Background(), "", 10); err != nil || len(rows) != 0 {
			t.Fatalf("queue=%v %v", rows, err)
		}
	}
}

func TestCopyConcurrentDestinationHasOneWinner(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	if _, err := e.fs.WriteFile(ctx, "/ali/source", []byte("body"), false); err != nil {
		t.Fatal(err)
	}
	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := e.fs.Copy(ctx, "/ali/source", "/ali/dest"); errs <- err }()
	}
	wg.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrExists) {
			t.Fatalf("copy race: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners=%d", winners)
	}
	rows, _, err := e.j.ListActive(ctx, "", 20)
	if err != nil || len(rows) != 2 {
		t.Fatalf("unexpected queue: %v %v", rows, err)
	}
	if files, err := os.ReadDir(e.j.StagingDir()); err != nil || len(files) != 0 {
		t.Fatalf("staging leaks=%v %v", files, err)
	}
}

func TestCopyRejectsCachedSizeMismatch(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	entry := e.fake.Seed("source", []byte("body"))
	if _, err := e.fs.StatPath(ctx, "/ali/source"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "wrong-size")
	if err := os.WriteFile(p, []byte("truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	key := cache.FileKey{Remote: "ali", RemoteID: entry.ID, Version: entry.Version}
	if err := e.cache.LinkFile(key, p, 9); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Copy(ctx, "/ali/source", "/ali/dest"); err == nil {
		t.Fatal("copied a different-sized cached version")
	}
	if _, err := e.fs.StatPath(ctx, "/ali/dest"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("published bad content: %v", err)
	}
}

func TestCopyUsesServerCapabilityAndDoesNotReplayAmbiguousFailure(t *testing.T) {
	for _, backendErr := range []error{nil, provider.ErrUnsupported, provider.ErrTransient} {
		e := newEnv(t, envOpt{})
		p := &nativeCopier{Fake: e.fake, err: backendErr, body: []byte("server")}
		e.fs.mounts[0].Provider = p
		e.fake.Seed("source", p.body)
		_, err := e.fs.Copy(context.Background(), "/ali/source", "/ali/dest")
		if p.copies != 1 {
			t.Fatalf("copies=%d", p.copies)
		}
		if backendErr == provider.ErrTransient {
			if err == nil || e.fake.Calls("ReadRange") != 0 {
				t.Fatalf("ambiguous failure replayed: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		if backendErr == nil && e.fake.Calls("ReadRange") != 0 {
			t.Fatal("native copy downloaded data")
		}
		rows, _, _ := e.j.ListActive(context.Background(), "", 10)
		want := 0
		if backendErr == provider.ErrUnsupported {
			want = 1
		}
		if len(rows) != want {
			t.Fatalf("queue: %d want %d", len(rows), want)
		}
	}
}

func TestCrossRemoteCachedCopyReusesInodeAndSurvivesJournalReopen(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	target := fakeprovider.New("other")
	e.fs.mounts = append(e.fs.mounts, Mount{Prefix: "/other", Remote: "other", RootID: fakeprovider.RootID,
		AccountBinding: "other-account-generation", Provider: target, Mode: config.ModeWriteback})
	if err := e.fs.ensureMountDirs(ctx); err != nil {
		t.Fatal(err)
	}
	body := []byte("cross-remote")
	// The destination already has these bytes under another name: after
	// restart its rapid-upload handshake must avoid all UploadPart traffic.
	target.Seed("same-content", body)
	entry := e.fake.Seed("source", body)
	if _, err := e.fs.StatPath(ctx, "/ali/source"); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(src, body, 0600); err != nil {
		t.Fatal(err)
	}
	key := cache.FileKey{Remote: "ali", RemoteID: entry.ID, Version: entry.Version}
	if err := e.cache.LinkFile(key, src, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	before := e.fake.Calls("ReadRange")
	if _, err := e.fs.Copy(ctx, "/ali/source", "/other/dest"); err != nil {
		t.Fatal(err)
	}
	if e.fake.Calls("ReadRange") != before {
		t.Fatal("cross copy redownloaded cached bytes")
	}
	rows, _, _ := e.j.ListActive(ctx, "", 10)
	if len(rows) != 1 {
		t.Fatalf("rows: %v", rows)
	}
	a, _ := os.Stat(src)
	b, _ := os.Stat(rows[0].BlobPath)
	if !os.SameFile(a, b) {
		t.Fatal("cross copy allocated duplicate payload")
	}
	// A recreated uploader reads only persisted state, not the Copy stack.
	dir := filepath.Dir(e.j.StagingDir())
	e.j.Close()
	j, err := journal.Open(journal.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err := j.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	u, err := upload.New(upload.Options{Journal: j, Providers: func(name string) (provider.Provider, bool) { return target, name == "other" }, Hooks: e.fs.UploadHooks()})
	if err != nil {
		t.Fatal(err)
	}
	defer u.Stop()
	e.fs.SetWriteBackend(j, u)
	if _, err := u.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if target.Calls("BeginUpload") != 1 || target.Calls("UploadPart") != 0 {
		t.Fatalf("rapid copy: begin=%d parts=%d", target.Calls("BeginUpload"), target.Calls("UploadPart"))
	}
	got, err := e.fs.ReadFileRange(ctx, "/other/dest", 0, int64(len(body)))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("recovered copy: %q %v", got, err)
	}
}

func TestCopyStrictWaitsForRemoteIncludingEmptyFiles(t *testing.T) {
	for _, body := range [][]byte{nil, []byte("strict-copy")} {
		e := newEnv(t, envOpt{mode: config.ModeStrict})
		e.fake.Seed("source", body)
		a, err := e.fs.Copy(context.Background(), "/ali/source", "/ali/dest")
		if err != nil || a.Size != int64(len(body)) {
			t.Fatalf("strict copy: %+v %v", a, err)
		}
		rows, _, err := e.j.ListActive(context.Background(), "", 10)
		if err != nil || len(rows) != 0 || e.fake.Calls("BeginUpload") != 1 {
			t.Fatalf("strict returned before upload: %v %v", rows, err)
		}
		list, _, err := e.fake.List(context.Background(), fakeprovider.RootID, "")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, entry := range list {
			if entry.Name == "dest" && entry.Size == int64(len(body)) {
				found = true
			}
		}
		if !found {
			t.Fatal("destination absent on remote after strict copy")
		}
	}
}

func TestCopyReadsCommittedVersionWithoutFlushingWriter(t *testing.T) {
	e := newEnv(t, envOpt{})
	ctx := context.Background()
	e.fake.Seed("source", []byte("old"))
	a, err := e.fs.StatPath(ctx, "/ali/source")
	if err != nil {
		t.Fatal(err)
	}
	h, err := e.fs.Open(ctx, a.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	defer e.fs.Release(ctx, h)
	if _, err := e.fs.Write(ctx, h, []byte("new"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Copy(ctx, "/ali/source", "/ali/dest"); err != nil {
		t.Fatal(err)
	}
	got, err := e.fs.ReadFileRange(ctx, "/ali/dest", 0, 3)
	if err != nil || string(got) != "old" {
		t.Fatalf("snapshot=%q %v", got, err)
	}
	rows, _, err := e.j.ListActive(ctx, "", 10)
	if err != nil || len(rows) != 1 || rows[0].Name != "dest" {
		t.Fatalf("copy flushed source: %v %v", rows, err)
	}
}

func TestCopyRejectsReadonlyDirectoryAndInvalidPaths(t *testing.T) {
	e := newEnv(t, envOpt{mode: config.ModeReadonly})
	e.fake.Seed("source", []byte("old"))
	e.fs.SetWriteBackend(nil, nil)
	if _, err := e.fs.Copy(context.Background(), "/ali/source", "/ali/dest"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("readonly: %v", err)
	}
	if _, err := e.fs.Copy(context.Background(), "/ali", "/ali/dest"); !errors.Is(err, ErrIsDir) {
		t.Fatalf("directory: %v", err)
	}
	for _, bad := range []string{"source", "/ali/../source", "/ali/\\source", "/ali/\x00source"} {
		if _, err := e.fs.Copy(context.Background(), bad, "/ali/dest"); err == nil {
			t.Fatalf("accepted path %q", bad)
		}
	}
}
