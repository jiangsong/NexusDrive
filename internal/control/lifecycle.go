package control

import (
	"net/http"
)

// A restart is the one control action that ends the process. It exists because
// almost every configuration change — a new remote, an edited mount, a proxy
// section on a build without hot reload — needs the daemon to be rebuilt from
// its frozen structure, and asking the user to find a terminal to do it defeats
// the point of a settings page. The single invariant is that two owners of the
// storage and the mount must never coexist: the running process fully releases
// everything (journal lock, FUSE mount) before its successor starts. That
// ordering lives in the process's main goroutine; this handler only asks for it.

// RestartMode names how the process comes back. The daemon always re-executes
// itself in place: syscall.Exec replaces the process image after every resource
// is released, so there is exactly one owner of the journal and the mount at
// all times and the supervisor's PID never changes. A clean exit that relies on
// a supervisor to relaunch would instead depend on its restart policy and would
// leave the mount detached in the window before it fired.
type RestartMode string

// RestartReexec: the process re-executes itself once it has released everything.
const RestartReexec RestartMode = "reexec"

// Lifecycle is how the mounting process lets the control plane restart it. It
// is nil in a process that has nothing to restart (an offline management
// command), and the endpoint reports that plainly.
type Lifecycle struct {
	// Restart triggers the graceful restart. It is invoked once, after the
	// HTTP response has been written and flushed, and must return promptly:
	// the real work (drain uploads, unmount, close, re-exec) belongs to the
	// process's main goroutine, not to this request.
	Restart func()
}

// RestartResponse is the reply to an accepted restart request.
type RestartResponse struct {
	Mode RestartMode `json:"mode"`
	// Restarting is always true in a 200: the process is on its way down.
	Restarting bool `json:"restarting"`
}

func (s *Server) daemonRestart(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	life := s.collector.Lifecycle
	if life == nil || life.Restart == nil {
		http.Error(w, "this process cannot restart itself; restart it the way it was started", http.StatusNotImplemented)
		return
	}
	confirm := r.URL.Query().Get("confirm") == "true"
	if err := requireConfirm(confirm, "detaches the mount and restarts the daemon"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// From here the process is going down. Refuse further mutations so a
	// second request cannot start changing state a restart is about to drop.
	if !s.draining.CompareAndSwap(false, true) {
		http.Error(w, "a restart is already in progress", http.StatusConflict)
		return
	}
	writeJSON(w, RestartResponse{Mode: RestartReexec, Restarting: true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// The response is out and the socket can close under us now; hand the
	// teardown to the main goroutine.
	life.Restart()
}
