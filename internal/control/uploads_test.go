package control

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloudfs/internal/journal"
	"cloudfs/internal/upload"

	"cloudfs/internal/i18n"
)

func TestUploadControlPagesAndDoesNotExposeSessions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ids := map[string]bool{}
	for _, name := range []string{"a", "b", "c"} {
		u := f.queue(t, name, 10)
		ids[u.ID] = true
		if err := f.j.SetSession(ctx, u.ID, map[string]string{"access_token": "never-show-this-token"}); err != nil {
			t.Fatal(err)
		}
	}
	p := socketPath(t)
	r, err := NewServer(f.coll).Start(ctx, p, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	q := UploadRequest{Action: "list", Limit: 1}
	for {
		out, online, err := CallUploads(ctx, p, "", q)
		if err != nil || !online || len(out.Uploads) != 1 {
			t.Fatalf("list = %+v, %v, %v", out, online, err)
		}
		u := out.Uploads[0]
		if !ids[u.ID] {
			t.Fatalf("duplicate/unknown upload %s", u.ID)
		}
		delete(ids, u.ID)
		b, _ := json.Marshal(out)
		if strings.Contains(string(b), "never-show") || strings.Contains(string(b), "BlobPath") || strings.Contains(string(b), "Session") {
			t.Fatal("private upload state exposed")
		}
		if out.NextCursor == "" {
			break
		}
		q.Cursor = out.NextCursor
	}
	if len(ids) != 0 {
		t.Fatalf("missed uploads: %v", ids)
	}
}

func TestUploadControlReportsPublicationBarrier(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.queue(t, "pending-publication", 10)
	u.NeedsPublish = true
	if err := f.j.Commit(ctx, u); err != nil {
		t.Fatal(err)
	}
	out, err := ManageUploads(ctx, f.j, nil, nil, UploadRequest{Action: "list"})
	if err != nil || len(out.Uploads) != 1 || !out.Uploads[0].NeedsPublish {
		t.Fatalf("barrier not visible: %+v %v", out, err)
	}
	encoded, err := json.Marshal(out)
	if err != nil || !strings.Contains(string(encoded), `"needs_publish":true`) {
		t.Fatalf("JSON: %s %v", encoded, err)
	}
	if strings.Contains(string(encoded), u.BlobPath) {
		t.Fatal("publication status leaked blob path")
	}
}

