package control

import (
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"cloudfs/internal/journal"
	"cloudfs/internal/meta"
)

func TestUploadDiscardHTTPRequiresUnambiguousConfirmationAndUsesVFS(t *testing.T) {
	f, p := cacheControl(t)
	f.coll.FS.SetWriteBackend(f.j, nil)
	f.coll.DiscardUpload = f.coll.FS.DiscardUpload
	ctx := t.Context()
	if _, err := f.coll.FS.WriteFile(ctx, "/docs/retained", []byte("retained"), false); err != nil {
		t.Fatal(err)
	}
	rows, err := f.j.Pending(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queue: %+v %v", rows, err)
	}
	u := rows[0]
	if _, err := f.j.RequestCancel(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	s := NewServer(f.coll)
	confirmed := `{"id":"` + u.ID + `","confirm":true}`
	for _, tc := range []struct {
		method, query, body, origin, host, header, content string
		status                                             int
	}{
		{"POST", "", `{"id":"` + u.ID + `"}`, "", "cloudfs", "1", "application/json", 400},
		{"POST", "", confirmed, "https://evil.invalid", "cloudfs", "1", "application/json", 403},
		{"POST", "", confirmed, "", "evil.invalid", "1", "application/json", 403},
		{"POST", "", confirmed, "", "cloudfs", "", "application/json", 403},
		{"GET", "", confirmed, "", "cloudfs", "1", "application/json", 405},
		{"POST", "", confirmed, "", "cloudfs", "1", "text/plain", 415},
		{"POST", "?confirm=true", confirmed, "", "cloudfs", "1", "application/json", 400},
		{"POST", "", confirmed + `{}`, "", "cloudfs", "1", "application/json", 400},
		{"POST", "", `null`, "", "cloudfs", "1", "application/json", 400},
		{"POST", "", `{"id":"` + u.ID + `","confirm":false,"confirm":true}`, "", "cloudfs", "1", "application/json", 400},
		{"POST", "", `{"id":"other","id":"` + u.ID + `","confirm":true}`, "", "cloudfs", "1", "application/json", 400},
		{"POST", "", `{"id":"` + u.ID + `","Confirm":true}`, "", "cloudfs", "1", "application/json", 400},
		{"POST", "", `{"id":"` + u.ID + `","confirm":true,"all":false}`, "", "cloudfs", "1", "application/json", 400},
		{"POST", "", `{"id":"` + strings.Repeat("x", 5000) + `","confirm":true}`, "", "cloudfs", "1", "application/json", 400},
	} {
		r := httptest.NewRequest(tc.method, "http://"+tc.host+"/uploads/drop"+tc.query, strings.NewReader(tc.body))
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-CloudFS-Control", tc.header)
		r.Header.Set("Content-Type", tc.content)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
		if row, err := f.j.Get(ctx, u.ID); err != nil || row.State != journal.StateCancelled {
			t.Fatalf("refusal mutated upload: %+v %v", row, err)
		}
	}
	socket := socketPath(t)
	srv, err := s.Start(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	q := UploadRequest{Action: "drop", ID: u.ID, Confirm: true}
	h, err := f.coll.FS.Open(ctx, u.Ino, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, online, err := CallUploads(ctx, socket, "", q); !online || err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("open reader discarded: %v %v", online, err)
	}
	if err := f.coll.FS.Release(ctx, h); err != nil {
		t.Fatal(err)
	}
	if _, err := f.meta.DB().Exec(`CREATE TRIGGER fail_discard BEFORE DELETE ON nodes BEGIN SELECT RAISE(ABORT,'private-credential-path'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CallUploads(ctx, socket, "", q); err == nil || !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "private-credential-path") {
		t.Fatalf("failure leaked or disappeared: %v", err)
	}
	if row, err := f.j.Get(ctx, u.ID); err != nil || row.State != journal.StatePurging {
		t.Fatalf("failure lost intent: %+v %v", row, err)
	}
	if _, err := f.meta.DB().Exec(`DROP TRIGGER fail_discard`); err != nil {
		t.Fatal(err)
	}
	before := p.TotalCalls()
	out, online, err := CallUploads(ctx, socket, "", q)
	if err != nil || !online || out.Discarded != u.ID || out.State != "" || !strings.Contains(out.Warning, "not undone") {
		t.Fatalf("discard: %+v %v %v", out, online, err)
	}
	if p.TotalCalls() != before {
		t.Fatal("discard made provider calls")
	}
	if _, err := f.meta.Get(ctx, u.Ino); !errors.Is(err, meta.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := os.Stat(u.BlobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, _, err := CallUploads(ctx, socket, "", q); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("repeated discard invented success: %v", err)
	}
}

func TestUploadDiscardCannotFallThroughToJournalOnlyManagement(t *testing.T) {
	f := newFixture(t)
	u := f.queue(t, "retained", 8)
	if _, err := f.j.RequestCancel(t.Context(), u.ID); err != nil {
		t.Fatal(err)
	}
	q := UploadRequest{Action: "drop", ID: u.ID, Confirm: true}
	if _, err := ManageUploads(t.Context(), f.j, nil, nil, q); err == nil {
		t.Fatal("journal-only discard accepted")
	}
	if _, err := ManageUploadDiscard(t.Context(), nil, q); err == nil {
		t.Fatal("missing coordinator accepted")
	}
	if row, err := f.j.Get(t.Context(), u.ID); err != nil || row.State != journal.StateCancelled {
		t.Fatalf("row changed: %+v %v", row, err)
	}
}
