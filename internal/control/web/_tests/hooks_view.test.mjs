import test from 'node:test';
import assert from 'node:assert/strict';

import { clientRow } from '../hooks_view.js';

test('a client row states installed, absent or missing, and whether the shape is verified', () => {
  assert.equal(clientRow({ client: 'claude', present: true, installed: true, verified: true }).state, 'installed');
  assert.equal(clientRow({ client: 'codex', present: true, installed: false, verified: false }).state, 'absent');
  assert.equal(clientRow({ client: 'gemini', present: false, installed: false }).state, 'missing');
  const r = clientRow({ client: 'codex', present: true, installed: true, note: 'enable hooks' });
  assert.equal(r.verifiedKey, 'hooks.unverified');
  assert.equal(r.note, 'enable hooks');
  assert.equal(clientRow(null).state, 'missing');
});
