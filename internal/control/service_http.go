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
		http.Error(w, "this daemon does not manage a service", http.StatusNotImplemented)
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
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
			http.Error(w, "cannot determine service state", http.StatusInternalServerError)
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
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
			http.Error(w, "cannot determine service state", http.StatusInternalServerError)
			return
		}
		if installed {
			// Refuse rather than silently re-enable a service the user may
			// have deliberately stopped.
			http.Error(w, "the service is already installed; uninstall it first to reinstall", http.StatusConflict)
			return
		}
	}
	if svc.Install == nil {
		http.Error(w, "this daemon cannot install a service", http.StatusNotImplemented)
		return
	}
	if err := svc.Install(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, ServiceActionResponse{Installed: true})
}

func (s *Server) serviceUninstall(w http.ResponseWriter, r *http.Request, svc *ServiceControl) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	confirm := r.URL.Query().Get("confirm") == "true"
	if err := requireConfirm(confirm, "stops the service and detaches its mount"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if svc.Uninstall == nil {
		http.Error(w, "this daemon cannot uninstall a service", http.StatusNotImplemented)
		return
	}
	if err := svc.Uninstall(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, ServiceActionResponse{Installed: false})
}
