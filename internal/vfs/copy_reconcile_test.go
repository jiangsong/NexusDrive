package vfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"cloudfs/internal/journal"
	"cloudfs/internal/provider"
	"cloudfs/test/fakeprovider"
)

// copyingFake adds a server-side copy to the fake backend, with control over
// the two things that make a copy's result unknowable: an answer that never
// arrives, and one that arrives after the object was already created.
type copyingFake struct {
	*fakeprovider.Fake
	// failAfterCopy makes the copy take effect and then report a failure, the
	// way a lost response does.
	failAfterCopy error
	// refuseBefore makes the copy fail without acting.
	refuseBefore error
	copies       int
}

func (c *copyingFake) Capabilities() provider.Caps {
	caps := c.Fake.Capabilities()
	caps.ServerCopy = true
	return caps
}

// Copy duplicates through the fake's own upload path, so the object it creates
// is indistinguishable from one the backend made itself.
func (c *copyingFake) Copy(ctx context.Context, id, newParentID, newName string) (provider.Entry, error) {
	if c.refuseBefore != nil {
		return provider.Entry{}, c.refuseBefore
	}
	src, err := c.Fake.Stat(ctx, id)
	if err != nil {
		return provider.Entry{}, err
	}
	body, err := c.Fake.ReadRange(ctx, id, src.Version, 0, src.Size)
	if err != nil {
		return provider.Entry{}, err
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return provider.Entry{}, err
	}
	session, err := c.Fake.BeginUpload(ctx, newParentID, newName, int64(len(data)), nil)
	if err != nil {
		return provider.Entry{}, err
	}
	token, err := c.Fake.UploadPart(ctx, session, 0, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return provider.Entry{}, err
	}
	entry, err := c.Fake.CompleteUpload(ctx, session, []provider.PartToken{token})
	if err != nil {
		return provider.Entry{}, err
	}
	c.copies++
	if c.failAfterCopy != nil {
		// The object exists; the caller is told the request failed.
		return provider.Entry{}, c.failAfterCopy
	}
	return entry, nil
}

func newCopyEnv(t *testing.T) (*env, *copyingFake) {
	t.Helper()
	var copier *copyingFake
	e := newEnv(t, envOpt{wrapProvider: func(f *fakeprovider.Fake) provider.Provider {
		copier = &copyingFake{Fake: f}
		return copier
	}})
	return e, copier
}

func seedCopySource(t *testing.T, e *env) {
	t.Helper()
	ctx := context.Background()
	e.fake.Seed("source.bin", []byte("copied bytes"))
	if _, err := e.fs.ReadDirPath(ctx, "/ali"); err != nil {
		t.Fatal(err)
	}
}

