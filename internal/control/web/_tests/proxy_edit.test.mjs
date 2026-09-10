import test from 'node:test';
import assert from 'node:assert/strict';

import { planOutboundEdit, renameOutboundReferences } from '../screens/proxy_edit.js';

const credentialed = { name: 'office', type: 'socks5', addr: 'socks5://bob:***@10.0.0.9:1080', has_credentials: true };

test('an untouched credentialed outbound asks the daemon to keep what it has', () => {
  const got = planOutboundEdit(credentialed, { name: 'office', type: 'socks5', addr: credentialed.addr });
  assert.equal(got.error, undefined);
  assert.equal(got.outbound.keep_credentials, true);
  assert.equal(got.outbound.has_credentials, true, 'the row must keep saying it carries a credential');
});

test('editing only the name of a credentialed outbound asks for the address again', () => {
  const got = planOutboundEdit(credentialed, { name: 'hq', type: 'socks5', addr: credentialed.addr });
  assert.equal(got.outbound, undefined);
  assert.equal(got.error, 'proxy.creds.reenter');
});

test('editing only the type of a credentialed outbound asks for the address again', () => {
  const got = planOutboundEdit(credentialed, { name: 'office', type: 'http', addr: credentialed.addr });
  assert.equal(got.error, 'proxy.creds.reenter');
});

test('a rename with a freshly typed address is accepted and drops the stale flag', () => {
  const got = planOutboundEdit(credentialed, { name: 'hq', type: 'socks5', addr: 'socks5://bob:s3cret@10.0.0.9:1080' });
  assert.equal(got.error, undefined);
  assert.equal(got.outbound.keep_credentials, undefined);
  assert.equal(got.outbound.has_credentials, undefined);
  assert.equal(got.outbound.name, 'hq');
});

test('clearing the address of a credentialed outbound is refused, not sent as an empty address', () => {
  const got = planOutboundEdit(credentialed, { name: 'office', type: 'socks5', addr: '' });
  assert.equal(got.outbound, undefined);
  assert.equal(got.error, 'proxy.creds.cleared');
});

test('a second edit before saving still knows the outbound carries a credential', () => {
  const first = planOutboundEdit(credentialed, { name: 'office', type: 'socks5', addr: credentialed.addr }).outbound;
  const second = planOutboundEdit(first, { name: 'office', type: 'socks5', addr: first.addr });
  assert.equal(second.error, undefined);
  assert.equal(second.outbound.keep_credentials, true);
});

test('an outbound with no credential is edited freely', () => {
  const plain = { name: 'lan', type: 'http', addr: 'http://10.0.0.1:3128' };
  const got = planOutboundEdit(plain, { name: 'lan2', type: 'socks5', addr: 'socks5://10.0.0.1:1080' });
  assert.equal(got.error, undefined);
  assert.deepEqual(got.outbound, { name: 'lan2', type: 'socks5', addr: 'socks5://10.0.0.1:1080' });
});

test('a new outbound needs a name', () => {
  assert.equal(planOutboundEdit(null, { name: '', type: 'socks5', addr: 'x:1' }).error, 'proxy.name.required');
});

test('renaming rewrites every group member and rule that named the outbound', () => {
  const cfg = {
    outbounds: [{ name: 'office', type: 'socks5', addr: 'a' }],
    groups: [{ name: 'g', type: 'fallback', members: ['office', 'direct'] }],
    rules: ['DOMAIN-SUFFIX,example.com,office', 'FINAL,direct'],
  };
  const next = renameOutboundReferences(cfg, 'office', 'hq');
  assert.deepEqual(next.groups[0].members, ['hq', 'direct']);
  assert.deepEqual(next.rules, ['DOMAIN-SUFFIX,example.com,hq', 'FINAL,direct']);
  assert.deepEqual(cfg.groups[0].members, ['office', 'direct'], 'the original config must not be mutated');
});

test('renaming does not rewrite a rule that merely contains the name as text', () => {
  const cfg = {
    outbounds: [],
    groups: [],
    rules: ['DOMAIN-SUFFIX,office.example.com,direct'],
  };
  assert.deepEqual(renameOutboundReferences(cfg, 'office', 'hq').rules, ['DOMAIN-SUFFIX,office.example.com,direct']);
});

test('renaming an outbound a remote selects is refused, because this page cannot follow it there', () => {
  const plain = { name: 'office', type: 'socks5', addr: 'socks5://10.0.0.9:1080' };
  const got = planOutboundEdit(plain, { name: 'hq', type: 'socks5', addr: plain.addr }, ['gdrive', 'nas']);
  assert.equal(got.outbound, undefined);
  assert.equal(got.error, 'proxy.rename.blocked');
  assert.deepEqual(got.args, ['gdrive, nas']);
});

test('editing anything but the name of an outbound a remote selects is allowed', () => {
  const plain = { name: 'office', type: 'socks5', addr: 'socks5://10.0.0.9:1080' };
  const got = planOutboundEdit(plain, { name: 'office', type: 'http', addr: 'http://10.0.0.9:3128' }, ['gdrive']);
  assert.equal(got.error, undefined);
  assert.equal(got.outbound.type, 'http');
});

test('a rename with no remote pointing at it stays allowed', () => {
  const plain = { name: 'office', type: 'socks5', addr: 'socks5://10.0.0.9:1080' };
  assert.equal(planOutboundEdit(plain, { name: 'hq', type: 'socks5', addr: plain.addr }, []).error, undefined);
});
