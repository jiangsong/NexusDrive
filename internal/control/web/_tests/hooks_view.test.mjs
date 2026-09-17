import test from 'node:test';
import assert from 'node:assert/strict';

import { clientRow, commands } from '../hooks_view.js';

test('a client row states installed, absent or missing, and whether the shape is verified', () => {
  assert.equal(clientRow({ client: 'claude', present: true, installed: true, verified: true }).state, 'installed');
  assert.equal(clientRow({ client: 'codex', present: true, installed: false, verified: false }).state, 'absent');
  assert.equal(clientRow({ client: 'gemini', present: false, installed: false }).state, 'missing');
  const r = clientRow({ client: 'codex', present: true, installed: true, note: 'enable hooks' });
  assert.equal(r.verifiedKey, 'hooks.unverified');
  assert.equal(r.note, 'enable hooks');
  assert.equal(clientRow(null).state, 'missing');
});

test('the install command names the detected clients still to do, uninstall appears once anything is installed', () => {
  const r = { install_command: 'cloudfs hooks install', uninstall_command: 'cloudfs hooks uninstall', detected: ['claude', 'codex'],
    clients: [{ client: 'claude', installed: true }, { client: 'codex', installed: false }, { client: 'gemini', installed: false }] };
  assert.deepEqual(commands(r), [
    { key: 'hooks.cmd.install', text: 'cloudfs hooks install --client codex' },
    { key: 'hooks.cmd.uninstall', text: 'cloudfs hooks uninstall' },
  ]);
  assert.deepEqual(commands({ detected: ['claude'], clients: [{ client: 'claude', installed: false }] }), [{ key: 'hooks.cmd.install', text: 'cloudfs hooks install' }]);
  assert.deepEqual(commands({ detected: [], clients: [] }), [{ key: 'hooks.cmd.install', text: 'cloudfs hooks install' }]);
  assert.deepEqual(commands({ detected: ['claude'], clients: [{ client: 'claude', installed: true }] }), [{ key: 'hooks.cmd.uninstall', text: 'cloudfs hooks uninstall' }]);
  assert.deepEqual(commands(null), [{ key: 'hooks.cmd.install', text: 'cloudfs hooks install' }]);
});
