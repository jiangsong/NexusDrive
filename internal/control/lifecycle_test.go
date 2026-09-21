package control

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRestartRefusesAWorkingDirectoryInsideTheMount(t *testing.T) {
	called := false
	srv := NewServer(&Collector{Version: "restart-test", Lifecycle: &Lifecycle{
		CheckRestart: func() error { return errors.New("pid 42 codex: /mnt/cloud/project") },
		Restart:      func() { called = true },
	}})

	rr := lifecycleRequest(t, srv, http.MethodPost, "/daemon/restart?confirm=true")
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "pid 42 codex") {
		t.Fatalf("busy-cwd restart = %d %s", rr.Code, rr.Body)
	}
	if called {
		t.Fatal("restart ran despite a cwd holder")
	}
	// A refused restart must not leave the control plane draining.
	if rr := lifecycleRequest(t, srv, http.MethodPost, "/cache/drop"); rr.Code == http.StatusServiceUnavailable {
		t.Fatal("refused restart left the server draining")
	}
}

func lifecycleRequest(t *testing.T, srv *Server, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = "127.0.0.1:9101"
	if method != http.MethodGet {
		req.Header.Set("X-CloudFS-Control", "1")
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestRestartDrainsAndReportsItsMode(t *testing.T) {
	var mu sync.Mutex
	called := 0
	srv := NewServer(&Collector{
		Version: "restart-test",
		Lifecycle: &Lifecycle{
			Restart: func() { mu.Lock(); called++; mu.Unlock() },
		},
	})

	// Without confirm the process is not torn down.
	rr := lifecycleRequest(t, srv, http.MethodPost, "/daemon/restart")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("restart without confirm = %d %s", rr.Code, rr.Body)
	}
	mu.Lock()
	if called != 0 {
		t.Fatal("restart ran before it was confirmed")
	}
	mu.Unlock()

	rr = lifecycleRequest(t, srv, http.MethodPost, "/daemon/restart?confirm=true")
	if rr.Code != http.StatusOK {
		t.Fatalf("confirmed restart = %d %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), string(RestartReexec)) {
		t.Fatalf("mode not reported: %s", rr.Body)
	}
	mu.Lock()
	if called != 1 {
		t.Fatalf("restart callback ran %d times", called)
	}
	mu.Unlock()

	// Draining now turns away further mutations, so nothing changes state the
	// teardown is about to drop.
	rr = lifecycleRequest(t, srv, http.MethodPost, "/cache/drop")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("mutation while draining = %d", rr.Code)
	}
	// A read still answers so the UI can show the daemon is going down.
	rr = lifecycleRequest(t, srv, http.MethodGet, "/status")
	if rr.Code != http.StatusOK {
		t.Fatalf("status while draining = %d", rr.Code)
	}
}

func TestRestartUnavailableWhenNotWired(t *testing.T) {
	srv := NewServer(&Collector{Version: "no-restart"})
	rr := lifecycleRequest(t, srv, http.MethodPost, "/daemon/restart?confirm=true")
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("restart without a lifecycle = %d %s", rr.Code, rr.Body)
	}
}

func TestRestartRefusesCrossSite(t *testing.T) {
	srv := NewServer(&Collector{Version: "guard", Lifecycle: &Lifecycle{Restart: func() {}}})
	req := httptest.NewRequest(http.MethodPost, "/daemon/restart?confirm=true", nil)
	req.Host = "127.0.0.1:9101" // no X-CloudFS-Control header
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("unmarked restart = %d", rr.Code)
	}
}
