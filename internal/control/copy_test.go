package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCopyControlUsesLiveJournalAndRejectsClobber(t *testing.T) {
	f, _ := cacheControl(t)
	f.coll.FS.SetWriteBackend(f.j, nil)
	ctx := context.Background()
	socket := socketPath(t)
	srv, err := NewServer(f.coll).Start(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	q := CopyRequest{From: "/docs/a", To: "/docs/b"}
	out, online, err := CallCopy(ctx, socket, "", q)
	if err != nil || !online || out.File.Size != 14 {
		t.Fatalf("copy: %+v %v %v", out, online, err)
	}
	data, err := f.coll.FS.ReadFileRange(ctx, q.To, 0, 14)
	if err != nil || string(data) != "cached content" {
		t.Fatalf("read: %q %v", data, err)
	}
	if _, online, err := CallCopy(ctx, socket, "", q); !online || err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("clobber: %v %v", online, err)
	}
	rows, _, err := f.j.ListActive(ctx, "", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queue: %v %v", rows, err)
	}
}

func TestCopyControlRejectsInvalidAndBrowserRequests(t *testing.T) {
	f, p := cacheControl(t)
	f.coll.FS.SetWriteBackend(f.j, nil)
	s := NewServer(f.coll)
	for _, tc := range []struct {
		name, method, body, origin, header, host, content string
		status                                            int
	}{
		{"origin", "POST", `{"from":"/docs/a","to":"/docs/b"}`, "https://bad.invalid", "1", "cloudfs", "application/json", 403},
		{"header", "POST", `{}`, "", "", "cloudfs", "application/json", 403},
		{"host", "POST", `{}`, "", "1", "bad.invalid", "application/json", 403},
		{"method", "GET", `{}`, "", "1", "cloudfs", "application/json", 405},
		{"type", "POST", `{}`, "", "1", "cloudfs", "text/plain", 415},
		{"unknown", "POST", `{"from":"/docs/a","to":"/docs/b","overwrite":true}`, "", "1", "cloudfs", "application/json", 400},
		{"trailing", "POST", `{"from":"/docs/a","to":"/docs/b"}{}`, "", "1", "cloudfs", "application/json", 400},
		{"relative", "POST", `{"from":"docs/a","to":"/docs/b"}`, "", "1", "cloudfs", "application/json", 400},
		{"traversal", "POST", `{"from":"/docs/../a","to":"/docs/b"}`, "", "1", "cloudfs", "application/json", 400},
		{"null", "POST", `null`, "", "1", "cloudfs", "application/json", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://"+tc.host+"/copy", strings.NewReader(tc.body))
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("X-CloudFS-Control", tc.header)
			r.Header.Set("Content-Type", tc.content)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	if p.TotalCalls() != 0 {
		t.Fatal("rejected request contacted provider")
	}
}

func TestCopyControlDoesNotReplayLostResponse(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer s.Close()
	_, online, err := CallCopy(context.Background(), "", strings.TrimPrefix(s.URL, "http://"), CopyRequest{From: "/a", To: "/b"})
	if err == nil || !online || calls.Load() != 1 {
		t.Fatalf("online=%v err=%v calls=%d", online, err, calls.Load())
	}
}
