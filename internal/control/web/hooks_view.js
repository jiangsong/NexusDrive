// The Hooks card's row decision, kept DOM-free so it runs under node.

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
