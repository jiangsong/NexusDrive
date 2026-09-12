package mcpsrv

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/export"

	"github.com/google/uuid"
)

// fakeExportJobs records what the tools asked for. The boundary under test is
// the server's, not the runner's: whether a destination is allowed, whether a
// read-only server refuses, and what comes back out.
type fakeExportJobs struct {
	created   []export.Request
	cancelled []string
	jobs      map[string]export.Job
	order     []string
}

func newFakeExportJobs() *fakeExportJobs {
	return &fakeExportJobs{jobs: map[string]export.Job{}}
}

func (f *fakeExportJobs) Create(_ context.Context, req export.Request) (export.Job, error) {
	f.created = append(f.created, req)
	j := export.Job{ID: uuid.NewString(), State: export.StatePlanning, Sources: req.Sources, Dest: req.Dest,
		Options: export.JobOptions{Mirror: req.Mirror, Verify: req.Verify}}
	f.jobs[j.ID] = j
	f.order = append(f.order, j.ID)
	return j, nil
}

func (f *fakeExportJobs) Job(_ context.Context, id string) (export.Job, error) {
	j, ok := f.jobs[id]
	if !ok {
		return export.Job{}, export.ErrNotFound
	}
	return j, nil
}

func (f *fakeExportJobs) Jobs(_ context.Context, limit int, after string) ([]export.Job, string, error) {
	ids := f.order
	if after != "" {
		for i, id := range ids {
			if id == after {
				ids = ids[i+1:]
				break
			}
		}
	}
	next := ""
	if limit > 0 && len(ids) > limit {
		next, ids = ids[limit-1], ids[:limit]
	}
	out := make([]export.Job, 0, len(ids))
	for _, id := range ids {
		out = append(out, f.jobs[id])
	}
	return out, next, nil
}

func (f *fakeExportJobs) Cancel(_ context.Context, id string) error {
	if _, ok := f.jobs[id]; !ok {
		return export.ErrNotFound
	}
	f.cancelled = append(f.cancelled, id)
	return nil
}

func (f *fakeExportJobs) Progress(_ context.Context, id string) (export.Progress, error) {
	j, ok := f.jobs[id]
	if !ok {
		return export.Progress{}, export.ErrNotFound
	}
	return export.Progress{BytesDone: j.BytesDone, BytesTotal: j.BytesTotal, Rate: 512, ETA: 2 * time.Second}, nil
}

// TestExportToolRefusesADestinationOutsideTheExportRoots is the whole reason
// mcp.export_roots exists. The path allowlist says what an agent may read out
// of the mount; it says nothing about where on this machine those bytes may
// be written, and an export that could name any directory would be a
// file-writing primitive nobody granted.
func TestExportToolRefusesADestinationOutsideTheExportRoots(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	fake := newFakeExportJobs()
	e := newEnv(t, Options{Export: fake, ExportRoots: []string{root}})

	for _, dest := range []string{
		outside,
		filepath.Join(root, "..", filepath.Base(outside)),
		root + "-sibling",
		"relative/dir",
		"",
	} {
		res := e.call(t, "export", map[string]any{"paths": []string{"/"}, "dest": dest}, nil)
		if !res.IsError {
			t.Fatalf("dest %q was accepted", dest)
		}
		if dest != "" && !strings.Contains(errText(res), "export root") {
			t.Errorf("dest %q was refused for the wrong reason: %s", dest, errText(res))
		}
	}
	if len(fake.created) != 0 {
		t.Fatalf("a refused destination still reached the manager: %+v", fake.created)
	}

	// The root itself and anything beneath it — including a directory that
	// does not exist yet — is what the configuration allows.
	for _, dest := range []string{root, filepath.Join(root, "photos", "2024")} {
		var out exportJobView
		res := e.call(t, "export", map[string]any{"paths": []string{"/"}, "dest": dest}, &out)
		if res.IsError {
			t.Fatalf("dest %q was refused: %s", dest, errText(res))
		}
		if out.ID == "" || out.Dest != dest {
			t.Fatalf("dest %q came back as %+v", dest, out)
		}
	}
}

// TestExportToolsAreAbsentWithoutARootOrAQueue: a server with no export
// manager must not advertise the tools at all, and one with a manager but no
// configured root must refuse every destination rather than defaulting to
// somewhere convenient.
func TestExportToolsAreAbsentWithoutARootOrAQueue(t *testing.T) {
	plain := newEnv(t, Options{})
	tools, err := plain.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		switch tool.Name {
		case "export", "list_export_jobs", "get_export_job", "cancel_export_job":
			t.Errorf("a server with no export queue still advertises %s", tool.Name)
		}
	}

	e := newEnv(t, Options{Export: newFakeExportJobs()})
	res := e.call(t, "export", map[string]any{"paths": []string{"/"}, "dest": t.TempDir()}, nil)
	if !res.IsError || !strings.Contains(errText(res), "export_roots") {
		t.Fatalf("an unconfigured server accepted a destination: %v %s", res.IsError, errText(res))
	}
}

