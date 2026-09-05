package mcpsrv

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/journal"
	"cloudfs/internal/provider"
	"cloudfs/internal/vfs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func seedCopyJob(t *testing.T, e *env, source, target string, change func(*journal.CopySpec)) string {
	t.Helper()
	ctx := context.Background()
	identity, err := e.fs.Meta().Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.fake.Seed("work/source", []byte("source"))
	if _, err := e.fs.StatPath(ctx, "/work"); err != nil {
		t.Fatal(err)
	}
	parent, err := e.fs.Meta().Resolve(ctx, "/work")
	if err != nil {
		t.Fatal(err)
	}
	mount := e.fs.Mounts()[0]
	spec := journal.CopySpec{MetaIdentity: identity, TargetParentIno: parent.Ino,
		SourceMount: mount.Prefix, SourceRootID: mount.RootID, SourceAccountBinding: mount.AccountBinding,
		TargetMount: mount.Prefix, TargetRootID: mount.RootID, TargetAccountBinding: mount.AccountBinding,
		SourcePath: source, TargetPath: target, SourceRemote: mount.Remote, TargetRemote: mount.Remote,
		SourceID: "PRIVATE_BACKEND_ID", SourceVersion: "PRIVATE_VERSION", TargetParentID: parent.RemoteID, Mode: "writeback", Size: 3}
	if change != nil {
		change(&spec)
	}
	c, err := e.j.BeginCopy(ctx, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := c.Job().ID
	if _, err := c.Write([]byte("a")); err != nil {
		c.Close()
		t.Fatal(err)
	}
	if err := c.Checkpoint(ctx); err != nil {
		c.Close()
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCopyJobToolsManagePreparationWithoutLeakingRecoveryData(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	ctx := context.Background()
	id := seedCopyJob(t, e, "/work/source", "/work/target", nil)
	if err := e.j.FailCopy(ctx, id, "PRIVATE_BACKEND_ID PRIVATE_VERSION /private/cache/payload token=SECRET"); err != nil {
		t.Fatal(err)
	}
	before := e.fake.TotalCalls()
	var info vfs.CopyInfo
	res := e.call(t, "get_copy_job", copyJobInput{ID: id}, &info)
	if res.IsError || info.ID != id || info.State != journal.CopyFailed || info.Checkpoint != 1 || info.Size != 3 {
		t.Fatalf("inspect: %+v %s", info, errText(res))
	}
	encoded, _ := json.Marshal(res)
	for _, secret := range []string{"PRIVATE_BACKEND_ID", "PRIVATE_VERSION", "/private/cache", "SECRET"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("copy inspection leaked %q", secret)
		}
	}
	for _, action := range []string{"retry", "cancel"} {
		var out copyMutationOutput
		res := e.call(t, action+"_copy_job", copyJobInput{ID: id}, &out)
		if res.IsError || !out.Accepted || out.ID != id || out.Action != action {
			t.Fatalf("%s: %+v %s", action, out, errText(res))
		}
	}
	job, err := e.j.GetCopy(ctx, id)
	if err != nil || job.State != journal.CopyCancelled || job.Checkpoint != 1 {
		t.Fatalf("cancelled job: %+v %v", job, err)
	}
	if res := e.call(t, "forget_copy_job", forgetCopyJobInput{ID: id}, nil); !res.IsError {
		t.Fatal("cleanup did not require confirmation")
	}
	if _, err := e.j.GetCopy(ctx, id); err != nil {
		t.Fatal("unconfirmed cleanup removed job")
	}
	if res := e.call(t, "forget_copy_job", forgetCopyJobInput{ID: id, Confirm: true}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	if _, err := e.j.GetCopy(ctx, id); !errors.Is(err, journal.ErrNotFound) {
		t.Fatalf("forgotten job still exists: %v", err)
	}
	if e.fake.TotalCalls() != before {
		t.Fatal("local copy management contacted provider")
	}
}

func TestCopyJobToolsRequireBothPathsAndCurrentBindings(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	visible := seedCopyJob(t, e, "/work/source", "/work/visible", nil)
	hidden := []string{
		seedCopyJob(t, e, "/secret/source", "/work/from-private", nil),
		seedCopyJob(t, e, "/work/source", "/secret/to-private", nil),
		seedCopyJob(t, e, "/work/source", "/work/old-root", func(s *journal.CopySpec) { s.SourceRootID = "old-root" }),
		seedCopyJob(t, e, "/work/source", "/work/old-db", func(s *journal.CopySpec) { s.MetaIdentity = "another-db" }),
		seedCopyJob(t, e, "/work/source", "/work/old-target", func(s *journal.CopySpec) { s.TargetRemote = "another-account" }),
		seedCopyJob(t, e, "/work/source", "/work/old-account", func(s *journal.CopySpec) { s.SourceAccountBinding = "another-generation" }),
	}
	before := e.fake.TotalCalls()
	var page copyJobsOutput
	if res := e.call(t, "list_copy_jobs", copyJobsInput{}, &page); res.IsError || len(page.Jobs) != 1 || page.Jobs[0].ID != visible {
		t.Fatalf("filtered page: %+v %s", page, errText(res))
	}
	for _, id := range append(hidden, journal.NewID()) {
		for _, tool := range []string{"get_copy_job", "retry_copy_job", "cancel_copy_job", "forget_copy_job"} {
			var in any = copyJobInput{ID: id}
			if tool == "forget_copy_job" {
				in = forgetCopyJobInput{ID: id, Confirm: true}
			}
			res := e.call(t, tool, in, nil)
			if !res.IsError || errText(res) != "copy job not found or inaccessible" {
				t.Fatalf("%s leaked existence or accepted hidden job: %s", tool, errText(res))
			}
		}
	}
	for _, id := range hidden {
		job, err := e.j.GetCopy(context.Background(), id)
		if err != nil || job.State != journal.CopyPreparing {
			t.Fatalf("hidden job was mutated: %+v %v", job, err)
		}
	}
	if e.fake.TotalCalls() != before {
		t.Fatal("permission filtering contacted provider")
	}
}

func TestCopyJobToolsReadOnlyAndReferencedContent(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}, ReadOnly: true})
	id := seedCopyJob(t, e, "/work/source", "/work/target", nil)
	for _, tool := range []string{"retry_copy_job", "cancel_copy_job", "forget_copy_job"} {
		var in any = copyJobInput{ID: id}
		if tool == "forget_copy_job" {
			in = forgetCopyJobInput{ID: id, Confirm: true}
		}
		if res := e.call(t, tool, in, nil); !res.IsError || !strings.Contains(errText(res), "read-only") {
			t.Fatalf("read-only %s: %s", tool, errText(res))
		}
	}
	if res := e.call(t, "get_copy_job", copyJobInput{ID: id}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	// A genuine copy creates a submitted upload and a readable local version.
	w := newEnv(t, Options{Allow: []string{"/work"}})
	w.fake.Seed("work/source", []byte("retained content"))
	if res := w.call(t, "copy", moveInput{From: "/work/source", To: "/work/dest"}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	var page copyJobsOutput
	if res := w.call(t, "list_copy_jobs", copyJobsInput{}, &page); res.IsError || len(page.Jobs) != 1 {
		t.Fatalf("copy not inspectable: %+v %s", page, errText(res))
	}
	job := page.Jobs[0]
	if job.State != journal.CopySubmitted || job.UploadID != job.ID {
		t.Fatalf("wrong upload handoff state: %+v", job)
	}
	for _, tool := range []string{"cancel_copy_job", "retry_copy_job", "forget_copy_job"} {
		var in any = copyJobInput{ID: job.ID}
		if tool == "forget_copy_job" {
			in = forgetCopyJobInput{ID: job.ID, Confirm: true}
		}
		if res := w.call(t, tool, in, nil); !res.IsError {
			t.Fatalf("%s accepted submitted/referenced content", tool)
		}
	}
	data, err := w.fs.ReadFileRange(context.Background(), "/work/dest", 0, 100)
	if err != nil || string(data) != "retained content" {
		t.Fatalf("management damaged local version: %q %v", data, err)
	}
}

func TestCopyJobPaginationIsBoundedPrivateAndComplete(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}, Limits: Limits{MaxBytes: 650, MaxEntries: 10}})
	want := map[string]bool{}
	for i := 0; i < 5; i++ {
		id := seedCopyJob(t, e, "/work/source", "/work/"+strings.Repeat("long", 55)+fmt.Sprint(i), nil)
		want[id] = true
	}
	before := e.fake.TotalCalls()
	cursor, seenCursor := "", ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("cursor did not make progress")
		}
		var page copyJobsOutput
		if res := e.call(t, "list_copy_jobs", copyJobsInput{Cursor: cursor, Limit: 10000}, &page); res.IsError {
			t.Fatal(errText(res))
		}
		data, _ := json.Marshal(page)
		if len(data) > 650 || len(page.Jobs) != 1 {
			t.Fatalf("byte budget not enforced: %d %+v", len(data), page)
		}
		for _, job := range page.Jobs {
			if !want[job.ID] {
				t.Fatalf("duplicate or unexpected job: %s", job.ID)
			}
			delete(want, job.ID)
		}
		if !page.Truncated {
			break
		}
		cursor, seenCursor = page.NextCursor, page.NextCursor
	}
	if len(want) != 0 || e.fake.TotalCalls() != before {
		t.Fatalf("lost jobs or unexpected IO: %v", want)
	}
	for _, cursor := range []string{journal.NewID(), strings.Repeat("a", 200), seenCursor[:len(seenCursor)-2] + "XX"} {
		if res := e.call(t, "list_copy_jobs", copyJobsInput{Cursor: cursor}, nil); !res.IsError {
			t.Fatal("invalid cursor accepted")
		}
	}
	other, err := New(Options{FS: e.fs})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.decodeCopyCursor(seenCursor); err == nil {
		t.Fatal("another server accepted a cursor under different permissions")
	}
}