func TestUploadControlRetryGuard(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.queue(t, "a", 10)
	p := socketPath(t)
	r, err := NewServer(f.coll).Start(ctx, p, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	q := UploadRequest{Action: "retry", ID: u.ID}
	if _, online, err := CallUploads(ctx, p, "", q); !online || err == nil {
		t.Fatalf("retry pending = %v, %v", online, err)
	}
	if err := f.j.Fail(ctx, u.ID, errors.New("denied")); err != nil {
		t.Fatal(err)
	}
	out, online, err := CallUploads(ctx, p, "", q)
	if err != nil || !online || out.Requeued != 1 {
		t.Fatalf("retry = %+v, %v, %v", out, online, err)
	}
	if _, err := f.j.Claim(ctx, "ali", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CallUploads(ctx, p, "", q); err == nil {
		t.Fatal("requeued active upload")
	}
	row, err := f.j.Get(ctx, u.ID)
	if err != nil || row.State != journal.StateUploading {
		t.Fatalf("row = %+v, %v", row, err)
	}
}

func TestUploadControlRejectsBrowserAndMalformedRequests(t *testing.T) {
	f := newFixture(t)
	u := f.queue(t, "a", 10)
	if err := f.j.Fail(context.Background(), u.ID, errors.New("denied")); err != nil {
		t.Fatal(err)
	}
	s := NewServer(f.coll)
	for _, tc := range []struct {
		name, method, path, body, host, origin, header string
		status                                         int
	}{
		{"origin", "POST", "/uploads/retry", `{"all":true}`, "cloudfs", "https://evil.example", "1", 403},
		{"cancel origin", "POST", "/uploads/cancel", `{"id":"` + u.ID + `"}`, "cloudfs", "https://evil.example", "1", 403},
		{"cancel get", "GET", "/uploads/cancel", "", "cloudfs", "", "", 405},
		{"cancel missing selector", "POST", "/uploads/cancel", `{}`, "cloudfs", "", "1", 400},
		{"cancel ambiguous selector", "POST", "/uploads/cancel", `{"id":"` + u.ID + `","all":true}`, "cloudfs", "", "1", 400},
		{"dns", "POST", "/uploads/retry", `{"all":true}`, "evil.example", "", "1", 403},
		{"simple form", "POST", "/uploads/retry", `{"all":true}`, "cloudfs", "", "", 403},
		{"get mutation", "GET", "/uploads/retry", ``, "cloudfs", "", "", 405},
		{"unknown field", "POST", "/uploads/retry", `{"all":true,"extra":1}`, "cloudfs", "", "1", 400},
		{"ambiguous selector", "POST", "/uploads/retry", `{"all":true,"id":"a"}`, "cloudfs", "", "1", 400},
		{"missing selector", "POST", "/uploads/retry", `{}`, "cloudfs", "", "1", 400},
		{"trailing object", "POST", "/uploads/retry", `{"all":true}{}`, "cloudfs", "", "1", 400},
		{"large body", "POST", "/uploads/retry", `{"id":"` + strings.Repeat("a", 5000) + `"}`, "cloudfs", "", "1", 400},
		{"large page", "GET", "/uploads?limit=1001", ``, "cloudfs", "", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, strings.NewReader(tc.body))
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("X-CloudFS-Control", tc.header)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
	row, _ := f.j.Get(context.Background(), u.ID)
	if row.State != journal.StateDead {
		t.Fatal("rejected request changed queue")
	}
}

func TestUploadControlAllowsSameOriginUIRequest(t *testing.T) {
	f := newFixture(t)
	u := f.queue(t, "a", 10)
	if err := f.j.Fail(context.Background(), u.ID, errors.New("denied")); err != nil {
		t.Fatal(err)
	}
	s := NewServer(f.coll)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9101/uploads/retry", strings.NewReader(`{"id":"`+u.ID+`"}`))
	req.Header.Set("Origin", "http://127.0.0.1:9101")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-CloudFS-Control", "1")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("same-origin status %d: %s", w.Code, w.Body.String())
	}
	row, err := f.j.Get(context.Background(), u.ID)
	if err != nil || row.State != journal.StatePending {
		t.Fatalf("row = %+v, %v", row, err)
	}
}

func TestUploadCancelControlReturnsRetainedState(t *testing.T) {
	f := newFixture(t)
	u := f.queue(t, "cancel.txt", 10)
	f.coll.CancelUpload = f.j.RequestCancel // idle fixture: no worker is running
	ctx := context.Background()
	p := socketPath(t)
	s, err := NewServer(f.coll).Start(ctx, p, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out, online, err := CallUploads(ctx, p, "", UploadRequest{Action: "cancel", ID: u.ID})
	if err != nil || !online || out.State != journal.StateCancelled || !strings.Contains(out.Warning, "does not undo") {
		t.Fatalf("cancel: %+v %v %v", out, online, err)
	}
	if _, online, err := CallUploads(ctx, p, "", UploadRequest{Action: "retry", ID: u.ID}); !online || err == nil {
		t.Fatalf("cancelled retry bypass: %v %v", online, err)
	}
	st := f.coll.Collect(ctx, i18n.EN)
	if st.Uploads.Cancelled != 1 || st.Uploads.RetainedBytes != 10 || len(st.Warnings) == 0 {
		t.Fatalf("status hides retained task: %+v", st)
	}
}

func TestUploadFlushClientCancellationEndsWait(t *testing.T) {
	f := newFixture(t)
	entered, exited := make(chan struct{}), make(chan struct{})
	f.coll.FlushUploads = func(ctx context.Context) (journal.Stats, error) {
		close(entered)
		defer close(exited)
		<-ctx.Done()
		return journal.Stats{}, ctx.Err()
	}
	p := socketPath(t)
	r, err := NewServer(f.coll).Start(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := CallUploads(ctx, p, "", UploadRequest{Action: "flush"}); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("handler did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("handler leaked after disconnect")
	}
}

func TestUploadFlushReportsDeadLettersOverHTTP(t *testing.T) {
	f := newFixture(t)
	f.coll.FlushUploads = func(context.Context) (journal.Stats, error) { return journal.Stats{Dead: 1}, upload.ErrDeadLetters }
	p := socketPath(t)
	r, err := NewServer(f.coll).Start(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, online, err := CallUploads(context.Background(), p, "", UploadRequest{Action: "flush"}); !online || err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("flush = %v, %v", online, err)
	}
}

func TestUploadClientNeverReplaysALostMutationResponse(t *testing.T) {
	p := socketPath(t)
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})}
	go s.Serve(l)
	defer s.Close()
	var calls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.Write([]byte(`{}`)) }))
	defer fallback.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, q := range []UploadRequest{{Action: "retry", All: true}, {Action: "cancel", ID: journal.NewID()}, {Action: "resume", ID: journal.NewID(), Confirm: true}, {Action: "drop", ID: journal.NewID(), Confirm: true}} {
		_, online, err := CallUploads(ctx, p, strings.TrimPrefix(fallback.URL, "http://"), q)
		if !online || err == nil || calls.Load() != 0 {
			t.Fatalf("request replayed: online %v err %v calls %d", online, err, calls.Load())
		}
	}
}

func TestUploadReadOnlyJournalRefusesMutation(t *testing.T) {
	f := newFixture(t)
	u := f.queue(t, "a", 10)
	if err := f.j.Fail(context.Background(), u.ID, errors.New("denied")); err != nil {
		t.Fatal(err)
	}
	ro, err := journal.OpenReadOnly(f.dir + "/journal")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ManageUploads(context.Background(), ro, nil, nil, UploadRequest{Action: "retry", All: true}); err == nil {
		t.Fatal("nonowner mutated queue")
	}
}
