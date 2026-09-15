package daemon

import (
	"sync"

	"cloudfs/internal/agent"
	"cloudfs/internal/control"
)

// agentView adapts the agent store to the control plane. It keeps a missing
// store a nil interface, the way Export does: the audit and session routes
// answer 503 on the strength of that field alone.
func (d *Daemon) agentView() control.AgentView {
	if d.Agent == nil || d.Sessions == nil {
		return nil
	}
	return control.NewAgentView(d.Agent, d.Sessions, d.Workspace(), control.RollbackDeps{FS: agent.VFSOps(d.FS), Preimages: d.Preimages})
}

// Workspace is the directory MCP sessions deliver into: mcp.workspace, or
// the first allow prefix plus "/.agent" when the file sets none. It is ""
// when neither gives one, which is what /status shows and what
// begin_session reports as a configuration error.
func (d *Daemon) Workspace() string {
	ws, err := agent.DefaultWorkspace(d.Config.MCP.Workspace, agent.Scope{Read: d.Config.MCP.Allow})
	if err != nil {
		return ""
	}
	return ws
}

// mcpHTTP is what this process knows about its MCP HTTP transport. The
// collector is built before the listener starts, so the view asks for it
// on every request rather than copying it once.
type mcpHTTP struct {
	mu       sync.Mutex
	addr     string
	envToken bool
	render   control.MCPSnippetRenderer
}

// SetMCPHTTP records that the MCP HTTP transport is served on addr, or no
// longer served when addr is "". envToken says CLOUDFS_MCP_TOKEN is set;
// the daemon does not read the environment itself.
func (d *Daemon) SetMCPHTTP(addr string, envToken bool) {
	d.mcp.mu.Lock()
	defer d.mcp.mu.Unlock()
	d.mcp.addr, d.mcp.envToken = addr, envToken
}

// SetMCPSnippets injects the renderer of client registration snippets.
// cmd/cloudfs supplies it from the MCP adapter, so this package never
// imports mcpsrv.
func (d *Daemon) SetMCPSnippets(render control.MCPSnippetRenderer) {
	d.mcp.mu.Lock()
	defer d.mcp.mu.Unlock()
	d.mcp.render = render
}

func (d *Daemon) mcpHTTPState() control.MCPHTTPState {
	d.mcp.mu.Lock()
	defer d.mcp.mu.Unlock()
	return control.MCPHTTPState{Addr: d.mcp.addr, EnvToken: d.mcp.envToken, Owner: d.Journal != nil && d.Journal.Owner()}
}

// mcpSnippets renders through whatever SetMCPSnippets installed, looked up
// per call so an injection after Collector() still counts.
func (d *Daemon) mcpSnippets(url string) (map[string]string, map[string]string) {
	d.mcp.mu.Lock()
	render := d.mcp.render
	d.mcp.mu.Unlock()
	if render == nil {
		return nil, nil
	}
	return render(url)
}

// mcpView adapts the agent store to the connection routes; nil without a
// store, so /mcp/connect and /mcp/tokens answer 503.
func (d *Daemon) mcpView() control.MCPView {
	if d.Agent == nil {
		return nil
	}
	return control.NewMCPView(d.Agent, d.mcpHTTPState, d.mcpSnippets)
}
