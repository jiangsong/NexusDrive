import test from 'node:test';
import assert from 'node:assert/strict';

import { proxyChoices } from '../screens/proxy_choice.js';

const cfg = {
  outbounds: [{ name: 'direct', type: 'direct' }, { name: 'local', type: 'socks5', addr: '127.0.0.1:1081' }],
  groups: [{ name: 'auto', type: 'fallback', members: ['local'] }],
  rules: [],
  default: 'auto',
};

test('an empty value follows the rules and says where they lead', () => {
  const [first] = proxyChoices(cfg, '');
  assert.equal(first.value, '');
  assert.equal(first.key, 'conn.proxy.rules');
  assert.deepEqual(first.args, ['auto']);
});

test('direct is offered once, then groups, then outbounds', () => {
  assert.deepEqual(proxyChoices(cfg, '').map((c) => c.value), ['', 'direct', 'auto', 'local']);
});

test('no proxy section still offers rules and direct', () => {
  const got = proxyChoices(null, '');
  assert.deepEqual(got.map((c) => c.value), ['', 'direct']);
  assert.deepEqual(got[0].args, ['direct']);
});

test('a saved name the configuration no longer has stays selectable and is marked', () => {
  const got = proxyChoices(cfg, 'hk');
  const last = got[got.length - 1];
  assert.equal(last.value, 'hk');
  assert.equal(last.key, 'conn.proxy.missing');
});

test('a saved name the configuration has is not listed twice', () => {
  assert.equal(proxyChoices(cfg, 'local').filter((c) => c.value === 'local').length, 1);
});
