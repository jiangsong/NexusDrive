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

import { latestOnly, coalesce } from '../paged.js';

test('only the newest of several overlapping loads gets to paint', () => {
  // Two change events started two loads; the first fetch landed after the
  // second had cleared the table, and both appended: every row twice.
  const guard = latestOnly();
  const first = guard.take();
  const second = guard.take();
  assert.equal(guard.current(first), false, 'the older load must paint nothing');
  assert.equal(guard.current(second), true);
  const third = guard.take();
  assert.equal(guard.current(second), false);
  assert.equal(guard.current(third), true);
});

test('a burst of change events becomes one reload, and a long copy still refreshes', () => {
  let now = 0;
  let timers = [];
  const setTimer = (fn, ms) => { const t = { at: now + ms, fn }; timers.push(t); return t; };
  const clearTimer = (t) => { timers = timers.filter((x) => x !== t); };
  const advance = (ms) => {
    now += ms;
    const due = timers.filter((t) => t.at <= now).sort((a, b) => a.at - b.at);
    timers = timers.filter((t) => t.at > now);
    for (const t of due) t.fn();
  };
  let runs = 0;
  const reload = coalesce(() => { runs++; }, 300, { setTimer, clearTimer });
  for (let i = 0; i < 50; i++) reload();
  assert.equal(runs, 0, 'nothing runs inside the burst');
  advance(300);
  assert.equal(runs, 1, 'one reload for fifty events');
  advance(1000);
  assert.equal(runs, 1, 'and no run without a call');
  // A copy that raises an event every 10 ms for a second: the listing is
  // refreshed on the way, about every 300 ms, not once at the end.
  for (let i = 0; i < 100; i++) { reload(); advance(10); }
  assert.ok(runs >= 3 && runs <= 5, `runs during a one-second stream = ${runs}`);
  reload();
  reload.cancel();
  advance(1000);
  assert.equal(timers.length, 0, 'cancel drops the pending run');
});

import { listingAffected } from '../paged.js';

test('a listing reloads for its own rows, not for what lands deeper down', () => {
  // Uploads finishing under /fs/.git/objects/ab kept the listing of /fs
  // reloading for as long as the queue ran; none of them is a row of /fs.
  assert.equal(listingAffected({ paths: ['/fs/.git/objects/ab/cdef'] }, '/fs'), false);
  assert.equal(listingAffected({ paths: ['/fs/README.md'] }, '/fs'), true, 'a direct child changed state');
  assert.equal(listingAffected({ paths: ['/fs/new-dir'] }, '/fs'), true, 'a row appeared');
  assert.equal(listingAffected({ paths: ['/fs'] }, '/fs'), true, 'the directory itself');
  assert.equal(listingAffected({ paths: ['/other/x'] }, '/fs'), false);
});

test('the root lists its own children and nothing deeper', () => {
  assert.equal(listingAffected({ paths: ['/smoke'] }, '/'), true);
  assert.equal(listingAffected({ paths: ['/smoke/a.txt'] }, '/'), false);
});

test('an ancestor moving or going away takes the listing with it', () => {
  assert.equal(listingAffected({ paths: ['/fs'], subtree: true }, '/fs/.git/objects'), true);
  assert.equal(listingAffected({ paths: ['/fs'] }, '/fs/.git/objects'), false, 'a plain touch of an ancestor changes no row');
  assert.equal(listingAffected({ rescan: true, paths: [] }, '/anything'), true);
});
