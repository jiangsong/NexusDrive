package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cloudfs/internal/journal"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

// TestConflictCheckPagesTheDirectory: the conflict check must find the file
// wherever it is in the directory. Reading only the first page made every
// file past it look unchanged, so a rewrite of such a file silently
// overwrote someone else's edit instead of landing beside it.
func TestConflictCheckPagesTheDirectory(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// The fake pages by ListPageSize; fill more than one page of names that
	// sort before the target so it lands on a later page.
	for i := 0; i < fakeprovider.ListPageSize*2; i++ {
		f.fake.Seed(fmt.Sprintf("a%02d.txt", i), []byte("filler"))
	}
	f.fake.Seed("notes.md", []byte("remote edit"))
	f.queue(t, "notes.md", []byte("local edit"), "stale-version")
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.ok) != 1 || f.ok[0].ConflictName == "" {
		t.Fatalf("a changed file on the second page was overwritten: %+v", f.ok)
	}
	if got, _ := f.fake.Content("/notes.md"); string(got) != "remote edit" {
		t.Fatalf("remote edit lost: %q", got)
	}
}

// linkingProvider wraps the fake and, like a storage pool, keeps the upload's
// bytes through the link the uploader offers.
type linkingProvider struct {
	*fakeprovider.Fake
	mu     sync.Mutex
	linked []string
	dir    string
	fail   error
}

func (l *linkingProvider) BeginUpload(ctx context.Context, parentID, name string, size int64, h provider.Hashes) (provider.UploadSession, error) {
	if l.fail != nil {
		return provider.UploadSession{}, l.fail
	}
	if link := provider.UploadBlobLinkFrom(ctx); link != nil {
		dst := filepath.Join(l.dir, name+".hold")
		if err := link(dst); err != nil {
			return provider.UploadSession{}, fmt.Errorf("link: %w", err)
		}
		l.mu.Lock()
		l.linked = append(l.linked, dst)
		l.mu.Unlock()
	}
	return l.Fake.BeginUpload(ctx, parentID, name, size, h)
}

func newLinkingFixture(t *testing.T) (*fixture, *linkingProvider) {
	t.Helper()
	f := newFixture(t)
	lp := &linkingProvider{Fake: f.fake, dir: t.TempDir()}
	f.up.opt.Providers = func(remote string) (provider.Provider, bool) { return lp, remote == "ali" }
	return f, lp
}

// TestBlobLinkIsOfferedToProvider: a backend that links the blob keeps the
// bytes after the queue has released its own copy, without a second read
// of the content.
func TestBlobLinkIsOfferedToProvider(t *testing.T) {
	f, lp := newLinkingFixture(t)
	ctx := context.Background()
	u := f.queue(t, "keep.txt", []byte("kept bytes"), "")
	// Not a rapid upload: the fake has never seen these bytes.
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.j.Get(ctx, u.ID); got.State != journal.StateDone {
		t.Fatalf("state = %s (%s)", got.State, got.LastError)
	}
	if len(lp.linked) != 1 {
		t.Fatalf("linked = %v", lp.linked)
	}
	data, err := os.ReadFile(lp.linked[0])
	if err != nil || string(data) != "kept bytes" {
		t.Fatalf("held copy = %q, %v", data, err)
	}
	// The queue released its own link; the held copy is what remains.
	if _, err := os.Stat(u.BlobPath); err == nil {
		t.Fatalf("the queue still holds %s after success", u.BlobPath)
	}
}

// TestUnavailableDefersWithoutSpendingAttempts: a backend that cannot reach
// anything holding the destination is waited for, not retried into a dead
// letter. The attempt budget is for failures of the upload, not absences of
// the backend.
func TestUnavailableDefersWithoutSpendingAttempts(t *testing.T) {
	f, lp := newLinkingFixture(t)
	ctx := context.Background()
	u := f.queue(t, "later.txt", []byte("when the drives are back"), "")
	lp.fail = fmt.Errorf("pool: %w", provider.ErrUnavailable)
	for i := 0; i < 6; i++ {
		f.clk.advance(time.Minute)
		if _, err := f.up.DrainOnce(ctx, "ali"); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := f.j.Get(ctx, u.ID)
	if got.State == journal.StateDead || len(f.dead) != 0 {
		t.Fatalf("dead-lettered while the backend was merely unavailable: %s %v", got.State, f.dead)
	}
	if got.Attempt != 0 {
		t.Fatalf("attempts spent on an unavailable backend: %d", got.Attempt)
	}
	if !errors.Is(lp.fail, provider.ErrUnavailable) {
		t.Fatal("test wiring")
	}
	lp.fail = nil
	f.clk.advance(time.Minute)
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.j.Get(ctx, u.ID); got.State != journal.StateDone {
		t.Fatalf("state after the backend returned = %s (%s)", got.State, got.LastError)
	}
	if data, _ := f.fake.Content("/later.txt"); string(data) != "when the drives are back" {
		t.Fatalf("content = %q", data)
	}
}

var _ io.Reader = (*os.File)(nil)
