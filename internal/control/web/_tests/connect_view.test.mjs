import test from 'node:test';
import assert from 'node:assert/strict';

import { stdioWarning } from '../connect_view.js';

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
