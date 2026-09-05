package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloudfs/internal/journal"
	"cloudfs/internal/upload"
)

func TestPendingUploadCleanupIsVisibleWithoutExposingPrivateIntent(t *testing.T) {
	f := newFixture(t)
	u := f.queue(t, "retained", 7)
	ctx := t.Context()
	if _, err := f.j.RequestCancel(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.j.BeginUploadCleanup(ctx, u.ID, "private-metadata-identity"); err != nil {
		t.Fatal(err)
	}
	st := f.coll.Collect(ctx)
	if st.Uploads.Purging != 1 || st.Uploads.RetainedBytes != 7 || !strings.Contains(strings.Join(st.Warnings, " "), "unfinished local cleanup") {
		t.Fatalf("hidden cleanup status: %+v", st)
	}
	d := &Doctor{Journal: f.j}
	checks := d.checkJournal(ctx)
	if len(checks) == 0 || checks[0].Level != LevelWarn || checks[0].Fixable || !strings.Contains(checks[0].Detail, "unfinished local cleanup") {
		t.Fatalf("doctor hid or offered to reset cleanup: %+v", checks)
	}
	srv := NewServer(f.coll)
	for _, path := range []string{"/uploads", "/status", "/metrics"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "http://cloudfs"+path, nil)
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "purging") {
			t.Fatalf("%s omitted cleanup: %d %s", path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "private-metadata-identity") || strings.Contains(w.Body.String(), u.BlobPath) {
			t.Fatalf("%s exposed private cleanup intent", path)
		}
	}
	// Retry remains a conflict, not a way to bypass the cleanup coordinator.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "http://cloudfs/uploads/retry", strings.NewReader(`{"id":"`+u.ID+`"}`))
	r.Header.Set("X-CloudFS-Control", "1")
	r.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("cleanup retry status=%d body=%s", w.Code, w.Body.String())
	}
	if row, err := f.j.Get(ctx, u.ID); err != nil || row.State != journal.StatePurging {
		t.Fatalf("retry reset cleanup: %+v %v", row, err)
	}
	_, err := ManageUploads(ctx, f.j, func(context.Context) (journal.Stats, error) {
		return journal.Stats{Purging: 1}, upload.ErrCleanupPending
	}, nil, UploadRequest{Action: "flush"})
	if err != upload.ErrCleanupPending {
		t.Fatalf("flush hid cleanup: %v", err)
	}
}
