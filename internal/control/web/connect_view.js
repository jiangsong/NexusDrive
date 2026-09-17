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

// bridgeBanner returns the stdio→HTTP bridge banner for the same answer, or
// null when there is nothing to say: no stdio server beside the owner
// (state n/a), or an answer from a daemon that predates the field. A
// connected bridge is a green line under the stdio warning — the writes of
// that server do reach the mount — with the number of sessions using it;
// a disabled one is a yellow line whose reason names what to change.
export function bridgeBanner(c) {
  const b = c && c.bridge;
  if (!b || typeof b.state !== 'string') return null;
  if (b.state === 'connected') {
    return { cls: 'ok', key: 'connect.bridge.connected', arg: Number(b.sessions) || 0 };
  }
  if (b.state === 'disabled') {
    const reasons = { http_off: 'connect.bridge.http_off', not_loopback: 'connect.bridge.not_loopback', no_token: 'connect.bridge.no_token' };
    return { cls: 'warn', key: 'connect.bridge.disabled', reasonKey: reasons[b.reason] || 'connect.bridge.unknown' };
  }
  return null;
}
