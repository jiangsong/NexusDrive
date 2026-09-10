package control

import (
	"net/http"
)

// The service endpoints let the settings page install or remove the per-user
// mount supervisor without a terminal. The running daemon can manage its own
// service because internal/service is injected-and-testable and holds the one
// ordering invariant (stop, unmount, then remove) in a single place. The
// daemon may install and uninstall as well as report status: a NAS user who
// reached the dashboard has already shown they can reach the machine.

// ServiceControl is the seam to internal/service. It is a struct of funcs, like
// AuthStarter, so a control test drives it without a real service manager and
// so this package need not construct a service.Runtime (which would shell out).
// nil means this daemon does not offer service management.
type ServiceControl struct {
	// Supported reports whether this platform has a service manager, and why
	// not when it does not.
	Supported func() (bool, string)
	Installed func() (bool, error)
	// Status returns the manager's own status text.
	Status func() (string, error)
	// Install writes and enables the definition; Uninstall stops it, detaches
	// the mount, and removes the definition.
	Install   func() error
	Uninstall func() error
}

// ServiceStatusResponse describes the current service state.
type ServiceStatusResponse struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	Installed bool   `json:"installed"`
	// Detail is the manager's own status text, when it could be read.
	Detail string `json:"detail,omitempty"`
}

// ServiceActionResponse is the reply to install/uninstall.
type ServiceActionResponse struct {
	Installed bool `json:"installed"`
}

func (s *Server) service(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	svc := s.collector.Service
	if svc == nil {
		httpErrorT(w, r, http.StatusNotImplemented, "err.service_unmanaged")
		return
	}
	switch r.URL.Path {
	case "/service/status":
		s.serviceStatus(w, r, svc)
	case "/service/install":
		s.serviceInstall(w, r, svc)
	case "/service/uninstall":
		s.serviceUninstall(w, r, svc)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) serviceStatus(w http.ResponseWriter, r *http.Request, svc *ServiceControl) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	out := ServiceStatusResponse{Supported: true}
	if svc.Supported != nil {
		out.Supported, out.Reason = svc.Supported()
	}
	if !out.Supported {
		writeJSON(w, out)
		return
	}
	if svc.Installed != nil {
		installed, err := svc.Installed()
		if err != nil {
			httpErrorT(w, r, http.StatusInternalServerError, "err.service_state_unknown")
			return
		}
		out.Installed = installed
	}
	// The manager's status text is best-effort: a not-installed service makes
	// systemctl exit non-zero, which is not an error to report here.
	if out.Installed && svc.Status != nil {
		if detail, err := svc.Status(); err == nil {
			out.Detail = detail
		}
	}
	writeJSON(w, out)
}

func (s *Server) serviceInstall(w http.ResponseWriter, r *http.Request, svc *ServiceControl) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	if svc.Supported != nil {
		if ok, why := svc.Supported(); !ok {
			http.Error(w, why, http.StatusNotImplemented)
			return
		}
	}
	if svc.Installed != nil {
		installed, err := svc.Installed()
		if err != nil {
			httpErrorT(w, r, http.StatusInternalServerError, "err.service_state_unknown")
			return
		}
		if installed {
			// Refuse rather than silently re-enable a service the user may
			// have deliberately stopped.
			httpErrorT(w, r, http.StatusConflict, "err.service_installed")
			return
		}
	}
	if svc.Install == nil {
		httpErrorT(w, r, http.StatusNotImplemented, "err.service_install_unsupported")
		return
	}
	if err := svc.Install(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, ServiceActionResponse{Installed: true})
}

func (s *Server) serviceUninstall(w http.ResponseWriter, r *http.Request, svc *ServiceControl) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	confirm := r.URL.Query().Get("confirm") == "true"
	if !confirmed(w, r, confirm, "confirm.stop_service") {
		return
	}
	if svc.Uninstall == nil {
		httpErrorT(w, r, http.StatusNotImplemented, "err.service_uninstall_unsupported")
		return
	}
	if err := svc.Uninstall(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, ServiceActionResponse{Installed: false})
}
