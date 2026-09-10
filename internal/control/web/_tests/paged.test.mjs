import test from 'node:test';
import assert from 'node:assert/strict';

import { pageCursor, pageFailureMode } from '../paged.js';

test('a failed first page replaces the table with the error', () => {
  assert.equal(pageFailureMode(''), 'replace');
  assert.equal(pageFailureMode(undefined), 'replace');
  assert.equal(pageFailureMode(null), 'replace');
});

test('a failed continuation keeps the pages already on screen', () => {
  // "Load more" failing threw away everything fetched so far, including the
  // rows the reader was looking at, for a failure that cost them nothing.
  assert.equal(pageFailureMode('cursor-abc'), 'append');
});

test('a loader handed a click event asks for the first page, not for a cursor', () => {
  // `onclick: load` hands the loader the click event as its cursor. The event
  // then went into &cursor= and the daemon answered 400, so a refresh button
  // wired that way broke the table it was meant to reload. Nothing that is
  // not a string can be a cursor.
  const clickLike = { type: 'click', target: {}, preventDefault() {} };
  assert.equal(pageCursor(clickLike), '');
  assert.equal(pageCursor(undefined), '');
  assert.equal(pageCursor(null), '');
  assert.equal(pageCursor(0), '');
});

test('a real cursor survives untouched', () => {
  assert.equal(pageCursor('cursor-abc'), 'cursor-abc');
  assert.equal(pageCursor(''), '');
});
