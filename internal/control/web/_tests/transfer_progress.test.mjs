import test from 'node:test';
import assert from 'node:assert/strict';
import { batchProgress, localPercent, rateWindow } from '../transfer_progress.js';

// The daemon computes the percentage itself (upload.PercentOf) so that the
// console, the terminal and the MCP tool cannot come to disagree by a digit
// about the same batch. The page prefers what it was sent; the formula here
// stays as the fallback for a daemon older than batch.percent, which a new
// console has to keep working against.

test('the page prefers the percentage the daemon computed', () => {
  // Bytes that would compute 25% locally, and a server that says 40.
  const p = batchProgress({ active: true, percent: 40, files_total: 10, files_done: 1, bytes_total: 400, bytes_done: 100 }, 0);
  assert.equal(p.percent, 40);
});

test('a daemon that sends no percentage still drives the bar', () => {
  const p = batchProgress({ active: true, files_total: 10, files_done: 1, bytes_total: 400, bytes_done: 100 }, 0);
  assert.equal(p.percent, 25, 'the fallback must keep an older daemon working');
});

test('the fallback agrees with the server at the 99.5% cap', () => {
  const b = { active: true, files_total: 2, files_done: 1, bytes_total: 1000, bytes_done: 1000 };
  // What this page works out on its own, and what the daemon sends for the
  // same batch, have to be the same number — that is the whole contract.
  assert.equal(localPercent(true, 2, 1, 1000, 1000), 99.5);
  assert.equal(batchProgress(b, 0).percent, 99.5);
  assert.equal(batchProgress({ ...b, percent: 99.5 }, 0).percent, 99.5);
});

test('a finished batch reads 100 whether or not the daemon said so', () => {
  const b = { active: false, files_total: 5, files_done: 5, bytes_total: 500, bytes_done: 500 };
  assert.equal(batchProgress(b, 0).percent, 100);
  assert.equal(batchProgress({ ...b, percent: 100 }, 0).percent, 100);
  assert.equal(localPercent(false, 5, 5, 500, 500), 100);
});

test('a percentage that is not a number is ignored, not rendered', () => {
  for (const bad of [null, undefined, 'lots', NaN, Infinity]) {
    const p = batchProgress({ active: true, percent: bad, files_total: 4, files_done: 1 }, 0);
    assert.equal(p.percent, 25, `a ${String(bad)} percent must fall back`);
  }
});

test('an idle queue with no batch behind it shows nothing', () => {
  const p = batchProgress({ active: false }, 0);
  assert.equal(p.state, 'idle');
  assert.equal(p.percent, 0);
});

test('a running batch is measured by bytes and never looks finished early', () => {
  const p = batchProgress({ active: true, files_total: 10, files_done: 9, bytes_total: 1000, bytes_done: 999 }, 100);
  assert.equal(p.state, 'active');
  assert.ok(p.percent < 100, 'a batch with bytes left is not 100%');
  assert.equal(p.eta, 0.01);
});

test('a batch without bytes is measured by files', () => {
  const p = batchProgress({ active: true, files_total: 4, files_done: 1 }, 0);
  assert.equal(p.percent, 25);
  assert.equal(p.eta, null);
});

test('a finished batch stays full with its figures', () => {
  const p = batchProgress({ active: false, files_total: 5, files_done: 5, bytes_total: 500, bytes_done: 500 }, 50);
  assert.equal(p.state, 'done');
  assert.equal(p.percent, 100);
  assert.equal(p.rate, 0);
});

test('the rate is bytes per second over the window, and resets with the batch', () => {
  const w = rateWindow(10000);
  assert.equal(w.sample(0, 0, true), 0);
  assert.equal(w.sample(1000, 1000, true), 1000);
  assert.equal(w.sample(2000, 3000, true), 1500);
  // Twelve seconds later the first readings have left the window.
  assert.equal(w.sample(12000, 13000, true), 1000);
  assert.equal(w.sample(13000, 0, true), 0, 'a new batch starts the window over');
  assert.equal(w.sample(14000, 500, false), 0, 'not active: no rate');
});
