package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloudfs/internal/export"
	"cloudfs/internal/provider"

	"github.com/google/uuid"
)

// fakeExports stands in for the manager. The routes are what is under test
// here — the guard, the confirmations, the shape of the reply and what is
// left out of it — and none of that needs a real destination disk.
type fakeExports struct {
	jobs     map[string]export.Job
	order    []string
	items    map[string][]export.Item
	created  export.Request
	acted    []string
	failWith error
}

func newFakeExports() *fakeExports {
	return &fakeExports{jobs: map[string]export.Job{}, items: map[string][]export.Item{}}
}

func (f *fakeExports) add(j export.Job) {
	f.jobs[j.ID] = j
	f.order = append(f.order, j.ID)
}

func (f *fakeExports) Create(_ context.Context, req export.Request) (export.Job, error) {
	if f.failWith != nil {
		return export.Job{}, f.failWith
	}
	f.created = req
	j := export.Job{ID: uuid.NewString(), State: export.StatePlanning, Sources: req.Sources, Dest: req.Dest,
		Options: export.JobOptions{Mirror: req.Mirror, Verify: req.Verify}, CreatedAt: time.Now()}
	f.add(j)
	return j, nil
}

func (f *fakeExports) Job(_ context.Context, id string) (export.Job, error) {
	j, ok := f.jobs[id]
	if !ok {
		return export.Job{}, export.ErrNotFound
	}
	return j, nil
}

func (f *fakeExports) Jobs(_ context.Context, limit int, after string) ([]export.Job, string, error) {
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
		next = ids[limit-1]
		ids = ids[:limit]
	}
	out := make([]export.Job, 0, len(ids))
	for _, id := range ids {
		out = append(out, f.jobs[id])
	}
	return out, next, nil
}

func (f *fakeExports) Items(_ context.Context, id string) ([]export.Item, error) {
	return f.items[id], nil
}

func (f *fakeExports) ItemsPage(ctx context.Context, id, after string, limit int, state export.ItemState) ([]export.Item, string, error) {
	items, err := f.Items(ctx, id)
	if err != nil {
		return nil, "", err
	}
	filtered := make([]export.Item, 0, len(items))
	for _, it := range items {
		if it.Rel > after && (state == "" || it.State == state) {
			filtered = append(filtered, it)
		}
	}
	next := ""
	if len(filtered) > limit {
		next = filtered[limit-1].Rel
		filtered = filtered[:limit]
	}
	return filtered, next, nil
}

func (f *fakeExports) Progress(_ context.Context, id string) (export.Progress, error) {
	j, ok := f.jobs[id]
	if !ok {
		return export.Progress{}, export.ErrNotFound
	}
	return export.Progress{BytesDone: j.BytesDone, BytesTotal: j.BytesTotal, Rate: 1024, ETA: 4 * time.Second,
		Members: []export.MemberProgress{{Remote: "demo", Inflight: 2, Bytes: j.BytesDone, Rate: 512}}}, nil
}

func (f *fakeExports) act(action, id string) error {
	if _, ok := f.jobs[id]; !ok {
		return export.ErrNotFound
	}
	f.acted = append(f.acted, action+" "+id)
	return nil
}

func (f *fakeExports) Pause(_ context.Context, id string) error  { return f.act("pause", id) }
func (f *fakeExports) Resume(_ context.Context, id string) error { return f.act("resume", id) }
func (f *fakeExports) Cancel(_ context.Context, id string) error { return f.act("cancel", id) }
func (f *fakeExports) Forget(_ context.Context, id string) error { return f.act("forget", id) }

// exportFixture is the server with a fake manager behind it.
func exportFixture(t *testing.T) (*Server, *fakeExports) {
	t.Helper()
	f := newFixture(t)
	fake := newFakeExports()
	f.coll.Export = fake
	return NewServer(f.coll), fake
}