func TestCopyJobHiddenPageDoesNotExposeCursorIDs(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	for i := 0; i < 201; i++ {
		seedCopyJob(t, e, "/secret/source", fmt.Sprintf("/secret/target-%d", i), nil)
	}
	before := e.fake.TotalCalls()
	var page copyJobsOutput
	if res := e.call(t, "list_copy_jobs", copyJobsInput{}, &page); res.IsError || len(page.Jobs) != 0 || !page.Truncated || page.NextCursor == "" {
		t.Fatalf("hidden page must remain bounded: %+v %s", page, errText(res))
	}
	raw, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
	if err != nil || validCopyID(string(raw)) {
		t.Fatalf("cursor was only base64 encoded: %q %v", raw, err)
	}
	var last copyJobsOutput
	if res := e.call(t, "list_copy_jobs", copyJobsInput{Cursor: page.NextCursor}, &last); res.IsError || last.Truncated || len(last.Jobs) != 0 {
		t.Fatalf("hidden continuation: %+v %s", last, errText(res))
	}
	if e.fake.TotalCalls() != before {
		t.Fatal("hidden pagination contacted provider")
	}
}

func TestCopyJobHTTPToolsUseAuthenticationAndVFS(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	id := seedCopyJob(t, e, "/work/source", "/work/target", nil)
	h := httptest.NewServer(requireBearer(newMCPHTTPHandler(e.server), "resource-test-token"))
	t.Cleanup(h.Close)
	t.Cleanup(func() { e.server.Close() })
	unauthorized := mcp.NewClient(&mcp.Implementation{Name: "unauthorized", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if cs, err := unauthorized.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: h.URL, DisableStandaloneSSE: true, MaxRetries: -1}, nil); err == nil {
		cs.Close()
		t.Fatal("unauthenticated copy-management client connected")
	}
	c := newSubscriptionClient(t, e.server, &mcp.StreamableClientTransport{
		Endpoint: h.URL, HTTPClient: &http.Client{Transport: resourceAuthTransport{base: h.Client().Transport}},
		DisableStandaloneSSE: true, MaxRetries: -1,
	})
	for _, name := range []string{"get_copy_job", "cancel_copy_job"} {
		out, err := c.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: copyJobInput{ID: id}})
		if err != nil || out.IsError {
			t.Fatalf("HTTP %s: %+v %v", name, out, err)
		}
	}
	job, err := e.j.GetCopy(context.Background(), id)
	if err != nil || job.State != journal.CopyCancelled {
		t.Fatalf("HTTP did not reach VFS management: %+v %v", job, err)
	}
}

