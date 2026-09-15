import test from 'node:test';
import assert from 'node:assert/strict';

import { tokenize, parseQueryString, buildQueryString, searchURL, highlightParts, sizeRange, splitSizeRange, pushRecent } from '../search_query.js';

test('the default scope asks for the whole drive', () => {
  // The old listener always sent path=<cwd>, so a search from /work never
  // found a file in /archive. The whole drive is now the default and the
  // request says so by not naming a path at all.
  const url = searchURL({ query: 'plan', scope: 'all', cwd: '/work' });
  assert.ok(url.startsWith('/search?q=plan'));
  assert.ok(!url.includes('path='));
  assert.ok(searchURL({ query: 'plan', scope: 'cwd', cwd: '/work' }).includes('&path=%2Fwork'));
  assert.ok(searchURL({ query: 'plan', scope: 'all', cwd: '/', sort: 'size' }).endsWith('&sort=size'));
  assert.ok(searchURL({ query: 'plan', scope: 'all', cwd: '/', sort: '-mtime' }).endsWith('&sort=-mtime'));
  assert.ok(!searchURL({ query: 'plan', scope: 'all', cwd: '/' }).includes('sort='));
  assert.ok(searchURL({ query: 'a b', scope: 'all', cwd: '/' }).startsWith('/search?q=a%20b&limit=100'));
});

test('quotes group a literal and an unterminated quote is a word', () => {
  assert.deepEqual(tokenize('  my "quarterly report" -"old draft" a"b.txt "open'), ['my', '"quarterly report"', '-"old draft"', 'a"b.txt', '"open']);
  assert.deepEqual(tokenize(''), []);
});

test('filters and terms round-trip through the query string', () => {
  const q = 'report -tmp ext:go,md size:>1m dm:>2026-09 type:file';
  const p = parseQueryString(q);
  assert.deepEqual(p.filters, { kind: 'file', ext: 'go,md', size: '>1m', dm: '>2026-09' });
  assert.deepEqual(p.terms, ['report', '-tmp']);
  assert.deepEqual(p.errors, []);
  assert.equal(buildQueryString(p.terms, p.filters), q);
  // The filters may be typed in any order; the bar writes them back in one
  // canonical order, words first, so the box and the bar agree on a spelling.
  const shuffled = parseQueryString('type:file report size:>1m -tmp dm:>2026-09 ext:go,md');
  assert.deepEqual(shuffled, p);
  assert.equal(buildQueryString(shuffled.terms, shuffled.filters), q);
  // Empty filters are not written; path: and negated filters stay words.
  assert.equal(buildQueryString(['a', 'path:src', '-ext:bak'], { ext: '', size: '' }), 'a path:src -ext:bak');
  assert.deepEqual(parseQueryString('path:src -ext:bak "size:1k"').terms, ['path:src', '-ext:bak', '"size:1k"']);
});

test('a size range composes and decomposes', () => {
  assert.equal(sizeRange('1m', '10m'), '1m..10m');
  assert.equal(sizeRange('1m', ''), '>1m');
  assert.equal(sizeRange('', '10m'), '<10m');
  assert.equal(sizeRange('', ''), '');
  assert.deepEqual(splitSizeRange('1m..10m'), { min: '1m', max: '10m' });
  assert.deepEqual(splitSizeRange('>1m'), { min: '1m', max: '' });
  assert.deepEqual(splitSizeRange('<=10m'), { min: '', max: '10m' });
  assert.deepEqual(splitSizeRange('4k'), { min: '4k', max: '4k' });
  assert.deepEqual(splitSizeRange(''), { min: '', max: '' });
});

test('invalid filter values stay words and are reported', () => {
  const p = parseQueryString('size:lots dm:yesterday type:link "unterminated');
  assert.deepEqual(p.errors, ['size', 'dm', 'type']);
  assert.ok(p.terms.includes('size:lots'));
  assert.deepEqual(p.filters, {});
  assert.equal(buildQueryString(p.terms, p.filters), 'size:lots dm:yesterday type:link "unterminated');
  // The daemon takes one size, one date and one kind per query; a second
  // one is left as a word for it to refuse, and flagged here.
  const twice = parseQueryString('size:>1m size:<9m');
  assert.deepEqual(twice.filters, { size: '>1m' });
  assert.deepEqual(twice.errors, ['size']);
  assert.deepEqual(twice.terms, ['size:<9m']);
  // An empty value, a slash in an extension and the wrong kind are errors
  // too; the daemon's grammar accepts >= and <= and a..b ranges.
  assert.deepEqual(parseQueryString('ext: ext:a/b type:folder').errors, ['ext', 'ext', 'type']);
  assert.deepEqual(parseQueryString('size:>=1.5kb dm:2026-01..2026-03 ext:.go type:dir').errors, []);
});

test('highlighting matches bare words only, keeps casing and never emits markup', () => {
  const parsed = parseQueryString('Plan ext:md');
  assert.deepEqual(highlightParts('MyPlan.md', parsed), [{ text: 'My', hit: false }, { text: 'Plan', hit: true }, { text: '.md', hit: false }]);
  assert.deepEqual(highlightParts('x', parseQueryString('ext:md')), [{ text: 'x', hit: false }]);
  assert.deepEqual(highlightParts('draft.md', parseQueryString('*.md')), [{ text: 'draft', hit: false }, { text: '.md', hit: true }]);
  assert.ok(highlightParts('<b>plan</b>', parsed).every((p) => typeof p.text === 'string' && !('html' in p)));
  // Overlapping and adjacent hits merge into one run; a negated word, a
  // path word and a filter do not highlight; a quoted literal does.
  assert.deepEqual(highlightParts('planplan.txt', parseQueryString('plan lanp')), [{ text: 'planplan', hit: true }, { text: '.txt', hit: false }]);
  assert.deepEqual(highlightParts('tmp.md', parseQueryString('-tmp src/tmp size:>1k')), [{ text: 'tmp.md', hit: false }]);
  assert.deepEqual(highlightParts('My Report.md', parseQueryString('"my report"')), [{ text: 'My Report', hit: true }, { text: '.md', hit: false }]);
  assert.deepEqual(highlightParts('anything', parseQueryString('')), [{ text: 'anything', hit: false }]);
});

test('recent searches are most-recent-first, unique and capped at ten', () => {
  let list = [];
  for (let i = 0; i < 12; i++) list = pushRecent(list, 'q' + i);
  assert.equal(list.length, 10);
  assert.equal(list[0], 'q11');
  assert.deepEqual(pushRecent(['a', 'b'], 'b'), ['b', 'a']);
  assert.deepEqual(pushRecent(['a'], '  '), ['a']);
  const before = ['a'];
  pushRecent(before, 'b');
  assert.deepEqual(before, ['a']);
});