// TestReadOnlyServerRefusesExportAndCancel: read_only is the setting someone
// turns on so an agent cannot change anything. Writing a copy of the drive
// onto the local disk is a change, and so is stopping a job somebody else
// started.
func TestReadOnlyServerRefusesExportAndCancel(t *testing.T) {
	root := t.TempDir()
	fake := newFakeExportJobs()
	e := newEnv(t, Options{Export: fake, ExportRoots: []string{root}, ReadOnly: true})

	res := e.call(t, "export", map[string]any{"paths": []string{"/"}, "dest": root}, nil)
	if !res.IsError || !strings.Contains(errText(res), "read-only") {
		t.Fatalf("a read-only server started an export: %v %s", res.IsError, errText(res))
	}
	res = e.call(t, "cancel_export_job", map[string]any{"id": uuid.NewString()}, nil)
	if !res.IsError || !strings.Contains(errText(res), "read-only") {
		t.Fatalf("a read-only server cancelled a job: %v %s", res.IsError, errText(res))
	}
	if len(fake.created) != 0 || len(fake.cancelled) != 0 {
		t.Fatalf("a read-only server reached the manager: %+v %+v", fake.created, fake.cancelled)
	}
	// Reading is still allowed: that is what read-only means.
	if res := e.call(t, "list_export_jobs", map[string]any{}, &exportJobsOutput{}); res.IsError {
		t.Fatalf("a read-only server refused to list jobs: %s", errText(res))
	}
}

// TestExportJobToolsListInspectAndCancel walks the read and management tools
// over one job, including the cursor, which shares its key with the copy and
// upload cursors and must not be interchangeable with them.
func TestExportJobToolsListInspectAndCancel(t *testing.T) {
	root := t.TempDir()
	fake := newFakeExportJobs()
	e := newEnv(t, Options{Export: fake, ExportRoots: []string{root}})

	var started exportJobView
	if res := e.call(t, "export", map[string]any{"paths": []string{"/"}, "dest": root, "verify": true}, &started); res.IsError {
		t.Fatalf("export: %s", errText(res))
	}
	if len(fake.created) != 1 || !fake.created[0].Verify || fake.created[0].Sources[0] != "/" {
		t.Fatalf("the request reached the manager as %+v", fake.created)
	}

	var list exportJobsOutput
	if res := e.call(t, "list_export_jobs", map[string]any{}, &list); res.IsError {
		t.Fatalf("list: %s", errText(res))
	}
	if len(list.Jobs) != 1 || list.Jobs[0].ID != started.ID {
		t.Fatalf("list: %+v", list)
	}

	var got exportJobView
	if res := e.call(t, "get_export_job", map[string]any{"id": started.ID}, &got); res.IsError {
		t.Fatalf("get: %s", errText(res))
	}
	if got.Rate != 512 || got.ETASeconds != 2 {
		t.Fatalf("get carries no live progress: %+v", got)
	}
	if res := e.call(t, "get_export_job", map[string]any{"id": "not-a-uuid"}, nil); !res.IsError {
		t.Fatal("a malformed job id was accepted")
	}
	if res := e.call(t, "list_export_jobs", map[string]any{"cursor": "!!!"}, nil); !res.IsError {
		t.Fatal("a malformed cursor was accepted")
	}
	// A cursor minted for another listing must not open this one: the three
	// job listings share one key and are separated only by their AAD.
	if res := e.call(t, "list_export_jobs", map[string]any{"cursor": e.server.encodeCopyCursor(started.ID)}, nil); !res.IsError {
		t.Fatal("a copy cursor was accepted as an export cursor")
	}

	if res := e.call(t, "cancel_export_job", map[string]any{"id": started.ID}, nil); res.IsError {
		t.Fatalf("cancel: %s", errText(res))
	}
	if len(fake.cancelled) != 1 || fake.cancelled[0] != started.ID {
		t.Fatalf("cancel did not reach the manager: %+v", fake.cancelled)
	}
}

// TestExportToolHonoursThePathAllowlist: the destination boundary is the new
// one, but the source boundary is the old one and must still hold.
func TestExportToolHonoursThePathAllowlist(t *testing.T) {
	root := t.TempDir()
	fake := newFakeExportJobs()
	e := newEnv(t, Options{Export: fake, ExportRoots: []string{root}, Allow: []string{"/work"}})

	res := e.call(t, "export", map[string]any{"paths": []string{"/work/a", "/secrets"}, "dest": root}, nil)
	if !res.IsError {
		t.Fatal("a source outside the allowlist was accepted")
	}
	if len(fake.created) != 0 {
		t.Fatalf("a denied source still reached the manager: %+v", fake.created)
	}
}
