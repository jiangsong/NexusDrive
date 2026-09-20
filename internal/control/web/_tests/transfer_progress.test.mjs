import test from 'node:test';
import assert from 'node:assert/strict';
import { batchProgress, rateWindow } from '../transfer_progress.js';

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
