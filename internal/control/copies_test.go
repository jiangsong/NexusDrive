package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"cloudfs/internal/journal"
)

func TestCopiesControlPagesAndOmitsPrivateState(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ids := map[string]bool{}
	for i := range 3 {
		c, err := f.j.BeginCopy(ctx, journal.CopySpec{SourcePath: "/source/a", SourceRemote: "source", SourceID: "private-source-id", SourceVersion: "private-version", Size: 1,
			TargetPath: fmt.Sprintf("/target/%d", i), TargetRemote: "target", TargetParentID: "private-parent", Mode: "writeback", MetaIdentity: "private-database"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		id := c.Job().ID
		ids[id] = true
		c.Close()
		if err := f.j.FailCopy(ctx, id, "private-error-token"); err != nil {
			t.Fatal(err)
		}
	}
	p := socketPath(t)
	srv, err := NewServer(f.coll).Start(ctx, p, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	q := CopiesRequest{Limit: 1}
	for {
		out, online, err := CallCopies(ctx, p, "", q)
		if err != nil || !online || len(out.Copies) != 1 {
			t.Fatalf("list: %+v %v %v", out, online, err)
		}
		job := out.Copies[0]
		if !ids[job.ID] {
			t.Fatal("duplicate or missing job")
		}
		delete(ids, job.ID)
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "private-") || strings.Contains(string(b), ".part") {
			t.Fatalf("leaked internal state: %s", b)
		}
		if job.State != journal.CopyFailed || job.Warning == "" {
			t.Fatalf("missing failure state: %+v", job)
		}
		one, online, err := CallCopies(ctx, p, "", CopiesRequest{ID: job.ID})
		if err != nil || !online || len(one.Copies) != 1 || one.Copies[0] != job {
			t.Fatalf("show: %+v %v %v", one, online, err)
		}
		if out.NextCursor == "" {
			break
		}
		q.Cursor = out.NextCursor
	}
	if len(ids) != 0 {
		t.Fatalf("missed jobs: %v", ids)
	}
	if _, online, err := CallCopies(ctx, p, "", CopiesRequest{ID: journal.NewID()}); !online || err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("missing: %v %v", online, err)
	}
}

func TestCopiesControlRejectsBrowserAndInvalidQueries(t *testing.T) {
	f := newFixture(t)
	s := NewServer(f.coll)
	for _, tc := range []struct {
		method, host, query, origin string
		status                      int
	}{
		{"GET", "cloudfs", "", "https://untrusted.invalid", 403},
		{"GET", "untrusted.invalid", "", "", 403},
		{"POST", "cloudfs", "", "", 405},
		{"GET", "cloudfs", "limit=0", "", 400},
		{"GET", "cloudfs", "limit=1001", "", 400},
		{"GET", "cloudfs", "limit=x", "", 400},
		{"GET", "cloudfs", "limit=1&limit=2", "", 400},
		{"GET", "cloudfs", "cursor=../bad", "", 400},
		{"GET", "cloudfs", "id=", "", 400},
		{"GET", "cloudfs", "overwrite=true", "", 400},
		{"GET", "cloudfs", "id=" + journal.NewID() + "&limit=1", "", 400},
		{"GET", "cloudfs", "cursor=%xx", "", 400},
	} {
		r := httptest.NewRequest(tc.method, "http://"+tc.host+"/copies?"+tc.query, nil)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-CloudFS-Control", "1")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%+v: status=%d %s", tc, w.Code, w.Body.String())
		}
	}
}

func TestCopiesSubmittedMeansHandoffNotUploadSuccess(t *testing.T) {
	f, _ := cacheControl(t)
	f.coll.FS.SetWriteBackend(f.j, nil)
	ctx := context.Background()
	if _, err := f.coll.FS.Copy(ctx, "/docs/a", "/docs/b"); err != nil {
		t.Fatal(err)
	}
	out, err := InspectCopies(ctx, f.j, CopiesRequest{})
	if err != nil || len(out.Copies) != 1 || out.Copies[0].State != journal.CopySubmitted || out.Copies[0].UploadID != out.Copies[0].ID {
		t.Fatalf("handoff=%+v %v", out, err)
	}
	u, err := f.j.Get(ctx, out.Copies[0].UploadID)
	if err != nil || u.State != journal.StatePending {
		t.Fatalf("inspection uploaded: %+v %v", u, err)
	}
}
