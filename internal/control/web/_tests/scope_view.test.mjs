import test from 'node:test';
import assert from 'node:assert/strict';

import { cleanPath, under, parsePrefixes, scopeParts, validateTokenScope } from '../scope_view.js';

test('an empty read list is the whole mount', () => {
  assert.deepEqual(scopeParts({}), [{ key: 'scope.all', args: [] }]);
  // The daemon writes `write: null` for "same as read" and a zero date for
  // "never expires"; neither is a part of the summary.
  assert.deepEqual(scopeParts({ write: null, expires_at: '0001-01-01T00:00:00Z' }), [{ key: 'scope.all', args: [] }]);
});

test('write null means the same as read and is not repeated', () => {
  assert.deepEqual(scopeParts({ read: ['/work'] }), [{ key: 'scope.read', args: ['/work'] }]);
  assert.deepEqual(scopeParts({ read: ['/work'], write: null }), [{ key: 'scope.read', args: ['/work'] }]);
});

test('an empty write list reads as read only', () => {
  assert.deepEqual(scopeParts({ read: ['/work'], write: [] }).map((p) => p.key), ['scope.read', 'scope.readonly']);
  assert.deepEqual(scopeParts({ read_only: true }).map((p) => p.key), ['scope.all', 'scope.readonly']);
  // read_only wins over a write list that is still spelled out.
  assert.deepEqual(scopeParts({ read: ['/work'], write: ['/work'], read_only: true }).map((p) => p.key), ['scope.read', 'scope.readonly']);
});

test('separate write, sandbox and expiry each get a part', () => {
  const parts = scopeParts({ read: ['/work'], write: ['/work/.agent'], sandbox: '/work/.agent/s1', expires_at: '2026-10-01T00:00:00Z' });
  assert.deepEqual(parts.map((p) => p.key), ['scope.read', 'scope.write', 'scope.sandbox', 'scope.expires']);
  assert.deepEqual(parts.map((p) => p.args), [['/work'], ['/work/.agent'], ['/work/.agent/s1'], ['2026-10-01T00:00:00Z']]);
});

test('several prefixes join into one argument', () => {
  assert.deepEqual(scopeParts({ read: ['/work', '/gd'] }), [{ key: 'scope.read', args: ['/work, /gd'] }]);
});

test('dotdot and trailing slashes are cleaned before comparing', () => {
  assert.equal(cleanPath('/work/../gd/'), '/gd');
  assert.equal(cleanPath('/'), '/');
  assert.equal(cleanPath(''), '/');
  assert.equal(cleanPath('/a/./b//c/'), '/a/b/c');
  assert.equal(cleanPath('/../..'), '/');
  assert.equal(under('/workshop/a', '/work'), false);
  assert.equal(under('/work/a', '/work'), true);
  assert.equal(under('/work', '/work/'), true);
  assert.equal(under('/anything', '/'), true);
});

test('prefix text is split per line, cleaned and deduped', () => {
  assert.deepEqual(parsePrefixes(' /work \n\n/work/\n/gd'), ['/work', '/gd']);
  assert.deepEqual(parsePrefixes(''), []);
  assert.deepEqual(parsePrefixes(null), []);
});

test('write must be inside read', () => {
  assert.equal(validateTokenScope({ read: ['/work'], write: ['/gd'] }), 'tokens.err.write_outside_read');
  assert.equal(validateTokenScope({ read: ['/work'], write: ['/work/.agent'] }), '');
  assert.equal(validateTokenScope({ read: [], write: ['/gd'] }), '');
  assert.equal(validateTokenScope({ read: ['work'], write: [] }), 'tokens.err.relative');
  assert.equal(validateTokenScope({ read: ['/work'], write: ['work/.agent'] }), 'tokens.err.relative');
  assert.equal(validateTokenScope({}), '');
});
