// The connect panel's decisions, with no DOM in them, so they can be checked
// under node: whether the stdio warning is shown, and where it links. The
// panel renders the returned keys through t().

// stdioWarning returns the banner for GET /mcp/connect's answer, or null
// when nothing is running beside the owner. The banner is a fact about the
// daemon (a heartbeat under the agent directory), never about the console,
// so it is rendered on nothing but stdio_non_owner.
export function stdioWarning(c) {
  if (!c || c.stdio_non_owner !== true) return null;
  return { key: 'connect.stdio.banner', linkKey: 'connect.stdio.link', href: '#/diagnostics' };
}
