package control

import (
	"net/http"
	"os"

	"cloudfs/internal/hooks"
)

// GET /agent/hooks is the Hooks card of the connect panel (ui-plan G7-1):
// each client's registration under the daemon's own home directory, the
// context mode in force, and the commands that would install or remove
// the hooks. It only reads: the browser cannot define what runs on this
// machine, so the route hands back commands to copy rather than a button
// that edits ~/.claude/settings.json — TestHooksRouteNeverWritesUserConfig
// holds it to that.

// HooksStatusResponse is GET /agent/hooks.
type HooksStatusResponse struct {
	// Context is hooks.context: off | minimal | full.
	Context string         `json:"context"`
	Clients []hooks.Status `json:"clients"`
	// Detected lists the clients whose config directory exists.
	Detected         []string `json:"detected"`
	InstallCommand   string   `json:"install_command"`
	UninstallCommand string   `json:"uninstall_command"`
	// MountsRegistry is the file the pure-shell guard consults, with how
	// many mounts it lists right now.
	MountsRegistry string `json:"mounts_registry"`
	Mounts         int    `json:"mounts"`
}

func (s *Server) agentHooks(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) || !allowMethod(w, r, http.MethodGet) {
		return
	}
	resp := HooksStatusResponse{Context: "minimal", Clients: []hooks.Status{}, Detected: []string{},
		InstallCommand: "cloudfs hooks install", UninstallCommand: "cloudfs hooks uninstall"}
	if cfg := s.collector.ConfigView(); cfg != nil && cfg.Hooks.Context != "" {
		resp.Context = cfg.Hooks.Context
	}
	home, err := os.UserHomeDir()
	if err != nil {
		writeJSON(w, resp)
		return
	}
	resp.Clients = hooks.Statuses(home)
	if d := hooks.Detect(home); d != nil {
		resp.Detected = d
	}
	resp.MountsRegistry = hooks.MountsPath(home)
	if mounts, err := hooks.ReadMounts(resp.MountsRegistry); err == nil {
		resp.Mounts = len(mounts)
	}
	writeJSON(w, resp)
}
