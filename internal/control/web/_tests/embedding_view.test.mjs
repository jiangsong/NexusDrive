import test from 'node:test';
import assert from 'node:assert/strict';

import { bannerFor, healthOf, progressOf, estimateOf, searchModeParam, degradedReason, AUTH_COMMAND } from '../embedding_view.js';

// The status document is what GET /index/embedding serves
// (internal/control/embedding.go EmbeddingResponse).
const remote = {
  enabled: true, provider: 'openai', model: 'text-embedding-3-small', dim: 512, base_host: 'api.openai.com',
  remote: true, healthy: true, embedded: 40, pending: 2, vectors: 40, max_chunks: 200000, capped: false,
  api_key_configured: true, chars_this_month: 8000,
  estimate: { chars: 8000, tokens_approx: 2000, corpus_tokens_approx: 12600, formula: 'chunks × ~300 tokens × unit price', note: 'an estimate' },
};

test('the banner exists for remote === true and for nothing else', () => {
  assert.deepEqual(bannerFor(remote), { host: 'api.openai.com' });
  assert.equal(bannerFor({ ...remote, remote: false }), null);
  assert.equal(bannerFor({ ...remote, remote: undefined }), null);
  // A truthy string is not the boolean the daemon sends; the banner is
  // decided by the daemon, not by whatever a proxy put in the field.
  assert.equal(bannerFor({ ...remote, remote: 'yes' }), null);
  assert.equal(bannerFor(null), null);
  assert.equal(bannerFor({}), null);
  // No host is still a banner: content leaves the machine.
  assert.deepEqual(bannerFor({ remote: true }), { host: '' });
});

test('health is a dot, the last error and the breaker time', () => {
  assert.deepEqual(healthOf(remote), { dot: 'ok', error: '', openUntil: '' });
  assert.deepEqual(healthOf({ ...remote, healthy: false, last_error: 'HTTP 503' }), { dot: 'warn', error: 'HTTP 503', openUntil: '' });
  assert.deepEqual(healthOf({ ...remote, healthy: false, last_error: 'HTTP 503', breaker_open_until: '2026-09-15T10:00:00Z' }),
    { dot: 'bad', error: 'HTTP 503', openUntil: '2026-09-15T10:00:00Z' });
  // Go's zero time is "not open", not a date in year one.
  assert.equal(healthOf({ ...remote, healthy: false, breaker_open_until: '0001-01-01T00:00:00Z' }).openUntil, '');
  assert.deepEqual(healthOf({ enabled: false, provider: 'none' }), { dot: 'off', error: '', openUntil: '' });
  assert.equal(healthOf(null).dot, 'off');
});

test('progress carries the cap', () => {
  assert.deepEqual(progressOf(remote), { embedded: 40, pending: 2, vectors: 40, max: 200000, capped: false });
  assert.deepEqual(progressOf({ ...remote, capped: true, vectors: 200000 }).capped, true);
  assert.deepEqual(progressOf({}), { embedded: 0, pending: 0, vectors: 0, max: 0, capped: false });
});

test('the estimate is the numbers plus the formula, never a price', () => {
  const e = estimateOf(remote);
  assert.deepEqual(e, { chars: 8000, tokens: 2000, corpusTokens: 12600, formula: 'chunks × ~300 tokens × unit price' });
  assert.ok(!('price' in e) && !('cost' in e), 'the page must not invent a price');
  // A daemon that sends no estimate block still yields numbers.
  assert.deepEqual(estimateOf({ chars_this_month: 400 }), { chars: 400, tokens: 100, corpusTokens: 0, formula: '' });
  assert.deepEqual(estimateOf({}), { chars: 0, tokens: 0, corpusTokens: 0, formula: '' });
});

test('the auth command is the terminal command, with no key in it', () => {
  assert.equal(AUTH_COMMAND, 'cloudfs index auth');
});

test('search modes map to the query parameter the daemon accepts', () => {
  assert.equal(searchModeParam('semantic'), 'hybrid');
  assert.equal(searchModeParam('content'), 'keyword');
  assert.equal(searchModeParam('name'), '');
  assert.equal(searchModeParam(undefined), '');
  assert.equal(searchModeParam('vector'), '');
});

test('the degraded reason is the daemon text or nothing', () => {
  assert.equal(degradedReason({ hits: [], degraded: 'semantic search is not configured; results use keyword matching' }),
    'semantic search is not configured; results use keyword matching');
  assert.equal(degradedReason({ hits: [] }), '');
  assert.equal(degradedReason({ degraded: '' }), '');
  assert.equal(degradedReason(null), '');
  // Whatever type arrives, the note is a string for a text node.
  assert.equal(degradedReason({ degraded: true }), 'true');
});
