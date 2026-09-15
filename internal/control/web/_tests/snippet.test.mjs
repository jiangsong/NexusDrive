import test from 'node:test';
import assert from 'node:assert/strict';

import { truncateRunes, highlightParts, snippetParts } from '../snippet.js';

test('highlighting is case-insensitive and keeps the original casing', () => {
  assert.deepEqual(highlightParts('CloudFS mounts cloudfs', 'cloudfs'),
    [{ text: 'CloudFS', hit: true }, { text: ' mounts ', hit: false }, { text: 'cloudfs', hit: true }]);
});

test('every word of the query is highlighted and a quoted phrase is one term', () => {
  assert.deepEqual(highlightParts('read the plan, then read it again', 'plan read'),
    [{ text: 'read', hit: true }, { text: ' the ', hit: false }, { text: 'plan', hit: true },
      { text: ', then ', hit: false }, { text: 'read', hit: true }, { text: ' it again', hit: false }]);
  assert.deepEqual(highlightParts('a quarterly report and a report', '"quarterly report"'),
    [{ text: 'a ', hit: false }, { text: 'quarterly report', hit: true }, { text: ' and a report', hit: false }]);
});

test('CJK text is truncated by character, never by byte', () => {
  const s = '云端文件系统'.repeat(100);
  const out = truncateRunes(s, 7);
  assert.equal(out, '云端文件系统云…');
  assert.equal(truncateRunes('云端', 2), '云端');
});

test('a surrogate pair is never split', () => {
  assert.equal(truncateRunes('a😀b😀c', 2), 'a😀…');
  const joined = snippetParts('😀'.repeat(300) + 'needle', 'needle', 240).map((p) => p.text).join('');
  for (const cp of joined) assert.ok(cp.codePointAt(0) < 0xd800 || cp.codePointAt(0) > 0xdfff, 'a lone surrogate leaked');
});

test('markup in file content stays plain text parts', () => {
  const parts = snippetParts('<img src=x onerror=alert(1)> needle', 'needle', 240);
  assert.equal(parts[0].text, '<img src=x onerror=alert(1)> ');
  assert.equal(parts[0].hit, false);
  assert.ok(parts.every((p) => typeof p.text === 'string' && !('html' in p)));
  assert.deepEqual(highlightParts('<b>&amp;</b>', '<b>'), [{ text: '<b>', hit: true }, { text: '&amp;</b>', hit: false }]);
});

test('the snippet is centred on the first hit and bounded', () => {
  const text = 'x'.repeat(1000) + 'needle' + 'y'.repeat(1000);
  const parts = snippetParts(text, 'needle', 240);
  const joined = parts.map((p) => p.text).join('');
  assert.ok([...joined].length <= 242, String([...joined].length));
  assert.ok(joined.startsWith('…') && joined.endsWith('…'));
  assert.ok(parts.some((p) => p.hit && p.text === 'needle'));
  // A hit near the start keeps the start; one near the end keeps the end.
  const head = snippetParts('needle' + 'y'.repeat(1000), 'needle', 240).map((p) => p.text).join('');
  assert.ok(head.startsWith('needle') && head.endsWith('…'));
  const tail = snippetParts('x'.repeat(1000) + 'needle', 'needle', 240).map((p) => p.text).join('');
  assert.ok(tail.startsWith('…') && tail.endsWith('needle'));
  // Text that fits is returned whole, with no ellipsis.
  assert.equal(snippetParts('short needle', 'needle').map((p) => p.text).join(''), 'short needle');
});

test('an empty query yields one plain part', () => {
  assert.deepEqual(highlightParts('abc', ''), [{ text: 'abc', hit: false }]);
  assert.deepEqual(highlightParts('abc', '   '), [{ text: 'abc', hit: false }]);
  assert.deepEqual(highlightParts('', 'abc'), [{ text: '', hit: false }]);
});

test('a letter whose lowercase form changes length still matches by position', () => {
  // İ lowercases to two UTF-16 units; a naive toLowerCase would shift every
  // index after it and cut the wrong characters.
  assert.deepEqual(highlightParts('İstanbul needle', 'needle'),
    [{ text: 'İstanbul ', hit: false }, { text: 'needle', hit: true }]);
});
