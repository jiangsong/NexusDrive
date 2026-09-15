import test from 'node:test';
import assert from 'node:assert/strict';

import { PRESETS, includeFor, parseSize } from '../index_presets.js';

test('the documents preset covers office and pdf files', () => {
  for (const g of ['**/*.md', '**/*.pdf', '**/*.docx', '**/*.xlsx', '**/*.pptx']) assert.ok(PRESETS.docs.includes(g), g);
});

test('includeFor returns a copy so a form cannot edit the preset', () => {
  const list = includeFor('code');
  list.push('x');
  assert.ok(!PRESETS.code.includes('x'));
  assert.deepEqual(includeFor('nope'), []);
  // Object.prototype names are not presets either.
  assert.deepEqual(includeFor('constructor'), []);
});

test('sizes parse with binary units', () => {
  assert.equal(parseSize('20MiB'), 20 * 1024 * 1024);
  assert.equal(parseSize('512 KiB'), 512 * 1024);
  assert.equal(parseSize('1.5 GiB'), Math.round(1.5 * 1024 ** 3));
  assert.equal(parseSize('4096'), 4096);
  assert.equal(parseSize('2 MB'), 2000000);
  assert.equal(parseSize(''), 0);
  assert.equal(parseSize(undefined), 0);
  assert.ok(Number.isNaN(parseSize('lots')));
  assert.ok(Number.isNaN(parseSize('20 MiBs')));
  assert.ok(Number.isNaN(parseSize('-3 MiB')));
});
