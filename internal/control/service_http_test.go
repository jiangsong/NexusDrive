package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func serviceRequest(t *testing.T, srv *Server, method, target string) *httptest.ResponseRecorder {
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

func TestServiceStatusInstallUninstall(t *testing.T) {
	installed := false
	svc := &ServiceControl{
		Supported: func() (bool, string) { return true, "" },
		Installed: func() (bool, error) { return installed, nil },
		Status:    func() (string, error) { return "state = running", nil },
		Install:   func() error { installed = true; return nil },
		Uninstall: func() error { installed = false; return nil },
	}
	srv := NewServer(&Collector{Version: "svc-test", Service: svc})

	rr := serviceRequest(t, srv, http.MethodGet, "/service/status")
	var st ServiceStatusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Supported || st.Installed {
		t.Fatalf("status before install = %+v", st)
	}

	rr = serviceRequest(t, srv, http.MethodPost, "/service/install")
	if rr.Code != http.StatusOK || !installed {
		t.Fatalf("install = %d installed=%v", rr.Code, installed)
	}

	// A second install is refused rather than silently re-enabling.
	rr = serviceRequest(t, srv, http.MethodPost, "/service/install")
	if rr.Code != http.StatusConflict {
		t.Fatalf("second install = %d %s", rr.Code, rr.Body)
	}

	// Uninstall needs confirm.
	rr = serviceRequest(t, srv, http.MethodPost, "/service/uninstall")
	if rr.Code != http.StatusBadRequest || !installed {
		t.Fatalf("uninstall without confirm = %d installed=%v", rr.Code, installed)
	}
	rr = serviceRequest(t, srv, http.MethodPost, "/service/uninstall?confirm=true")
	if rr.Code != http.StatusOK || installed {
		t.Fatalf("uninstall = %d installed=%v", rr.Code, installed)
	}

	// Detail is shown only when installed.
	rr = serviceRequest(t, srv, http.MethodPost, "/service/install")
	if rr.Code != http.StatusOK {
		t.Fatalf("reinstall = %d", rr.Code)
	}
	rr = serviceRequest(t, srv, http.MethodGet, "/service/status")
	_ = json.Unmarshal(rr.Body.Bytes(), &st)
	if !st.Installed || st.Detail != "state = running" {
		t.Fatalf("status after reinstall = %+v", st)
	}
}

func TestServiceUnsupportedPlatform(t *testing.T) {
	svc := &ServiceControl{
		Supported: func() (bool, string) { return false, "windows has no per-user service integration" },
		Installed: func() (bool, error) { return false, errors.New("should not be called") },
	}
	srv := NewServer(&Collector{Version: "svc-unsup", Service: svc})
	rr := serviceRequest(t, srv, http.MethodGet, "/service/status")
	var st ServiceStatusResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &st)
	if st.Supported || st.Reason == "" {
		t.Fatalf("unsupported status = %+v", st)
	}
	rr = serviceRequest(t, srv, http.MethodPost, "/service/install")
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("install on unsupported = %d", rr.Code)
	}
}

func TestServiceUnavailableWhenNotWired(t *testing.T) {
	srv := NewServer(&Collector{Version: "no-svc"})
	rr := serviceRequest(t, srv, http.MethodGet, "/service/status")
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("service status without a controller = %d", rr.Code)
	}
}