func outstanding(t *testing.T, e *env) []journal.ServerCopy {
	t.Helper()
	records, err := e.j.ServerCopies(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestServerCopyClearsItsIntentOnSuccess(t *testing.T) {
	e, copier := newCopyEnv(t)
	seedCopySource(t, e)
	ctx := context.Background()
	a, err := e.fs.Copy(ctx, "/ali/source.bin", "/ali/dest.bin")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "dest.bin" || a.Size != 12 {
		t.Fatalf("copy returned %+v", a)
	}
	if copier.copies != 1 {
		t.Fatalf("the provider copied %d times, want 1", copier.copies)
	}
	if records := outstanding(t, e); len(records) != 0 {
		t.Fatalf("a completed copy left %d intents behind: %+v", len(records), records)
	}
}

// TestALostAnswerLeavesAQuestionNotAGuess is the case the intent exists for:
// the provider created the object and the answer never came back. Reporting
// success would be a lie and reporting plain failure would abandon a real file
// on the account, so the copy is reported unresolved and the destination stays
// reserved.
func TestALostAnswerLeavesAQuestionNotAGuess(t *testing.T) {
	e, copier := newCopyEnv(t)
	seedCopySource(t, e)
	ctx := context.Background()
	copier.failAfterCopy = errors.New("connection reset by peer")

	_, err := e.fs.Copy(ctx, "/ali/source.bin", "/ali/dest.bin")
	if !errors.Is(err, ErrCopyUnresolved) {
		t.Fatalf("a lost answer returned %v, want ErrCopyUnresolved", err)
	}
	records := outstanding(t, e)
	if len(records) != 1 {
		t.Fatalf("the unresolved copy left %d intents, want 1", len(records))
	}
	if !strings.Contains(records[0].LastError, "connection reset") {
		t.Fatalf("the intent does not record why it is open: %q", records[0].LastError)
	}

	// A second attempt must not create a second object while the first is
	// unanswered — reconciliation runs first and finds the object that exists.
	copier.failAfterCopy = nil
	a, err := e.fs.Copy(ctx, "/ali/source.bin", "/ali/dest.bin")
	if !errors.Is(err, ErrExists) {
		t.Fatalf("retrying after reconciliation returned %+v, %v; the destination already holds the copy", a, err)
	}
	if copier.copies != 1 {
		t.Fatalf("the provider copied %d times; the retry must not duplicate the object", copier.copies)
	}
	if records := outstanding(t, e); len(records) != 0 {
		t.Fatalf("reconciliation left %d intents behind", len(records))
	}
	// The object the first attempt created is now in the tree.
	attr, err := e.fs.StatPath(ctx, "/ali/dest.bin")
	if err != nil || attr.Size != 12 {
		t.Fatalf("the copy that did happen was not adopted: %+v %v", attr, err)
	}
}

// TestReconciliationAdoptsAnObjectTheProcessNeverRecorded covers the crash
// window: the provider answered, the process died before the metadata write.
// A restart must find the file rather than leave it stranded on the account.
func TestReconciliationAdoptsAnObjectTheProcessNeverRecorded(t *testing.T) {
	e, copier := newCopyEnv(t)
	seedCopySource(t, e)
	ctx := context.Background()
	copier.failAfterCopy = errors.New("i/o timeout")
	if _, err := e.fs.Copy(ctx, "/ali/source.bin", "/ali/dest.bin"); !errors.Is(err, ErrCopyUnresolved) {
		t.Fatal(err)
	}
	if _, err := e.fs.StatPath(ctx, "/ali/dest.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatal("the destination is in the tree before reconciliation ran")
	}

	// This is what the daemon does at startup.
	if err := e.fs.ReconcileServerCopies(ctx); err != nil {
		t.Fatal(err)
	}
	attr, err := e.fs.StatPath(ctx, "/ali/dest.bin")
	if err != nil {
		t.Fatalf("reconciliation did not adopt the copied object: %v", err)
	}
	if attr.Size != 12 {
		t.Fatalf("adopted entry is %+v", attr)
	}
	if records := outstanding(t, e); len(records) != 0 {
		t.Fatalf("reconciliation left %d intents", len(records))
	}
}

// TestReconciliationReleasesADestinationNothingWasCreatedAt: when the request
// really did not take effect, the reservation must go, or the destination
// would stay unusable forever.
func TestReconciliationReleasesADestinationNothingWasCreatedAt(t *testing.T) {
	e, copier := newCopyEnv(t)
	seedCopySource(t, e)
	ctx := context.Background()
	// An intent is written, the provider is asked, and the answer is lost —
	// but this time nothing was created, which is only discoverable by looking.
	copier.refuseBefore = errors.New("connection reset by peer")
	if _, err := e.fs.Copy(ctx, "/ali/source.bin", "/ali/dest.bin"); !errors.Is(err, ErrCopyUnresolved) {
		t.Fatal(err)
	}
	if len(outstanding(t, e)) != 1 {
		t.Fatal("the failed copy did not leave an intent")
	}
	copier.refuseBefore = nil

	if err := e.fs.ReconcileServerCopies(ctx); err != nil {
		t.Fatal(err)
	}
	if records := outstanding(t, e); len(records) != 0 {
		t.Fatalf("reconciliation kept %d intents for a destination that does not exist", len(records))
	}
	if _, err := e.fs.StatPath(ctx, "/ali/dest.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatal("reconciliation invented a destination that was never created")
	}

	// With the reservation gone the copy can be made for real.
	if _, err := e.fs.Copy(ctx, "/ali/source.bin", "/ali/dest.bin"); err != nil {
		t.Fatalf("copying after a released reservation failed: %v", err)
	}
	if copier.copies != 1 {
		t.Fatalf("the provider copied %d times, want 1", copier.copies)
	}
}

// TestARefusalBeforeActingIsNotLeftUnresolved: errors that a provider raises
// without touching the account carry their own answer, and turning them into
// an open question would leave rows nobody can close.
func TestARefusalBeforeActingIsNotLeftUnresolved(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"not found", provider.ErrNotFound},
		{"already exists", provider.ErrExists},
		{"authentication", provider.ErrAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, copier := newCopyEnv(t)
			seedCopySource(t, e)
			copier.refuseBefore = tc.err
			_, err := e.fs.Copy(context.Background(), "/ali/source.bin", "/ali/dest.bin")
			if err == nil || errors.Is(err, ErrCopyUnresolved) {
				t.Fatalf("a definite refusal returned %v", err)
			}
			if records := outstanding(t, e); len(records) != 0 {
				t.Fatalf("a definite refusal left %d intents behind", len(records))
			}
		})
	}
}

