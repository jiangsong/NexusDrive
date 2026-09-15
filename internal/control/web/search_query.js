// The query grammar of the name search, on the page side. It is the same
// table meta/query.go reads — bare words AND-ed, double quotes group a
// literal, * and ? are wildcards, ext: size: dm: type: path: filter — so a
// query the filter bar composes and one a person types mean the same thing
// to the daemon. This module has no imports and touches no DOM: it turns a
// query string into words and filters and back, builds the /search URL, and
// says which spans of a name a query hit, so all of that runs under node.
//
// Only the shape of a value is checked here. The daemon is the truth about
// what a size or a date means; the page's job is to keep a filter it cannot
// read visible as the words the person typed, and to say which key it could
// not read, rather than to drop it.

const KEYS = ['ext', 'size', 'dm', 'type', 'path'];
// FILTER_KEYS are the keys the filter bar owns, in the order they are
// written back. path: stays a word: it is not a control on the bar.
const FILTER_KEYS = ['ext', 'size', 'dm', 'type'];
const NUMBER = '\\d+(?:\\.\\d+)?[kmg]?b?';
const SIZE = new RegExp('^(>=|<=|>|<)?(' + NUMBER + ')$|^(' + NUMBER + ')\\.\\.(' + NUMBER + ')$', 'i');
const DAY = '\\d{4}(?:-\\d{2}){0,2}';
const DATE = new RegExp('^(>=|<=|>|<)?(' + DAY + ')$|^(' + DAY + ')\\.\\.(' + DAY + ')$');
const EXT = /^\.?[^\s/.,]+$/;

// tokenize splits on whitespace. A word that starts with a double quote (or
// with -" for a negated one) runs to the closing quote, whitespace included,
// and keeps its quotes so the word can be written back as it was; without a
// closing quote the quote is part of a literal word. Quotes inside a word are
// literal, so a"b.txt is still searchable.
export function tokenize(text) {
  const words = [];
  const chars = [...String(text || '')];
  let i = 0;
  while (i < chars.length) {
    if (/\s/.test(chars[i])) { i++; continue; }
    const start = i;
    const open = chars[i] === '-' && chars[i + 1] === '"' ? i + 1 : i;
    if (chars[open] === '"') {
      const end = chars.indexOf('"', open + 1);
      if (end >= 0) {
        words.push(chars.slice(start, end + 1).join(''));
        i = end + 1;
        continue;
      }
    }
    while (i < chars.length && !/\s/.test(chars[i])) i++;
    words.push(chars.slice(start, i).join(''));
  }
  return words;
}

// keyOf reads the key of a key:value word. A quoted word and a negated word
// are never a filter the bar owns: the bar composes positive filters only.
function keyOf(word) {
  if (word.startsWith('"') || word.startsWith('-')) return '';
  const i = word.indexOf(':');
  if (i <= 0) return '';
  const key = word.slice(0, i);
  return KEYS.includes(key) ? key : '';
}

function validValue(key, value) {
  switch (key) {
    case 'size': return SIZE.test(value);
    case 'dm': return DATE.test(value);
    case 'type': return value === 'dir' || value === 'file';
    case 'ext': return value !== '' && value.split(',').every((e) => EXT.test(e));
    default: return true;
  }
}

// parseQueryString returns the words that are not filters, the filters the
// bar can show, and the keys of the values it could not read. An unreadable
// value or a repeated key stays a word — the person sees exactly what will
// be sent, and the daemon answers with its own error — and its key is listed
// in errors so the bar can mark it.
export function parseQueryString(text) {
  const terms = [];
  const filters = {};
  const errors = [];
  for (const word of tokenize(text)) {
    const key = keyOf(word);
    if (!key || key === 'path') { terms.push(word); continue; }
    const value = word.slice(key.length + 1);
    const slot = key === 'type' ? 'kind' : key;
    if (!validValue(key, value) || slot in filters) {
      errors.push(key);
      terms.push(word);
      continue;
    }
    filters[slot] = value;
  }
  return { terms, filters, errors };
}

