package control

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"cloudfs/internal/journal"
)

func TestUploadResumeHTTPRequiresConfirmationAndUsesVFS(t *testing.T) {
	f, _ := cacheControl(t)
	f.coll.FS.SetWriteBackend(f.j, nil)
	f.coll.ResumeUpload = f.coll.FS.ResumeUpload
	ctx := context.Background()
	if _, err := f.coll.FS.WriteFile(ctx, "/docs/resume", []byte("retained"), false); err != nil {
		t.Fatal(err)
	}
	rows, err := f.j.Pending(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queue: %+v %v", rows, err)
	}
	id := rows[0].ID
	if _, err := f.j.RequestCancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	s := NewServer(f.coll)
	for _, tc := range []struct {
		body, origin string
		status       int
	}{
		{`{"id":"` + id + `"}`, "", 400},
		{`{"id":"` + id + `","confirm":true}`, "https://evil.example", 403},
		{`{"id":"` + id + `","confirm":true,"all":true}`, "", 400},
		{`{"id":"` + id + `","confirm":true}`, "", 200},
	} {
		r := httptest.NewRequest("POST", "http://cloudfs/uploads/resume", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CloudFS-Control", "1")
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	if u, err := f.j.Get(ctx, id); err != nil || u.State != journal.StatePending {
		t.Fatalf("resume: %+v %v", u, err)
	}
}
