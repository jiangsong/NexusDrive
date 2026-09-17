// The Hooks card's decisions, with no DOM in them, so they run under node:
// what each client row says about its registration, and which commands the
// card offers. The card renders the returned keys through t().

// clientRow turns one GET /agent/hooks client entry into the row the card
// draws: a state (installed | absent | missing — the client is not on this
// machine), whether the shape is verified, and the platform's note.
export function clientRow(c) {
  const s = c || {};
  const state = s.installed ? 'installed' : s.present ? 'absent' : 'missing';
  return {
    client: String(s.client || ''),
    path: String(s.path || ''),
    state,
    stateKey: 'hooks.state.' + state,
    verified: s.verified === true,
    verifiedKey: s.verified === true ? 'hooks.verified' : 'hooks.unverified',
    note: typeof s.note === 'string' ? s.note : '',
  };
}

// commands is what the card offers to copy: the install command for the
// detected clients (narrowed to them when some are not installed), and the
// uninstall command when anything is installed. Nothing here runs; the
// browser hands a person a line for their shell.
export function commands(r) {
  const h = r || {};
  const clients = Array.isArray(h.clients) ? h.clients : [];
  const detected = Array.isArray(h.detected) ? h.detected : [];
  const installed = clients.filter((c) => c && c.installed).map((c) => c.client);
  const todo = detected.filter((c) => !installed.includes(c));
  const out = [];
  if (todo.length) out.push({ key: 'hooks.cmd.install', text: (h.install_command || 'cloudfs hooks install') + (todo.length < detected.length ? ' --client ' + todo.join(',') : '') });
  else if (!detected.length) out.push({ key: 'hooks.cmd.install', text: h.install_command || 'cloudfs hooks install' });
  if (installed.length) out.push({ key: 'hooks.cmd.uninstall', text: h.uninstall_command || 'cloudfs hooks uninstall' });
  return out;
}
