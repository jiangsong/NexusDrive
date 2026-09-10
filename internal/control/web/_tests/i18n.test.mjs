import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

// i18n.js is written for a browser and reads location, localStorage and
// document as it loads, to decide the language. Standing those three up is
// what lets the catalog itself — the part with no browser in it — be checked
// here rather than by eye. The import is dynamic because a static one would
// be hoisted above these assignments.
// ?lang=en is the first thing detect() consults, so the language is this
// test's choice rather than whatever the host's navigator claims.
globalThis.location = { search: '?lang=en', href: 'http://127.0.0.1:7777/?lang=en' };
globalThis.document = {};
globalThis.localStorage = { getItem: () => null, setItem: () => {} };

const { t, tables, locale } = await import('../i18n.js');
const source = readFileSync(new URL('../i18n.js', import.meta.url), 'utf8');

// tableSource cuts one `const <name> = { ... };` table out of the file. The
// parsed object cannot answer a question about duplicates — the second entry
// has already won by then — so this reads the text.
function tableSource(name) {
  const start = source.indexOf('const ' + name + ' = {');
  assert.ok(start >= 0, 'no ' + name + ' table in i18n.js');
  const end = source.indexOf('\n};', start);
  assert.ok(end > start, name + ' table is not terminated');
  return source.slice(start, end);
}

const KEY = /'([a-z][a-zA-Z0-9._]*)':/g;

function keysInOrder(name) {
  return [...tableSource(name).matchAll(KEY)].map((m) => m[1]);
}

function placeholders(value) {
  return (String(value).match(/%s/g) || []).length;
}

test('no key is written twice in a table', () => {
  // A key defined twice is not an error in JavaScript: the later entry wins
  // silently and the earlier one becomes unreachable text that still looks
  // like the live string when you read the file. 'action.link' sat in both
  // tables twice for exactly that reason — the file-action menu was showing
  // the second spelling while the first one read like the truth.
  for (const name of ['zh', 'en']) {
    const seen = new Set();
    const twice = [];
    for (const key of keysInOrder(name)) {
      if (seen.has(key)) twice.push(key);
      seen.add(key);
    }
    assert.deepEqual(twice, [], name + ' defines these keys more than once');
  }
});

test('the two catalogs hold exactly the same keys', () => {
  // A key in one table and not the other renders as the dotted identifier
  // itself on the other language's screen.
  const zh = Object.keys(tables.zh).sort();
  const en = Object.keys(tables.en).sort();
  assert.deepEqual(en.filter((k) => !(k in tables.zh)), [], 'in en but not zh');
  assert.deepEqual(zh.filter((k) => !(k in tables.en)), [], 'in zh but not en');
});

test('a key takes the same number of arguments in both languages', () => {
  // t() fills %s from the call site's arguments in order. A translation that
  // dropped one leaves that argument — a drive name, a byte count, a path —
  // out of the sentence in that language only, and nothing else notices.
  const wrong = [];
  for (const key of Object.keys(tables.zh)) {
    if (placeholders(tables.zh[key]) !== placeholders(tables.en[key])) wrong.push(key);
  }
  assert.deepEqual(wrong, [], 'these keys take a different number of %s per language');
});

test('an argument goes into the sentence exactly as it came', () => {
  // t() filled the placeholder with String.prototype.replace, which reads
  // $&, $`, $' and $1 in the *replacement* as patterns. The arguments are
  // provider error text, remote names and file paths — none of it ours — so
  // an error mentioning $& printed the matched "%s" back at the reader, and a
  // path with $' in it swallowed the rest of the sentence.
  //
  // The last case is the same hazard one level up: an argument that itself
  // contains %s must not swallow the argument after it.
  assert.equal(locale(), 'en');
  assert.equal(tables.en['setup.step'], 'Step %s of %s', 'this test reads that template literally');
  for (const raw of ['$&', "$'", '$`', '$1', 'a$&b$`c', '100%s']) {
    assert.equal(t('setup.step', raw, '6'), 'Step ' + raw + ' of 6');
  }
});

test('an unknown key renders as itself rather than as blank', () => {
  assert.equal(t('no.such.key.exists'), 'no.such.key.exists');
});
