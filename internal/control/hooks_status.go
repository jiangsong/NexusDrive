package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/hooks"
	"cloudfs/internal/integration"
)

// GET /agent/hooks is the status model for the connect panel. Mutations live
// under /agent/integration/ and use the same managed installer as the CLI.

// HooksStatusResponse is GET /agent/hooks.
type HooksStatusResponse struct {
	Integration   []integration.Status `json:"integration"`
	MCPConnection string               `json:"mcp_connection"`
	Directory     string               `json:"directory"`
	Memory        string               `json:"memory"`
	// Context is hooks.context: off | minimal | full.
	Context string         `json:"context"`
	Clients []hooks.Status `json:"clients"`
	// Detected lists the clients whose config directory exists.
	Detected []string `json:"detected"`
	// MountsRegistry is the file the pure-shell guard consults, with how
	// many mounts it lists right now.
	MountsRegistry string `json:"mounts_registry"`
	Mounts         int    `json:"mounts"`
}

func (s *Server) agentHooks(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) || !allowMethod(w, r, http.MethodGet) {
		return
	}
	resp := HooksStatusResponse{Context: "minimal", Clients: []hooks.Status{}, Detected: []string{}}
	cfg := s.collector.ConfigView()
	if cfg != nil && cfg.Hooks.Context != "" {
		resp.Context = cfg.Hooks.Context
	}
	home, err := os.UserHomeDir()
	if err != nil {
		writeJSON(w, resp)
		return
	}
	clients := []string{"codex", "claude"}
	resp.Integration = make([]integration.Status, len(clients))
	var wg sync.WaitGroup
	for i, client := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp.Integration[i] = s.integrationStatus(r.Context(), home, client)
		}()
	}
	wg.Wait()
	resp.MCPConnection = "unverified; run cloudfs agent status"
	if s.collector.MCP == nil || !s.collector.MCP.Connect(r.Context()).HTTPListening {
		resp.MCPConnection = "not listening"
	} else {
		resp.MCPConnection = "listener ready; per-client authentication shown below"
	}
	resp.Directory = "unavailable"
	if cfg != nil && len(cfg.Mounts) > 0 {
		resp.Directory = fmt.Sprintf("ready for %d configured mount(s); verified when a client session starts", len(cfg.Mounts))
	}
	resp.Memory = "unavailable"
	if s.collector.Memory != nil && s.collector.Memory.Root() != "" {
		resp.Memory = "configured; client authorization checked by cloudfs agent status"
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

type agentIntegrationRequest struct {
	Clients []string `json:"clients"`
	Confirm bool     `json:"confirm,omitempty"`
}

type agentIntegrationResult struct {
	Action          string               `json:"action"`
	Clients         []integration.Status `json:"clients"`
	RestartRequired bool                 `json:"restart_required,omitempty"`
}

func integrationClients(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("select at least one client")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, client := range in {
		client = strings.ToLower(strings.TrimSpace(client))
		if (client != "codex" && client != "claude") || seen[client] {
			return nil, fmt.Errorf("invalid or repeated client %q", client)
		}
		seen[client] = true
		out = append(out, client)
	}
	return out, nil
}

// agentIntegration executes an explicit UI action and never returns a clear
// credential. The installer writes it directly to the local protected file.
func (s *Server) agentIntegration(w http.ResponseWriter, r *http.Request) {
	if !privateRequest(w, r) || !allowMethod(w, r, http.MethodPost) {
		return
	}
	action := strings.Trim(strings.TrimPrefix(r.URL.Path, "/agent/integration/"), "/")
	if action != "install" && action != "uninstall" && action != "enable-http" {
		http.NotFound(w, r)
		return
	}
	var in agentIntegrationRequest
	if !decodeMutation(w, r, &in) {
		return
	}
	if action == "enable-http" {
		cfg := s.collector.ConfigView()
		if cfg == nil || cfg.SourcePath == "" {
			http.Error(w, "the running owner has no editable configuration", http.StatusServiceUnavailable)
			return
		}
		if err := config.SetMCPHTTP(cfg.SourcePath, "127.0.0.1:8765"); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		s.reloadConfigView()
		writeJSON(w, agentIntegrationResult{Action: action, RestartRequired: true})
		return
	}
	clients, err := integrationClients(in.Clients)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if action == "uninstall" && !in.Confirm {
		http.Error(w, "confirm=true is required to uninstall agent integration", http.StatusBadRequest)
		return
	}
	cfg := s.collector.ConfigView()
	if cfg == nil || s.collector.MCP == nil {
		http.Error(w, "agent integration requires the running owner daemon", http.StatusServiceUnavailable)
		return
	}
	conn := s.collector.MCP.Connect(r.Context())
	if action == "install" && !conn.HTTPListening {
		http.Error(w, "enable mcp.http before installing agent integration", http.StatusConflict)
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	binary, err := os.Executable()
	if err != nil || binary == "" {
		binary = "cloudfs"
	}
	for _, client := range clients {
		if action == "install" {
			err = integration.InstallClient(r.Context(), home, client, s.collector.Version, binary, conn.URL, cfg, s.collector.MCP)
		} else {
			err = integration.UninstallClient(r.Context(), home, client, s.collector.Version, cfg, s.collector.MCP)
		}
		if err != nil {
			http.Error(w, client+": "+err.Error(), http.StatusConflict)
			return
		}
	}
	if action == "install" {
		mounts := make([]string, 0, len(cfg.Mounts))
		for _, mount := range cfg.Mounts {
			mounts = append(mounts, filepath.Clean(mount.Path))
		}
		if err := hooks.WriteMounts(hooks.MountsPath(home), mounts); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	result := agentIntegrationResult{Action: action, Clients: make([]integration.Status, 0, len(clients))}
	for _, client := range clients {
		result.Clients = append(result.Clients, s.integrationStatus(r.Context(), home, client))
	}
	writeJSON(w, result)
}

func (s *Server) integrationStatus(ctx context.Context, home, client string) integration.Status {
	st := integration.Inspect(home, client)
	if _, err := exec.LookPath(client); err == nil {
		st.Present = true
		versionCtx, cancel := context.WithTimeout(ctx, time.Second)
		if output, err := exec.CommandContext(versionCtx, client, "--version").Output(); err == nil {
			st.ClientVersion = strings.TrimSpace(string(output))
		}
		cancel()
	} else if _, err := os.Stat(filepath.Join(home, "."+client)); err == nil {
		st.Present = true
	}
	if s.collector.MCP == nil {
		st.MCPConnection, st.Memory = "owner unavailable", "unavailable"
		return st
	}
	conn := s.collector.MCP.Connect(ctx)
	if !conn.HTTPListening {
		st.MCPConnection, st.Memory = "listener disabled", "unavailable"
		return st
	}
	if st.MCPConfigured {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		st.MCPConnection, st.Memory = integration.ProbeClient(probeCtx, home, client, conn.URL)
	} else {
		st.MCPConnection, st.Memory = "not configured", "unavailable"
	}
	return st
}