type copyToolBlockedProvider struct {
	provider.Provider
	blocked atomic.Bool
	entered chan struct{}
}

func (p *copyToolBlockedProvider) ReadRange(ctx context.Context, id, version string, offset, length int64) (io.ReadCloser, error) {
	if p.blocked.Load() && offset >= 1<<20 {
		select {
		case p.entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.Provider.ReadRange(ctx, id, version, offset, length)
}

func TestCopyJobToolCancelsLiveCopyAndRetryPreservesCheckpoint(t *testing.T) {
	e := newEnv(t, Options{Allow: []string{"/work"}})
	body := strings.Repeat("checkpoint", (2<<20)/10)
	e.fake.Seed("work/source", []byte(body))
	blocked := &copyToolBlockedProvider{Provider: e.fake, entered: make(chan struct{}, 1)}
	blocked.blocked.Store(true)
	e.fs.Mounts()[0].Provider = blocked
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		res, err := e.session.CallTool(ctx, &mcp.CallToolParams{Name: "copy", Arguments: moveInput{From: "/work/source", To: "/work/dest"}})
		if err == nil && !res.IsError {
			err = errors.New("cancelled preparation reported copy success")
		}
		done <- err
	}()
	select {
	case <-blocked.entered:
	case <-ctx.Done():
		t.Fatal("copy did not reach blocked download")
	}
	var page copyJobsOutput
	if res := e.call(t, "list_copy_jobs", copyJobsInput{}, &page); res.IsError || len(page.Jobs) != 1 {
		t.Fatalf("running copy not discoverable: %+v %s", page, errText(res))
	}
	id := page.Jobs[0].ID
	if res := e.call(t, "cancel_copy_job", copyJobInput{ID: id}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("copy tool did not return after management cancellation")
	}
	job, err := e.j.GetCopy(ctx, id)
	if err != nil || job.State != journal.CopyCancelled || job.Checkpoint != 1<<20 {
		t.Fatalf("cancel lost checkpoint: %+v %v", job, err)
	}
	blocked.blocked.Store(false)
	if res := e.call(t, "retry_copy_job", copyJobInput{ID: id}, nil); res.IsError {
		t.Fatal(errText(res))
	}
	// This fixture intentionally has no background daemon; drive the same VFS
	// resumption entry that the daemon worker uses after the tool queues retry.
	if _, err := e.fs.ResumeCopy(ctx, id); err != nil {
		t.Fatal(err)
	}
	data, err := e.fs.ReadFileRange(ctx, "/work/dest", 0, int64(len(body)))
	if err != nil || string(data) != body {
		t.Fatalf("retried content differs: len=%d err=%v", len(data), err)
	}
	var info vfs.CopyInfo
	if res := e.call(t, "get_copy_job", copyJobInput{ID: id}, &info); res.IsError || info.State != journal.CopySubmitted {
		t.Fatalf("retry did not hand off upload: %+v %s", info, errText(res))
	}
}

func TestCopyJobToolLimitsErrorsAndAllowlistOwnership(t *testing.T) {
	allow := []string{"/work"}
	e := newEnv(t, Options{Allow: allow, Limits: Limits{MaxBytes: 50}})
	id := seedCopyJob(t, e, "/work/source", "/work/target", nil)
	private := seedCopyJob(t, e, "/secret/source", "/secret/private-target", nil)
	allow[0] = "/" // Caller mutation must not broaden a running server's scope.
	for _, tool := range []string{"get_copy_job", "list_copy_jobs"} {
		var in any = copyJobInput{ID: id}
		if tool == "list_copy_jobs" {
			in = copyJobsInput{}
		}
		if res := e.call(t, tool, in, nil); !res.IsError || !strings.Contains(errText(res), "byte limit") {
			t.Fatalf("%s ignored response cap: %s", tool, errText(res))
		}
	}
	if res := e.call(t, "cancel_copy_job", copyJobInput{ID: private}, nil); !res.IsError || !strings.Contains(errText(res), "inaccessible") {
		t.Fatal("caller modified server allowlist")
	}
	for _, in := range []any{copyJobInput{ID: "../payload"}, copyJobInput{ID: strings.Repeat("x", 4096)}} {
		if res := e.call(t, "get_copy_job", in, nil); !res.IsError {
			t.Fatal("invalid job ID accepted")
		}
	}
	if res := e.call(t, "list_copy_jobs", copyJobsInput{Limit: -1}, nil); !res.IsError {
		t.Fatal("negative list limit accepted")
	}
	if err := e.j.Close(); err != nil {
		t.Fatal(err)
	}
	if res := e.call(t, "get_copy_job", copyJobInput{ID: id}, nil); !res.IsError || errText(res) != "copy management failed; inspect current state before retrying" {
		t.Fatalf("raw storage error exposed: %s", errText(res))
	}
}
