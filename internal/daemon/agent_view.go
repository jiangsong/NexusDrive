package daemon

import "cloudfs/internal/control"

// agentView adapts the agent store to the control plane. It keeps a missing
// store a nil interface, the way Export does: the audit and session routes
// answer 503 on the strength of that field alone.
func (d *Daemon) agentView() control.AgentView {
	if d.Agent == nil || d.Sessions == nil {
		return nil
	}
	return control.NewAgentView(d.Agent, d.Sessions, "")
}
