import test from 'node:test';
import assert from 'node:assert/strict';

import { plot, quadrantOf, median, bounds, radius, pointsOf } from '../heat_plot.js';

test('quadrants split at the medians of what is shown', () => {
  const pts = [{ x: 1, y: 1 }, { x: 10, y: 5 }, { x: 100, y: 50 }, { x: 400, y: 2 }];
  const b = bounds(pts);
  assert.deepEqual(b, { x: 55, y: 3.5 });
  assert.equal(quadrantOf(pts[0], b), 'warm_fresh');
  assert.equal(quadrantOf(pts[1], b), 'hot_fresh');
  assert.equal(quadrantOf(pts[2], b), 'hot_stale');
  assert.equal(quadrantOf(pts[3], b), 'warm_stale');
});

test('median tolerates odd, even, empty and junk', () => {
  assert.equal(median([3, 1, 2]), 2);
  assert.equal(median([4, 1, 2, 3]), 2.5);
  assert.equal(median([]), 0);
  assert.equal(median([NaN, 7]), 7);
});

test('radius grows with the log of the reads and never below one read', () => {
  assert.equal(radius(1), 3);
  assert.equal(radius(10), 6);
  assert.equal(radius(1000), 12);
  assert.equal(radius(0), 3);
  assert.equal(radius(-5), 3);
});

test('a plot is one circle per point with its path escaped, and empty data is a caption', () => {
  const svg = plot([{ x: 3, y: 4, kind: 'agent', path: '/a<b>&"c' }, { x: 30, y: 40, kind: 'kernel', path: '/d' }], { width: 200, height: 100, labels: { hot: 'HOT', stale: 'STALE' } });
  assert.ok(svg.startsWith('<svg'));
  assert.equal((svg.match(/<circle /g) || []).length, 2);
  assert.ok(svg.includes('data-path="/a&lt;b&gt;&amp;&quot;c"'));
  assert.ok(!svg.includes('<b>'));
  assert.ok(svg.includes('heat-agent') && svg.includes('heat-kernel'));
  assert.ok(svg.includes('>HOT<') && svg.includes('>STALE<'));
  assert.equal((svg.match(/heat-bound/g) || []).length, 2);
  const empty = plot([], { labels: { empty: 'nothing' } });
  assert.ok(empty.includes('heat-empty') && empty.includes('nothing'));
  assert.ok(!empty.includes('<circle'));
});

test('a single point sits on both boundaries and is hot and stale', () => {
  const pts = [{ x: 5, y: 2, path: '/one' }];
  assert.equal(quadrantOf(pts[0], bounds(pts)), 'hot_stale');
  const svg = plot(pts);
  assert.equal((svg.match(/<circle /g) || []).length, 1);
  assert.ok(svg.includes('data-quadrant="hot_stale"'));
});

test('pointsOf turns heat entries into days since mtime, reads and the leading reader', () => {
  const now = Date.parse('2026-09-17T00:00:00Z');
  const pts = pointsOf([
    { path: '/a', reads: 9, by_kind: { agent: 7, kernel: 2 }, mtime: '2026-09-07T00:00:00Z' },
    { path: '/b', reads: 1, by_kind: {}, mtime: null },
  ], now);
  assert.equal(pts[0].x, 10);
  assert.equal(pts[0].y, 9);
  assert.equal(pts[0].kind, 'agent');
  assert.equal(pts[1].x, 0);
  assert.equal(pts[1].kind, '');
  assert.deepEqual(pointsOf(null, now), []);
});
