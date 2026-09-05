package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCacheDropEndpoint(t *testing.T) {
	calls := 0
	c := &Collector{DropCaches: func(context.Context) (int, error) { calls++; return 7, nil }}
	srv := httptest.NewServer(NewServer(c).Handler())
	defer srv.Close()

	// It changes state, so GET is refused.
	resp, err := http.Get(srv.URL + "/cache/drop")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || calls != 0 {
		t.Fatalf("GET = %s, calls = %d", resp.Status, calls)
	}
	resp, err = http.Post(srv.URL+"/cache/drop", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls != 1 {
		t.Fatalf("POST = %s, calls = %d", resp.Status, calls)
	}
	var body [64]byte
	n, _ := resp.Body.Read(body[:])
	if !strings.Contains(string(body[:n]), `"files_dropped":7`) {
		t.Fatalf("body = %s", body[:n])
	}

	// A daemon without the hook says so instead of pretending.
	unwired := httptest.NewServer(NewServer(&Collector{}).Handler())
	defer unwired.Close()
	resp, _ = http.Post(unwired.URL+"/cache/drop", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("unwired = %s", resp.Status)
	}

	failing := httptest.NewServer(NewServer(&Collector{
		DropCaches: func(context.Context) (int, error) { return 0, errors.New("disk on fire") },
	}).Handler())
	defer failing.Close()
	resp, _ = http.Post(failing.URL+"/cache/drop", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failing = %s", resp.Status)
	}
}
