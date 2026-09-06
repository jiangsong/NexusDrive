package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// post is what a native client sends: the mutation header a browser form
// cannot set.
func post(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-CloudFS-Control", "1")
	return http.DefaultClient.Do(req)
}

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
	resp, err = post(srv.URL + "/cache/drop")
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

	// A POST without the control header is what an HTML form on another
	// site can send. It must not empty the cache. This was the one mutating
	// route that forgot the guard.
	resp, err = http.Post(srv.URL+"/cache/drop", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || calls != 1 {
		t.Fatalf("form POST without the header = %s, calls = %d; a cross-site form can drop the cache", resp.Status, calls)
	}

	// A daemon without the hook says so instead of pretending.
	unwired := httptest.NewServer(NewServer(&Collector{}).Handler())
	defer unwired.Close()
	resp, _ = post(unwired.URL + "/cache/drop")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("unwired = %s", resp.Status)
	}

	failing := httptest.NewServer(NewServer(&Collector{
		DropCaches: func(context.Context) (int, error) { return 0, errors.New("disk on fire") },
	}).Handler())
	defer failing.Close()
	resp, _ = post(failing.URL + "/cache/drop")
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failing = %s", resp.Status)
	}
}
