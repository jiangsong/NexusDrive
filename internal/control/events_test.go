package control

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestEventsStreamChangesAndStatus: a page holding /events open hears about a
// directory it is showing changing, without polling for it, and receives the
// status document on a schedule. The connection is the daemon's, so a slow
// page cannot block the filesystem — that is the VFS feed's own guarantee, and
// this only has to not break it.
func TestEventsStreamChangesAndStatus(t *testing.T) {
	f, _ := fsControl(t)
	srv := httptest.NewServer(NewServer(f.coll).Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events: %s %s", resp.Status, resp.Header.Get("Content-Type"))
	}
	events := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var event string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				events <- event + " " + strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	first := <-events
	if !strings.HasPrefix(first, "status ") || !strings.Contains(first, `"version":"test"`) {
		t.Fatalf("first event should be the status document: %s", first)
	}

	// A change on the filesystem reaches the page as a change event naming
	// the path, and nothing about the provider.
	docs, err := f.coll.FS.StatPath(context.Background(), "/docs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.coll.FS.Mkdir(context.Background(), docs.Ino, "live"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if strings.HasPrefix(ev, "change ") {
				if !strings.Contains(ev, `"/docs/live"`) && !strings.Contains(ev, `"/docs"`) && !strings.Contains(ev, `"rescan":true`) {
					t.Fatalf("change event names neither the path nor a rescan: %s", ev)
				}
				return
			}
		case <-deadline:
			t.Fatal("no change event arrived for a write")
		}
	}
}

// TestEventsRefuseCrossSite: a stream that reports every path a user touches
// takes the same guard as everything else.
func TestEventsRefuseCrossSite(t *testing.T) {
	f, _ := fsControl(t)
	s := NewServer(f.coll)
	r := httptest.NewRequest(http.MethodGet, "http://cloudfs/events", nil)
	r.Header.Set("Origin", "https://evil.invalid")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("cross-site events: %d", w.Code)
	}
}
