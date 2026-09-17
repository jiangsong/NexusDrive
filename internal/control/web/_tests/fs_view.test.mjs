import test from 'node:test';
import assert from 'node:assert/strict';

import { shareRefusal } from '../fs_view.js';

test('a share refusal is read from the 409 body, credentials may be forced', () => {
  assert.deepEqual(shareRefusal({ status: 409, message: '{"error":"x","code":"not_cached"}' }), { key: 'fs.share.not_cached', force: false });
  assert.deepEqual(shareRefusal({ status: 409, message: '{"error":"x","code":"credentials","findings":[]}' }), { key: 'fs.share.credentials', force: true });
  assert.equal(shareRefusal({ status: 409, message: 'not json' }), null);
  assert.equal(shareRefusal({ status: 500, message: '{"code":"not_cached"}' }), null);
  assert.equal(shareRefusal({ status: 409, message: '{"code":"something"}' }), null);
  assert.equal(shareRefusal(null), null);
});