// TestUnsupportedFallsBackToCopyingTheBytes: the one refusal that both proves
// nothing happened and means the fast path is unavailable.
func TestUnsupportedFallsBackToCopyingTheBytes(t *testing.T) {
	e, copier := newCopyEnv(t)
	seedCopySource(t, e)
	copier.refuseBefore = provider.ErrUnsupported
	a, err := e.fs.Copy(context.Background(), "/ali/source.bin", "/ali/dest.bin")
	if err != nil {
		t.Fatalf("copy did not fall back to the byte path: %v", err)
	}
	if a.Size != 12 {
		t.Fatalf("fallback copy produced %+v", a)
	}
	if records := outstanding(t, e); len(records) != 0 {
		t.Fatalf("the fallback left %d server-copy intents", len(records))
	}
}

// TestAnIntentFromAnotherStoreIsNotAdopted: a journal opened beside a
// different metadata store must not pull an object into a tree the intent was
// never about. The row is left alone — it is evidence, not garbage.
func TestAnIntentFromAnotherStoreIsNotAdopted(t *testing.T) {
	e, _ := newCopyEnv(t)
	seedCopySource(t, e)
	ctx := context.Background()
	if _, err := e.j.BeginServerCopy(ctx, journal.ServerCopySpec{
		MetaIdentity: "a-different-metadata-store", TargetMount: "/ali",
		TargetRootID: fakeprovider.RootID, SourceRemote: "ali", SourceID: "n1",
		TargetPath: "/ali/elsewhere.bin", TargetRemote: "ali",
		TargetParentID: fakeprovider.RootID, TargetParentIno: 1, TargetName: "elsewhere.bin",
	}); err != nil {
		t.Fatal(err)
	}
	err := e.fs.ReconcileServerCopies(ctx)
	if err == nil || !strings.Contains(err.Error(), "another metadata store") {
		t.Fatalf("reconciliation accepted a foreign intent: %v", err)
	}
	if records := outstanding(t, e); len(records) != 1 {
		t.Fatalf("a foreign intent was discarded rather than reported: %d rows", len(records))
	}
	if _, err := e.fs.StatPath(ctx, "/ali/elsewhere.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a foreign intent was adopted into this tree")
	}
}

