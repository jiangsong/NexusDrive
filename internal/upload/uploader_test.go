package upload

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/journal"
	"cloudfs/internal/net/retry"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

type fixture struct {
	j    *journal.Journal
	fake *fakeprovider.Fake
	up   *Uploader
	clk  *clock
	ok   []Result
	dead []error
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	j, err := journal.Open(journal.Options{Dir: t.TempDir(), Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	fake := fakeprovider.New("ali")
	f := &fixture{j: j, fake: fake, clk: c}
	u, err := New(Options{
		Journal: j,
		Providers: func(remote string) (provider.Provider, bool) {
			if remote == "ali" {
				return fake, true
			}
			return nil, false
		},
		MaxAttempts: 3,
		Policy:      retry.Policy{Backoff: retry.Backoff{Base: time.Millisecond, Max: time.Millisecond}, MaxAttempts: 2},
		Backoff:     retry.Backoff{Base: time.Millisecond, Max: time.Millisecond, Rand: func() float64 { return 1 }},
		Now:         c.now,
		Hooks: Hooks{
			OnSuccess: func(_ context.Context, _ journal.Upload, r Result) error {
				f.ok = append(f.ok, r)
				return nil
			},
			OnDead: func(_ context.Context, _ journal.Upload, cause error) { f.dead = append(f.dead, cause) },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.up = u
	return f
}

func (f *fixture) queue(t *testing.T, name string, content []byte, expectedVersion string) journal.Upload {
	t.Helper()
	return f.queueWithHashes(t, name, content, expectedVersion, []provider.HashType{provider.HashSHA1})
}

func (f *fixture) queueWithHashes(t *testing.T, name string, content []byte, expectedVersion string, hashes []provider.HashType) journal.Upload {
	t.Helper()
	s, err := f.j.NewStaging(hashes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteAt(content, 0); err != nil {
		t.Fatal(err)
	}
	h, _ := s.Hashes()
	blob, err := f.j.CommitStaging(s, h)
	if err != nil {
		t.Fatal(err)
	}
	u := journal.Upload{
		ID: journal.NewID(), StagingID: s.ID, Remote: "ali", RemoteParentID: fakeprovider.RootID,
		Name: name, BlobPath: blob, Size: int64(len(content)), Hashes: h,
		ExpectedVersion: expectedVersion,
	}
	if err := f.j.Commit(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestChunkedUploadRoundTrip(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	content := bytes.Repeat([]byte("ab"), 5<<20) // 10 MiB → 3 parts of 4 MiB
	u := f.queue(t, "big.bin", content, "")

	n, err := f.up.DrainAll(ctx)
	if err != nil || n != 1 {
		t.Fatalf("drain = %d, %v", n, err)
	}
	if len(f.ok) != 1 || f.ok[0].Rapid {
		t.Fatalf("result = %+v", f.ok)
	}
	got, _ := f.j.Get(ctx, u.ID)
	if got.State != journal.StateDone {
		t.Fatalf("state = %s (%s)", got.State, got.LastError)
	}
	// Verify the bytes landed.
	e := f.ok[0].Entry
	rc, err := f.fake.ReadRange(ctx, e.ID, e.Version, 0, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := make([]byte, len(content))
	if _, err := readFull(rc, buf); err != nil || !bytes.Equal(buf, content) {
		t.Fatal("uploaded content mismatch")
	}
	if f.fake.Calls("UploadPart") != 3 {
		t.Fatalf("expected 3 parts, got %d", f.fake.Calls("UploadPart"))
	}
}

func TestRapidUploadSkipsParts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	content := []byte("dedupe me")
	// Seed the same content so the provider knows the hash.
	f.fake.Seed("elsewhere/original.txt", content)
	before := f.fake.Calls("UploadPart")

	f.queue(t, "copy.txt", content, "")
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.ok) != 1 || !f.ok[0].Rapid {
		t.Fatalf("expected a rapid upload, got %+v", f.ok)
	}
	if f.fake.Calls("UploadPart") != before {
		t.Fatal("rapid upload must not send parts")
	}
}

func TestResumeReusesSessionAndSkipsUploadedParts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	content := bytes.Repeat([]byte("c"), 9<<20) // 3 parts of 4 MiB
	u := f.queue(t, "resume.bin", content, "")

	// Simulate a crash after the first part: open a real provider session,
	// upload part 0, persist both the session and the part, then requeue.
	sess, err := f.fake.BeginUpload(ctx, fakeprovider.RootID, "resume.bin", int64(len(content)), nil)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := f.fake.UploadPart(ctx, sess, 0, bytes.NewReader(content[:4<<20]), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.j.SetSession(ctx, u.ID, persistSession(sess)); err != nil {
		t.Fatal(err)
	}
	f.j.RecordPart(ctx, u.ID, journal.Part{Index: 0, ETag: pt.ETag, State: "done"})

	beginsBefore := f.fake.Calls("BeginUpload")
	partsBefore := f.fake.Calls("UploadPart")
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := f.j.Get(ctx, u.ID)
	if got.State != journal.StateDone {
		t.Fatalf("state = %s (%s)", got.State, got.LastError)
	}
	if f.fake.Calls("BeginUpload") != beginsBefore {
		t.Fatal("resume must reuse the persisted session, not start a new upload")
	}
	if sent := f.fake.Calls("UploadPart") - partsBefore; sent != 2 {
		t.Fatalf("resume sent %d parts, want 2 (part 0 was already uploaded)", sent)
	}
	if parts, _ := f.j.Parts(ctx, u.ID); len(parts) != 0 {
		t.Fatalf("parts should be cleared after success: %+v", parts)
	}
	// The reassembled file must match the original bytes.
	e := f.ok[0].Entry
	rc, err := f.fake.ReadRange(ctx, e.ID, e.Version, 0, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	buf := make([]byte, len(content))
	if _, err := readFull(rc, buf); err != nil || !bytes.Equal(buf, content) {
		t.Fatal("resumed upload produced different content")
	}
}

func TestStaleSessionRestartsUpload(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	content := bytes.Repeat([]byte("d"), 5<<20)
	u := f.queue(t, "stale.bin", content, "")

	// Persist a session id the provider has never heard of.
	f.j.SetSession(ctx, u.ID, map[string]string{sessionIDKey: "vanished", sessionPartSizeKey: "4194304"})
	f.j.RecordPart(ctx, u.ID, journal.Part{Index: 0, ETag: "bogus", State: "done"})

	// First pass detects the stale session, clears it and requeues.
	if _, err := f.up.DrainOnce(ctx, "ali"); err != nil {
		t.Fatal(err)
	}
	got, _ := f.j.Get(ctx, u.ID)
	if got.State != journal.StatePending {
		t.Fatalf("stale session should requeue, state = %s (%s)", got.State, got.LastError)
	}
	if got.Session[sessionIDKey] != "" {
		t.Fatalf("stale session should be cleared: %+v", got.Session)
	}
	// Second pass starts a fresh upload and succeeds.
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = f.j.Get(ctx, u.ID)
	if got.State != journal.StateDone {
		t.Fatalf("restart after stale session failed: %s (%s)", got.State, got.LastError)
	}
}

func TestRetryThenDeadLetter(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.queue(t, "flaky.txt", []byte("data"), "")
	// Every call fails transiently.
	f.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.FailNext = 1 << 30 })
	for i := 0; i < 5; i++ {
		f.clk.advance(time.Minute)
		if _, err := f.up.DrainOnce(ctx, "ali"); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := f.j.Get(ctx, u.ID)
	if got.State != journal.StateDead {
		t.Fatalf("state = %s after repeated failures", got.State)
	}
	if len(f.dead) == 0 {
		t.Fatal("OnDead hook not called")
	}
	// The blob is retained so the data is not lost.
	if _, err := os.Stat(u.BlobPath); err != nil {
		t.Fatalf("dead-lettered blob must survive: %v", err)
	}
	// Recovering: clear the fault and requeue.
	f.fake.SetFaults(func(ft *fakeprovider.Faults) { ft.FailNext = 0 })
	if err := f.j.Requeue(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	f.clk.advance(time.Minute)
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = f.j.Get(ctx, u.ID)
	if got.State != journal.StateDone {
		t.Fatalf("requeued upload should succeed, state = %s (%s)", got.State, got.LastError)
	}
}

func TestConflictCopy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// The file exists remotely at v1; our writer saw an older version.
	e := f.fake.Seed("notes.md", []byte("remote edit"))
	f.queue(t, "notes.md", []byte("local edit"), "stale-version")

	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.ok) != 1 {
		t.Fatalf("results = %+v", f.ok)
	}
	if f.ok[0].ConflictName == "" || !strings.HasPrefix(f.ok[0].ConflictName, "notes (conflict ") {
		t.Fatalf("expected a conflict copy, got %q", f.ok[0].ConflictName)
	}
	if !strings.HasSuffix(f.ok[0].ConflictName, ".md") {
		t.Fatalf("conflict name should keep the extension: %q", f.ok[0].ConflictName)
	}
	// The remote original is untouched.
	orig, err := f.fake.Stat(ctx, e.ID)
	if err != nil || orig.Version != e.Version {
		t.Fatalf("original was modified: %+v, %v", orig, err)
	}
}

func TestNoConflictWhenVersionMatches(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.fake.Seed("notes.md", []byte("remote"))
	f.queue(t, "notes.md", []byte("newer local"), e.Version)
	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	if f.ok[0].ConflictName != "" {
		t.Fatalf("unchanged remote must not produce a conflict copy: %q", f.ok[0].ConflictName)
	}
}

func TestUnknownRemoteDeadLetters(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s, _ := f.j.NewStaging(nil)
	s.WriteAt([]byte("x"), 0)
	h, _ := s.Hashes()
	blob, _ := f.j.CommitStaging(s, h)
	u := journal.Upload{ID: journal.NewID(), StagingID: s.ID, Remote: "ghost", RemoteParentID: "root", Name: "x", BlobPath: blob, Size: 1}
	f.j.Commit(ctx, u)

	if _, err := f.up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := f.j.Get(ctx, u.ID)
	if got.State != journal.StateDead || !strings.Contains(got.LastError, "no provider") {
		t.Fatalf("unknown remote = %s / %s", got.State, got.LastError)
	}
}

func TestAuthorizationFailureDeadLettersBeforeProviderResolution(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.queue(t, "fenced", []byte("retained"), "")
	resolved := 0
	up, err := New(Options{
		Journal: f.j,
		Providers: func(string) (provider.Provider, bool) {
			resolved++
			return f.fake, true
		},
		Hooks: Hooks{Authorize: func(context.Context, journal.Upload) error {
			return errors.New("binding mismatch")
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.DrainAll(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := f.j.Get(ctx, u.ID)
	if err != nil || got.State != journal.StateDead || got.LastError == "" {
		t.Fatalf("row: %+v %v", got, err)
	}
	if resolved != 0 || f.fake.TotalCalls() != 0 {
		t.Fatalf("authorization failure resolved=%d calls=%d", resolved, f.fake.TotalCalls())
	}
}

func TestConflictNameFormats(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	for in, wantPrefix := range map[string]string{
		"notes.md":       "notes (conflict 2026-09-02 ",
		"archive.tar.gz": "archive.tar (conflict 2026-09-02 ",
		"README":         "README (conflict 2026-09-02 ",
	} {
		got := conflictName(in, now)
		if !strings.HasPrefix(got, wantPrefix) {
			t.Errorf("conflictName(%q) = %q, want prefix %q", in, got, wantPrefix)
		}
	}
	if got := conflictName("notes.md", now); !strings.HasSuffix(got, ".md") {
		t.Errorf("extension lost: %q", got)
	}
}

func TestStartStopDrainsInBackground(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u := f.queue(t, "bg.txt", []byte("background"), "")
	f.up.opt.PollInterval = 5 * time.Millisecond
	f.up.Start(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := f.j.Get(ctx, u.ID)
		if got.State == journal.StateDone {
			f.up.Stop()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.up.Stop()
	got, _ := f.j.Get(ctx, u.ID)
	t.Fatalf("background uploader did not finish: state=%s err=%s", got.State, got.LastError)
}

func readFull(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			if total == len(buf) {
				return total, nil
			}
			return total, err
		}
		if n == 0 {
			break
		}
	}
	return total, nil
}

// putProvider adds single-request uploads to the fake, counting them.
type putProvider struct {
	*fakeprovider.Fake
	puts int
	max  int64
}

func (p *putProvider) Capabilities() provider.Caps {
	c := p.Fake.Capabilities()
	c.SinglePutMax = p.max
	return c
}

func (p *putProvider) PutFile(ctx context.Context, parentID, name string, r io.Reader, size int64, h provider.Hashes) (provider.Entry, error) {
	p.puts++
	// Reuse the fake's own upload path so the file lands like any other.
	s, err := p.Fake.BeginUpload(ctx, parentID, name, size, nil)
	if err != nil {
		return provider.Entry{}, err
	}
	pt, err := p.Fake.UploadPart(ctx, s, 0, r, size)
	if err != nil {
		return provider.Entry{}, err
	}
	return p.Fake.CompleteUpload(ctx, s, []provider.PartToken{pt})
}

// TestSmallFilesGoUpInOneRequest: a file under the backend's single-put
// limit costs one call, not begin + part + complete; a larger one still
// takes the resumable session.
func TestSmallFilesGoUpInOneRequest(t *testing.T) {
	f := newFixture(t)
	pp := &putProvider{Fake: f.fake, max: 64}
	f.up.opt.Providers = func(remote string) (provider.Provider, bool) { return pp, remote == "ali" }
	ctx := context.Background()

	small := f.queue(t, "small.txt", []byte("tiny"), "")
	big := f.queue(t, "big.bin", bytes.Repeat([]byte{1}, 200), "")
	if _, err := f.up.DrainOnce(ctx, "ali"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.up.DrainOnce(ctx, "ali"); err != nil {
		t.Fatal(err)
	}
	if pp.puts != 1 {
		t.Fatalf("single-request puts = %d, want 1", pp.puts)
	}
	// PutFile above goes through the fake's session path itself, so
	// subtract those: only the large file should have opened a session.
	if sessions := f.fake.Calls("BeginUpload") - pp.puts; sessions != 1 {
		t.Fatalf("the large file should have opened exactly one session, got %d", sessions)
	}
	for _, u := range []journal.Upload{small, big} {
		got, err := f.j.Get(ctx, u.ID)
		if err != nil || got.State != journal.StateDone {
			t.Fatalf("%s state = %v, %v", u.Name, got.State, err)
		}
	}
	entries, _, err := f.fake.List(ctx, fakeprovider.RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	landed := map[string]bool{}
	for _, e := range entries {
		landed[e.Name] = true
	}
	for _, name := range []string{"small.txt", "big.bin"} {
		if !landed[name] {
			t.Fatalf("%s did not land; listing has %v", name, landed)
		}
	}
}

// TestTombstonedUploadIsDeletedAfterLanding: a file deleted while its upload
// was in flight must not exist on the backend once the transfer completes.
func TestTombstonedUploadIsDeletedAfterLanding(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.queue(t, "gone.txt", []byte("was deleted mid-flight"), "")
	// Simulate the race: the row is claimed (in flight) and then the local
	// file is removed, which tombstones the row instead of dropping it.
	claimed, err := f.j.Claim(ctx, "ali", 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if err := f.j.Tombstone(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	f.up.process(ctx, claimed[0])

	entries, _, _ := f.fake.List(ctx, fakeprovider.RootID, "")
	for _, e := range entries {
		if e.Name == "gone.txt" {
			t.Fatal("a tombstoned upload resurrected the file on the backend")
		}
	}
	if _, err := f.j.Get(ctx, u.ID); !errors.Is(err, journal.ErrNotFound) {
		t.Fatalf("tombstoned row should be gone, got %v", err)
	}
	if len(f.ok) != 0 {
		t.Fatal("a tombstoned upload must not be reported as a success")
	}
}

// TestOwnEarlierUploadIsNotAConflict: the remote version changing because we
// uploaded the previous save of the same file is not someone else's edit.
func TestOwnEarlierUploadIsNotAConflict(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.queue(t, "doc.txt", []byte("first save"), "")
	if _, err := f.up.DrainOnce(ctx, "ali"); err != nil {
		t.Fatal(err)
	}
	if len(f.ok) != 1 {
		t.Fatalf("first upload did not land: %+v", f.ok)
	}
	landed := f.ok[0].Entry.Version
	// The second save was opened before the first upload finished, so its
	// expected version is stale; the hook knows what we last put there.
	f.up.opt.Hooks.RemoteVersion = func(context.Context, journal.Upload) (string, bool) { return landed, true }
	second := f.queue(t, "doc.txt", []byte("second save"), "stale-version-from-open-time")
	if _, err := f.up.DrainOnce(ctx, "ali"); err != nil {
		t.Fatal(err)
	}
	if len(f.ok) != 2 || f.ok[1].ConflictName != "" {
		t.Fatalf("second upload produced a conflict copy: %+v", f.ok)
	}
	entries, _, _ := f.fake.List(ctx, fakeprovider.RootID, "")
	for _, e := range entries {
		if strings.Contains(e.Name, "conflict") {
			t.Fatalf("conflict copy on the backend: %s", e.Name)
		}
	}
	_ = first
	_ = second
}

// TestUploadOfADeletedFileIsNotDeadLettered: a file created and removed again
// before its upload ran has nothing left to send. Failing it asks an operator
// to look at a file that no longer exists, and leaves the dead-letter queue
// dirty for every drain measurement after it.
func TestUploadOfADeletedFileIsNotDeadLettered(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	gone := false
	f.up.opt.Hooks.Exists = func(context.Context, journal.Upload) bool { return !gone }

	u := f.queue(t, "f.dat", []byte("x"), "")
	u.RemoteParentID = "missing-dir"
	u.Ino = 7
	if err := f.j.Drop(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.j.Commit(ctx, u); err != nil {
		t.Fatal(err)
	}
	gone = true
	if _, err := f.up.DrainOnce(ctx, "ali"); err != nil {
		t.Fatal(err)
	}
	st, err := f.j.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Dead != 0 {
		t.Fatalf("dead-lettered an upload whose file was deleted locally: %+v", st)
	}
	if len(f.dead) != 0 {
		t.Fatalf("OnDead fired for a deleted file: %v", f.dead)
	}
}