// do sends one control-plane request the way a local client does.
func do(t *testing.T, s *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://cloudfs"+target, strings.NewReader(body))
	req.Host = "cloudfs"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CloudFS-Control", "1")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// TestExportRoutesAreRegistered: a route that is not in routes() is a route
// the guard test never walks, which is how /cache/drop went unguarded. The
// four endpoints §5.6 names have to be in the table by pattern.
func TestExportRoutesAreRegistered(t *testing.T) {
	s, _ := exportFixture(t)
	want := map[string]bool{"/export": false, "/exports": false, "/exports/": false}
	for _, r := range s.routes() {
		if _, ok := want[r.pattern]; ok {
			want[r.pattern] = true
			if r.open {
				t.Errorf("%s is marked open; every export route changes or names local state", r.pattern)
			}
		}
	}
	for pattern, found := range want {
		if !found {
			t.Errorf("%s is not registered in routes()", pattern)
		}
	}
}

// TestExportStartRequiresConfirmationForMirror: a mirror is the only export
// that deletes anything, so it must not start on the strength of one flag.
func TestExportStartRequiresConfirmationForMirror(t *testing.T) {
	s, fake := exportFixture(t)
	w := do(t, s, "POST", "/export", `{"sources":["/a"],"dest":"/tmp/x","mirror":true}`)
	if w.Code != 400 {
		t.Fatalf("mirror without confirm: got %d %s, want 400", w.Code, w.Body.String())
	}
	if len(fake.order) != 0 {
		t.Fatal("a job was created for an unconfirmed mirror")
	}
	w = do(t, s, "POST", "/export", `{"sources":["/a"],"dest":"/tmp/x","mirror":true,"confirm":true}`)
	if w.Code != 200 {
		t.Fatalf("confirmed mirror: got %d %s", w.Code, w.Body.String())
	}
	var out ExportStartResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.ID == "" {
		t.Fatalf("no job id in %s (%v)", w.Body.String(), err)
	}
	if !fake.created.Mirror || fake.created.Dest != "/tmp/x" {
		t.Fatalf("request reached the manager as %+v", fake.created)
	}
}

// TestExportStartRejectsMalformedRequests keeps the refusals at the edge,
// where the caller can still read which part of their request was wrong.
func TestExportStartRejectsMalformedRequests(t *testing.T) {
	s, _ := exportFixture(t)
	for _, tc := range []struct{ name, body string }{
		{"no sources", `{"sources":[],"dest":"/tmp/x"}`},
		{"relative source", `{"sources":["photos"],"dest":"/tmp/x"}`},
		{"no destination", `{"sources":["/a"],"dest":"  "}`},
		{"negative tunable", `{"sources":["/a"],"dest":"/tmp/x","streams":-1}`},
		{"unknown field", `{"sources":["/a"],"dest":"/tmp/x","nonsense":1}`},
	} {
		if w := do(t, s, "POST", "/export", tc.body); w.Code != 400 {
			t.Errorf("%s: got %d %s, want 400", tc.name, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
}

// TestExportListAndDetailOmitPrivatePlanState: the plan carries remote object
// ids, versions and the mount binding each source came from. None of that is
// something a page or an agent has any use for, and all of it names another
// account's objects.
func TestExportListAndDetailOmitPrivatePlanState(t *testing.T) {
	s, fake := exportFixture(t)
	id := uuid.NewString()
	fake.add(export.Job{
		ID: id, State: export.StateRunning, Sources: []string{"/photos"}, Dest: "/Volumes/backup",
		BytesTotal: 2048, BytesDone: 1024, FilesTotal: 2, FilesDone: 1,
		MetaIdentity: "private-database",
		Bindings:     []export.Binding{{Prefix: "/", Remote: "ali", RootID: "private-root-id", AccountBinding: "private-binding"}},
		CreatedAt:    time.Now(),
	})
	fake.items[id] = []export.Item{{
		JobID: id, Rel: "photos/a.jpg", VPath: "/photos/a.jpg", Kind: provider.KindFile, Size: 1024,
		RemoteID: "private-remote-id", Version: "private-version", Hash: "private-hash", State: export.ItemDone,
	}}
	for _, target := range []string{"/exports", "/exports/" + id} {
		w := do(t, s, "GET", target, "")
		if w.Code != 200 {
			t.Fatalf("%s: got %d %s", target, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "private-") {
			t.Fatalf("%s leaked plan state: %s", target, w.Body.String())
		}
	}
	var detail ExportsResponse
	if err := json.Unmarshal(do(t, s, "GET", "/exports/"+id, "").Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Jobs) != 1 || detail.Jobs[0].ID != id {
		t.Fatalf("detail did not name the job: %+v", detail)
	}
	page := do(t, s, "GET", "/exports/"+id+"/items?limit=1&state=done", "")
	if page.Code != 200 {
		t.Fatalf("item page: got %d %s", page.Code, page.Body.String())
	}
	if detail.Progress == nil || detail.Progress.Rate != 1024 || detail.Progress.ETASeconds != 4 {
		t.Fatalf("detail carries no live progress: %+v", detail.Progress)
	}
	if len(detail.Items) != 1 || detail.Items[0].Rel != "photos/a.jpg" {
		t.Fatalf("detail carries no plan: %+v", detail.Items)
	}
	// A running job in a listing carries its rate, because the page draws one
	// line per job and does not fetch each of them.
	var list ExportsResponse
	if err := json.Unmarshal(do(t, s, "GET", "/exports", "").Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Jobs) != 1 || list.Jobs[0].Rate != 1024 {
		t.Fatalf("listing has no live rate for a running job: %+v", list.Jobs)
	}
	if list.Items != nil {
		t.Fatal("a listing must not carry every job's plan")
	}
}

// TestExportMutationsNeedTheRightConfirmation: forget removes the record and
// the partial files under it, so it demands confirm=true; the other three do
// not accept one, which is the convention every other admin route here keeps.
func TestExportMutationsNeedTheRightConfirmation(t *testing.T) {
	s, fake := exportFixture(t)
	id := uuid.NewString()
	fake.add(export.Job{ID: id, State: export.StateRunning, Dest: "/tmp/x"})
	if w := do(t, s, "POST", "/exports/forget", `{"id":"`+id+`"}`); w.Code != 400 {
		t.Fatalf("forget without confirm: got %d %s", w.Code, w.Body.String())
	}
	if w := do(t, s, "POST", "/exports/pause", `{"id":"`+id+`","confirm":true}`); w.Code != 400 {
		t.Fatalf("pause with a confirmation: got %d %s", w.Code, w.Body.String())
	}
	for _, action := range []string{"pause", "resume", "cancel"} {
		if w := do(t, s, "POST", "/exports/"+action, `{"id":"`+id+`"}`); w.Code != 200 {
			t.Fatalf("%s: got %d %s", action, w.Code, w.Body.String())
		}
	}
	w := do(t, s, "POST", "/exports/forget", `{"id":"`+id+`","confirm":true}`)
	if w.Code != 200 {
		t.Fatalf("forget: got %d %s", w.Code, w.Body.String())
	}
	var out ExportsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Forgotten != id {
		t.Fatalf("forget did not name the job: %s (%v)", w.Body.String(), err)
	}
	want := []string{"pause " + id, "resume " + id, "cancel " + id, "forget " + id}
	if len(fake.acted) != len(want) {
		t.Fatalf("manager saw %v, want %v", fake.acted, want)
	}
}

// TestExportUnknownIDsAndActionsAreNotFound: the one prefix serves both a job
// ID and the four action names, so a spelling that is neither must be a 404
// rather than a confusing 400 from the decoder.
func TestExportUnknownIDsAndActionsAreNotFound(t *testing.T) {
	s, _ := exportFixture(t)
	for _, target := range []string{"/exports/not-a-uuid", "/exports/", "/exports/purge"} {
		if w := do(t, s, "GET", target, ""); w.Code != 404 {
			t.Errorf("GET %s: got %d, want 404", target, w.Code)
		}
	}
	if w := do(t, s, "GET", "/exports/"+uuid.NewString(), ""); w.Code != 404 {
		t.Errorf("GET an unknown job: got %d, want 404", w.Code)
	}
}

// TestExportRoutesAnswerWhenNoQueueIsWired: a daemon without an export
// manager says so rather than panicking on a nil field.
func TestExportRoutesAnswerWhenNoQueueIsWired(t *testing.T) {
	f := newFixture(t)
	s := NewServer(f.coll)
	for _, tc := range []struct{ method, target, body string }{
		{"POST", "/export", `{"sources":["/a"],"dest":"/tmp/x"}`},
		{"GET", "/exports", ""},
		{"POST", "/exports/pause", `{"id":"` + uuid.NewString() + `"}`},
	} {
		if w := do(t, s, tc.method, tc.target, tc.body); w.Code != 503 {
			t.Errorf("%s %s: got %d %s, want 503", tc.method, tc.target, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
}

// TestExportStartSurfacesTheReasonItWasRefused: a refusal a person has to act
// on — a path that is under no mount, a mirror with no marker — must name
// what was wrong, while anything else stays a generic sentence.
func TestExportStartSurfacesTheReasonItWasRefused(t *testing.T) {
	s, fake := exportFixture(t)
	fake.failWith = export.ErrMirrorMarkerMissing
	w := do(t, s, "POST", "/export", `{"sources":["/a"],"dest":"/tmp/x","mirror":true,"confirm":true}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "marker") {
		t.Fatalf("mirror without a marker: got %d %s", w.Code, w.Body.String())
	}
	fake.failWith = errors.New("destination: /Volumes/backup: input/output error")
	w = do(t, s, "POST", "/export", `{"sources":["/a"],"dest":"/tmp/x"}`)
	if w.Code == 200 || strings.Contains(w.Body.String(), "/Volumes/backup") {
		t.Fatalf("an unclassified failure was passed through verbatim: %d %s", w.Code, w.Body.String())
	}
}

// TestExportClientHelpersRoundTrip exercises CallExport/CallExports/
// CallExportMutation over the real socket, which is what the CLI uses.
func TestExportClientHelpersRoundTrip(t *testing.T) {
	f := newFixture(t)
	fake := newFakeExports()
	f.coll.Export = fake
	ctx := context.Background()
	p := socketPath(t)
	srv, err := NewServer(f.coll).Start(ctx, p, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	started, online, err := CallExport(ctx, p, "", ExportRequest{Sources: []string{"/photos"}, Dest: "/tmp/dest"})
	if err != nil || !online || started.ID == "" {
		t.Fatalf("start: %+v %v %v", started, online, err)
	}
	list, online, err := CallExports(ctx, p, "", ExportsRequest{Limit: 10})
	if err != nil || !online || len(list.Jobs) != 1 || list.Jobs[0].ID != started.ID {
		t.Fatalf("list: %+v %v %v", list, online, err)
	}
	one, online, err := CallExports(ctx, p, "", ExportsRequest{ID: started.ID})
	if err != nil || !online || len(one.Jobs) != 1 || one.Progress == nil {
		t.Fatalf("show: %+v %v %v", one, online, err)
	}
	if _, _, err := CallExportMutation(ctx, p, "", ExportMutationRequest{Action: "cancel", ID: started.ID}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	forgotten, online, err := CallExportMutation(ctx, p, "", ExportMutationRequest{Action: "forget", ID: started.ID, Confirm: true})
	if err != nil || !online || forgotten.Forgotten != started.ID {
		t.Fatalf("forget: %+v %v %v", forgotten, online, err)
	}
	// The client refuses the same combinations the daemon does, so a mistyped
	// command never reaches the socket.
	if _, _, err := CallExportMutation(ctx, p, "", ExportMutationRequest{Action: "forget", ID: started.ID}); err == nil {
		t.Fatal("forget without a confirmation reached the daemon")
	}
	if _, _, err := CallExports(ctx, p, "", ExportsRequest{ID: "not-a-uuid"}); err == nil {
		t.Fatal("a malformed job id reached the daemon")
	}
}