// buildQueryString writes the words first and then the filters in a fixed
// order, so the bar and the box agree on one spelling of a query. A filter
// that is empty is not written.
export function buildQueryString(terms, filters = {}) {
  const parts = (terms || []).filter((w) => w !== '');
  for (const key of FILTER_KEYS) {
    const value = filters[key === 'type' ? 'kind' : key];
    if (value) parts.push(key + ':' + value);
  }
  return parts.join(' ');
}

// sizeRange writes what the two size inputs of the bar mean as one size:
// value; splitSizeRange reads one back into the inputs.
export function sizeRange(min, max) {
  min = String(min || '').trim();
  max = String(max || '').trim();
  if (min && max) return min + '..' + max;
  if (min) return '>' + min;
  if (max) return '<' + max;
  return '';
}

export function splitSizeRange(text) {
  text = String(text || '').trim();
  if (text.includes('..')) {
    const [min, max] = text.split('..');
    return { min, max };
  }
  if (text.startsWith('>=') || text.startsWith('>')) return { min: text.replace(/^>=?/, ''), max: '' };
  if (text.startsWith('<=') || text.startsWith('<')) return { min: '', max: text.replace(/^<=?/, '') };
  return { min: text, max: text };
}

// searchURL is the one place the /search request is spelled. The whole
// drive is the default: only the "this folder" scope sends a path.
export function searchURL({ query, scope, cwd, sort, limit = 100 }) {
  return '/search?q=' + encodeURIComponent(query)
    + (scope === 'cwd' ? '&path=' + encodeURIComponent(cwd) : '')
    + '&limit=' + limit
    + (sort ? '&sort=' + encodeURIComponent(sort) : '');
}

// needles are the pieces of a name a parsed query would have matched: the
// positive bare words, a quoted literal without its quotes, and for a
// wildcard word its longest run without * or ?. Negated words, key:value
// filters and path words (a bare word with a slash) match something other
// than the name and are not highlighted.
function needles(parsed) {
  const out = [];
  for (const raw of (parsed && parsed.terms) || []) {
    if (raw.startsWith('-') || keyOf(raw)) continue;
    let word = raw;
    if (word.startsWith('"') && word.endsWith('"') && word.length >= 2) word = word.slice(1, -1);
    else if (word.includes('/')) continue;
    if (/[*?]/.test(word)) word = word.split(/[*?]+/).sort((a, b) => b.length - a.length)[0] || '';
    if (word) out.push(word.toLowerCase());
  }
  return out;
}

// highlightParts cuts a name into runs that a query hit and runs it did not,
// case-insensitively and keeping the name's own casing. The result is text
// only: whoever renders it makes <mark> elements around the hits and never
// touches innerHTML, so a file called <b>x</b> stays a file called <b>x</b>.
export function highlightParts(name, parsed) {
  name = String(name);
  const folded = name.toLowerCase();
  const hit = new Array(name.length).fill(false);
  let any = false;
  for (const needle of needles(parsed)) {
    for (let at = folded.indexOf(needle); at >= 0; at = folded.indexOf(needle, at + 1)) {
      for (let i = at; i < at + needle.length; i++) hit[i] = true;
      any = true;
    }
  }
  if (!any) return [{ text: name, hit: false }];
  const parts = [];
  let start = 0;
  for (let i = 1; i <= name.length; i++) {
    if (i === name.length || hit[i] !== hit[start]) {
      parts.push({ text: name.slice(start, i), hit: hit[start] });
      start = i;
    }
  }
  return parts;
}

// pushRecent puts a query at the front of the recent list, once, and keeps
// the list to max entries. Blank input is not a search anyone will want back.
export function pushRecent(list, query, max = 10) {
  const q = String(query || '').trim();
  if (!q) return (list || []).slice();
  return [q, ...(list || []).filter((x) => x !== q)].slice(0, max);
}
