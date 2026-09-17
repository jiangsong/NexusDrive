import test from 'node:test';
import assert from 'node:assert/strict';

import { stdioWarning, bridgeBanner } from '../connect_view.js';

test('a stdio server beside the owner raises the banner and links to diagnostics', () => {
  const w = stdioWarning({ http_listening: true, owner: true, stdio_non_owner: true });
  assert.ok(w, 'no banner');
  assert.equal(w.href, '#/diagnostics');
  assert.equal(w.key, 'connect.stdio.banner');
  assert.equal(w.linkKey, 'connect.stdio.link');
});

test('without a stdio server there is no banner, whatever else the answer says', () => {
  assert.equal(stdioWarning({ http_listening: false, owner: false, stdio_non_owner: false }), null);
  assert.equal(stdioWarning({ http_listening: true, owner: true }), null);
  assert.equal(stdioWarning({ stdio_non_owner: 'true' }), null);
  assert.equal(stdioWarning(null), null);
});

test('a connected bridge is a green line counting its sessions', () => {
  const b = bridgeBanner({ stdio_non_owner: true, bridge: { state: 'connected', sessions: 2 } });
  assert.deepEqual(b, { cls: 'ok', key: 'connect.bridge.connected', arg: 2 });
  assert.equal(bridgeBanner({ bridge: { state: 'connected' } }).arg, 0);
});

test('a disabled bridge is a yellow line naming the reason', () => {
  for (const [reason, key] of [['http_off', 'connect.bridge.http_off'], ['not_loopback', 'connect.bridge.not_loopback'], ['no_token', 'connect.bridge.no_token'], ['something-new', 'connect.bridge.unknown']]) {
    const b = bridgeBanner({ bridge: { state: 'disabled', reason } });
    assert.equal(b.cls, 'warn');
    assert.equal(b.key, 'connect.bridge.disabled');
    assert.equal(b.reasonKey, key);
  }
});

test('n/a, a missing field and an odd shape all draw nothing', () => {
  assert.equal(bridgeBanner({ bridge: { state: 'n/a' } }), null);
  assert.equal(bridgeBanner({ stdio_non_owner: true }), null);
  assert.equal(bridgeBanner({ bridge: { state: 7 } }), null);
  assert.equal(bridgeBanner(null), null);
});