func TestBeginServerCopyReservesOneDestination(t *testing.T) {
	e, _ := newCopyEnv(t)
	ctx := context.Background()
	spec := journal.ServerCopySpec{
		MetaIdentity: "store", TargetMount: "/ali", TargetRootID: fakeprovider.RootID,
		SourceRemote: "ali", SourceID: "n1", TargetPath: "/ali/x.bin", TargetRemote: "ali",
		TargetParentID: fakeprovider.RootID, TargetParentIno: 1, TargetName: "x.bin",
	}
	id, err := e.j.BeginServerCopy(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	// A second intent for the same destination would make neither answerable.
	if _, err := e.j.BeginServerCopy(ctx, spec); !errors.Is(err, journal.ErrServerCopyBusy) {
		t.Fatalf("a second intent for the same destination returned %v", err)
	}
	// A different destination is fine.
	other := spec
	other.TargetName, other.TargetPath = "y.bin", "/ali/y.bin"
	if _, err := e.j.BeginServerCopy(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := e.j.FinishServerCopy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.j.BeginServerCopy(ctx, spec); err != nil {
		t.Fatalf("the destination was not released: %v", err)
	}
}

func TestBeginServerCopyRejectsAnIncompleteIntent(t *testing.T) {
	e, _ := newCopyEnv(t)
	ctx := context.Background()
	full := journal.ServerCopySpec{
		MetaIdentity: "store", TargetMount: "/ali", TargetRootID: fakeprovider.RootID,
		SourceRemote: "ali", SourceID: "n1", TargetPath: "/ali/x.bin", TargetRemote: "ali",
		TargetParentID: fakeprovider.RootID, TargetParentIno: 1, TargetName: "x.bin",
	}
	cases := map[string]func(*journal.ServerCopySpec){
		"no metadata identity": func(s *journal.ServerCopySpec) { s.MetaIdentity = "" },
		"no destination":       func(s *journal.ServerCopySpec) { s.TargetParentID = "" },
		"no source":            func(s *journal.ServerCopySpec) { s.SourceID = "" },
		"no parent inode":      func(s *journal.ServerCopySpec) { s.TargetParentIno = 0 },
		"unsafe name":          func(s *journal.ServerCopySpec) { s.TargetName = "a/b" },
		"negative size":        func(s *journal.ServerCopySpec) { s.Size = -1 },
	}
	for name, mutate := range cases {
		spec := full
		mutate(&spec)
		if _, err := e.j.BeginServerCopy(ctx, spec); err == nil {
			t.Fatalf("an intent with %s was accepted", name)
		}
	}
}

// TestAnUnresolvedCopyIsReportedInTheWarning: an object that may exist on the
// account with nothing local pointing at it is exactly what an operator needs
// told. A failure that only reaches a log line is a failure nobody sees.
func TestAnUnresolvedCopyIsReportedInTheWarning(t *testing.T) {
	e, copier := newCopyEnv(t)
	seedCopySource(t, e)
	ctx := context.Background()
	copier.failAfterCopy = errors.New("i/o timeout")
	if _, err := e.fs.Copy(ctx, "/ali/source.bin", "/ali/dest.bin"); !errors.Is(err, ErrCopyUnresolved) {
		t.Fatal(err)
	}
	// The destination cannot be listed, so reconciliation cannot settle it.
	copier.refuseBefore = nil
	e.fake.SetFaults(func(f *fakeprovider.Faults) { f.FailNext = 100 })
	if err := e.fs.ReconcileServerCopies(ctx); err == nil {
		t.Fatal("reconciliation reported success while the destination was unreachable")
	}
	warning := e.fs.CopyWarning()
	if !strings.Contains(warning, "unresolved") || !strings.Contains(warning, "may exist on the account") {
		t.Fatalf("status warning does not describe the situation: %q", warning)
	}

	// Once the backend answers again the warning clears with the intent.
	e.fake.SetFaults(func(f *fakeprovider.Faults) { f.FailNext = 0 })
	if err := e.fs.ReconcileServerCopies(ctx); err != nil {
		t.Fatal(err)
	}
	if warning := e.fs.CopyWarning(); warning != "" {
		t.Fatalf("the warning outlived the problem: %q", warning)
	}
	if records := outstanding(t, e); len(records) != 0 {
		t.Fatalf("%d intents survived a successful reconciliation", len(records))
	}
}
