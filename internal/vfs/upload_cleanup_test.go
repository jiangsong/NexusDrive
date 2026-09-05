package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"cloudfs/internal/cache"
	"cloudfs/internal/config"
	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
	"cloudfs/internal/provider"
)

func uploadCleanupAlias(t *testing.T, e *env, u journal.Upload, mode config.Mode) meta.Node {
	t.Helper()
	m := e.fs.mounts[0]
	m.Prefix, m.Mode = "/alias", mode
	e.fs.mounts = append(e.fs.mounts, m)
	parent, err := e.store.Insert(t.Context(), meta.Node{ParentIno: meta.RootIno, Name: "alias", Kind: provider.KindDir, Remote: m.Remote, RemoteID: m.RootID})
	if err != nil {
		t.Fatal(err)
	}
	n, err := e.store.Get(t.Context(), u.Ino)
	if err != nil {
		t.Fatal(err)
	}
	n.Ino, n.ParentIno = 0, parent.Ino
	n, err = e.store.Insert(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDiscardUploadRemovesAliasesAndKeepsSharedPayloadAndPinPolicy(t *testing.T) {
	e, u := stoppedUpload(t)
	ctx := t.Context()
	alias := uploadCleanupAlias(t, e, u, config.ModeWriteback)
	if err := e.fs.Pin(ctx, "/ali/resume.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.WriteFile(ctx, "/ali/shared.txt", []byte("retained"), false); err != nil {
		t.Fatal(err)
	}
	before := e.fake.TotalCalls()
	changes, unwatch := e.fs.WatchChanges()
	defer unwatch()
	if err := e.fs.DiscardUpload(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, ino := range []uint64{u.Ino, alias.Ino} {
		if _, err := e.store.Get(ctx, ino); !errors.Is(err, meta.ErrNotFound) {
			t.Fatalf("alias not removed: %v", err)
		}
	}
	if _, err := e.j.Get(ctx, u.ID); !errors.Is(err, journal.ErrNotFound) {
		t.Fatal(err)
	}
	if err := e.j.Commit(ctx, u); !errors.Is(err, journal.ErrUploadPurging) {
		t.Fatalf("discarded ID revived: %v", err)
	}
	if b, err := os.ReadFile(u.BlobPath); err != nil || string(b) != "retained" {
		t.Fatalf("other queue owner lost payload: %q %v", b, err)
	}
	key := cache.FileKey{Remote: u.Remote, RemoteID: localRemoteID(u.ID), Version: localVersion(u.ID)}
	if lease, err := e.cache.OpenWhole(key); err == nil {
		lease.Close()
		t.Fatal("discarded cache still published")
	}
	if len(e.fs.PinPolicies()) != 1 {
		t.Fatal("discard deleted user's persistent path rule")
	}
	if e.fake.TotalCalls() != before {
		t.Fatal("local discard made provider calls")
	}
	seen := map[string]bool{}
	for len(changes) > 0 {
		for _, p := range (<-changes).Paths {
			seen[p] = true
		}
	}
	if !seen["/ali/resume.txt"] || !seen["/alias/resume.txt"] {
		t.Fatalf("missing alias invalidation: %+v", seen)
	}
	if b, err := e.fs.ReadFileRange(ctx, "/ali/shared.txt", 0, 20); err != nil || string(b) != "retained" {
		t.Fatalf("shared upload unreadable: %q %v", b, err)
	}
}

func TestDiscardUploadRefusesUnsafeAdmissionBeforeIntent(t *testing.T) {
	for _, kind := range []string{"confirmation", "read", "write", "alias-reader", "readonly-alias", "readonly", "root", "replacement", "pending", "cancelling"} {
		t.Run(kind, func(t *testing.T) {
			e, u := stoppedUpload(t)
			ctx := t.Context()
			confirm := true
			want := ErrUploadCleanupTarget
			switch kind {
			case "confirmation":
				confirm = false
				want = nil
			case "read", "write", "alias-reader":
				ino := u.Ino
				if kind == "alias-reader" {
					ino = uploadCleanupAlias(t, e, u, config.ModeWriteback).Ino
				}
				h, err := e.fs.Open(ctx, ino, kind == "write")
				if err != nil {
					t.Fatal(err)
				}
				defer e.fs.Release(ctx, h)
				want = ErrUploadCleanupBusy
			case "readonly-alias":
				uploadCleanupAlias(t, e, u, config.ModeReadonly)
				want = ErrReadOnly
			case "readonly":
				e.fs.mounts[0].Mode = config.ModeReadonly
				want = ErrReadOnly
			case "root":
				e.fs.mounts[0].RootID = "different-root"
			case "replacement":
				if _, err := e.fs.WriteFile(ctx, "/ali/resume.txt", []byte("new version"), false); err != nil {
					t.Fatal(err)
				}
			case "pending", "cancelling":
				if err := e.fs.ResumeUpload(ctx, u.ID, true); err != nil {
					t.Fatal(err)
				}
				if kind == "cancelling" {
					if rows, err := e.j.Claim(ctx, u.Remote, 1); err != nil || len(rows) != 1 {
						t.Fatalf("claim: %+v %v", rows, err)
					}
					if _, err := e.j.RequestCancel(ctx, u.ID); err != nil {
						t.Fatal(err)
					}
				}
				want = journal.ErrUploadCleanupState
			}
			if err := e.fs.DiscardUpload(ctx, u.ID, confirm); err == nil || want != nil && !errors.Is(err, want) {
				t.Fatalf("unsafe admission: %v, want %v", err, want)
			}
			if _, err := e.j.UploadCleanupIdentity(ctx, u.ID); !errors.Is(err, journal.ErrNotFound) {
				t.Fatalf("refusal left cleanup intent: %v", err)
			}
			if _, err := os.Stat(u.BlobPath); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDiscardUploadRetriesEveryDurableBoundaryWithoutRepublishing(t *testing.T) {
	for _, phase := range []string{"intent", "metadata", "cache", "sql", "cache-unlink"} {
		t.Run(phase, func(t *testing.T) {
			e, u := stoppedUpload(t)
			ctx := t.Context()
			fault := errors.New("injected cleanup interruption")
			cacheDir := filepath.Join(filepath.Dir(filepath.Dir(e.j.StagingDir())), "cache", "hydrated")
			if phase == "sql" {
				if _, err := e.store.DB().Exec(`CREATE TRIGGER fail_cleanup BEFORE DELETE ON nodes BEGIN SELECT RAISE(ABORT,'cleanup failure'); END`); err != nil {
					t.Fatal(err)
				}
			} else {
				e.fs.uploadCleanupFault = func(at string) error {
					if at == phase {
						return fault
					}
					if phase == "cache-unlink" && at == "metadata" {
						if err := os.Rename(cacheDir, cacheDir+".saved"); err != nil {
							return err
						}
						return os.WriteFile(cacheDir, []byte("not a directory"), 0600)
					}
					return nil
				}
			}
			if err := e.fs.DiscardUpload(ctx, u.ID, true); err == nil {
				t.Fatal("fault ignored")
			}
			if row, err := e.j.Get(ctx, u.ID); err != nil || row.State != journal.StatePurging {
				t.Fatalf("intent lost: %+v %v", row, err)
			}
			if b, err := os.ReadFile(u.BlobPath); err != nil || string(b) != "retained" {
				t.Fatalf("deleted durable payload too early: %q %v", b, err)
			}
			if phase == "sql" {
				if _, err := e.store.DB().Exec(`DROP TRIGGER fail_cleanup`); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "cache-unlink" {
				if err := os.Remove(cacheDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(cacheDir+".saved", cacheDir); err != nil {
					t.Fatal(err)
				}
			}
			e.fs.uploadCleanupFault = nil
			if _, err := e.j.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if err := e.fs.RecoverUploadCleanups(ctx); err != nil {
				t.Fatal(err)
			}
			if err := e.fs.RecoverPublications(ctx, e.j); err != nil {
				t.Fatal(err)
			}
			if _, err := e.store.Get(ctx, u.Ino); !errors.Is(err, meta.ErrNotFound) {
				t.Fatalf("cleanup republished local version: %v", err)
			}
			if _, err := os.Stat(u.BlobPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("payload not removed after retry: %v", err)
			}
		})
	}
}

func TestDiscardUploadKeepsClosingReadRegisteredUntilItFinishes(t *testing.T) {
	e, u := stoppedUpload(t)
	ctx := t.Context()
	h, err := e.fs.Open(ctx, u.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	entered, proceed := make(chan struct{}), make(chan struct{})
	e.fs.readStartedFault = func() { close(entered); <-proceed }
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 8)
		n, err := e.fs.Read(ctx, h, buf, 0)
		if err == nil && string(buf[:n]) != "retained" {
			err = errors.New("read lost retained contents")
		}
		done <- err
	}()
	<-entered
	if err := e.fs.Release(ctx, h); err != nil {
		t.Fatal(err)
	}
	cleanupErr := e.fs.DiscardUpload(ctx, u.ID, true)
	close(proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.fs.readStartedFault = nil
	if !errors.Is(cleanupErr, ErrUploadCleanupBusy) {
		t.Fatalf("closing reader lost protection: %v", cleanupErr)
	}
	if _, err := e.fs.Read(ctx, h, make([]byte, 8), 0); !errors.Is(err, syscall.EBADF) {
		t.Fatalf("closed handle admitted a new read: %v", err)
	}
	if err := e.fs.DiscardUpload(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
}

func TestDiscardUploadAndReadOpenHaveOneWinner(t *testing.T) {
	for range 30 {
		e, u := stoppedUpload(t)
		ctx := t.Context()
		start := make(chan struct{})
		type opened struct {
			h   *Handle
			err error
		}
		reader, cleaner := make(chan opened, 1), make(chan error, 1)
		go func() { <-start; h, err := e.fs.Open(ctx, u.Ino, false); reader <- opened{h, err} }()
		go func() { <-start; cleaner <- e.fs.DiscardUpload(ctx, u.ID, true) }()
		close(start)
		r, err := <-reader, <-cleaner
		if r.err == nil {
			if !errors.Is(err, ErrUploadCleanupBusy) {
				t.Fatalf("cleanup raced read admission: %v", err)
			}
			if err := e.fs.Release(ctx, r.h); err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(r.err, ErrNotFound) || err != nil {
			t.Fatalf("unexpected winner: open=%v cleanup=%v", r.err, err)
		}
	}
}

func TestDiscardUploadRecoveryRejectsDifferentMetadata(t *testing.T) {
	e, u := stoppedUpload(t)
	ctx := t.Context()
	e.fs.uploadCleanupFault = func(string) error { return errors.New("interrupted") }
	if err := e.fs.DiscardUpload(ctx, u.ID, true); err == nil {
		t.Fatal("fault not injected")
	}
	other, err := meta.Open(filepath.Join(t.TempDir(), "new.db"), meta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	e.fs.meta = other
	e.fs.uploadCleanupFault = nil
	if err := e.fs.RecoverUploadCleanups(ctx); !errors.Is(err, journal.ErrUploadCleanupIdentity) {
		t.Fatalf("accepted new metadata: %v", err)
	}
	if b, err := os.ReadFile(u.BlobPath); err != nil || string(b) != "retained" {
		t.Fatalf("wrong database discarded payload: %q %v", b, err)
	}
}

func TestDiscardUploadWaitsForClosingWriterCommit(t *testing.T) {
	e, u := stoppedUpload(t)
	ctx := t.Context()
	h, err := e.fs.Open(ctx, u.Ino, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Write(ctx, h, []byte("new"), 0); err != nil {
		t.Fatal(err)
	}
	entered, proceed := make(chan struct{}), make(chan struct{})
	e.fs.commitFault = func() error { close(entered); <-proceed; return errors.New("commit interrupted") }
	done := make(chan error, 1)
	go func() { done <- e.fs.Release(ctx, h) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("writer never entered commit")
	}
	err = e.fs.DiscardUpload(ctx, u.ID, true)
	close(proceed)
	if commitErr := <-done; commitErr == nil {
		t.Fatal("commit fault not injected")
	}
	if !errors.Is(err, ErrUploadCleanupBusy) {
		t.Fatalf("closing writer lost protection: %v", err)
	}
}

func TestDiscardUploadKeepsExistingCacheLeaseReadable(t *testing.T) {
	e, u := stoppedUpload(t)
	ctx := t.Context()
	key := cache.FileKey{Remote: u.Remote, RemoteID: localRemoteID(u.ID), Version: localVersion(u.ID)}
	lease, err := e.cache.OpenWhole(key)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := e.fs.DiscardUpload(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, u.Size)
	if n, err := lease.ReadAt(buf, 0); err != nil || string(buf[:n]) != "retained" {
		t.Fatalf("existing lease lost bytes: %q %v", buf[:n], err)
	}
	if _, err := os.Stat(u.BlobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal name not removed: %v", err)
	}
	if e.cache.Stats().WholeBytes == 0 {
		t.Fatal("live detached lease lost accounting")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if e.cache.Stats().WholeBytes != 0 {
		t.Fatal("closed detached lease retained charge")
	}
}

func TestDiscardSubmittedCopyUploadKeepsSourceAndPreparationHistory(t *testing.T) {
	e, _ := stoppedUpload(t)
	ctx := t.Context()
	if _, err := e.fs.Copy(ctx, "/ali/resume.txt", "/ali/copy.txt"); err != nil {
		t.Fatal(err)
	}
	jobs, err := e.j.CopyJobs(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].State != journal.CopySubmitted {
		t.Fatalf("copy jobs: %+v %v", jobs, err)
	}
	id := jobs[0].ID
	if _, err := e.up.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	before := e.fake.TotalCalls()
	if err := e.fs.DiscardUpload(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if job, err := e.j.GetCopy(ctx, id); err != nil || job.State != journal.CopySubmitted {
		t.Fatalf("discard changed preparation history: %+v %v", job, err)
	}
	if b, err := e.fs.ReadFileRange(ctx, "/ali/resume.txt", 0, 20); err != nil || string(b) != "retained" {
		t.Fatalf("source lost: %q %v", b, err)
	}
	if e.fake.TotalCalls() != before {
		t.Fatal("discard touched remote")
	}
	if err := e.fs.ForgetCopy(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestDiscardUploadCannotBypassPathBasedMCPRead(t *testing.T) {
	e, u := stoppedUpload(t)
	ctx := t.Context()
	entered, proceed := make(chan struct{}), make(chan struct{})
	e.fs.readStartedFault = func() { close(entered); <-proceed }
	done := make(chan error, 1)
	go func() {
		b, err := e.fs.ReadFileRange(ctx, "/ali/resume.txt", 0, 20)
		if err == nil && string(b) != "retained" {
			err = errors.New("path read lost contents")
		}
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("path read bypassed tracked Read: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("path read did not start")
	}
	err := e.fs.DiscardUpload(ctx, u.ID, true)
	close(proceed)
	if readErr := <-done; readErr != nil {
		t.Fatal(readErr)
	}
	if !errors.Is(err, ErrUploadCleanupBusy) {
		t.Fatalf("discard bypassed path reader: %v", err)
	}
	e.fs.readStartedFault = nil
	if err := e.fs.DiscardUpload(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
}
