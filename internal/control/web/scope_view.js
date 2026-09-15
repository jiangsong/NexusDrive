// What a session or token scope means, in words. The daemon's Scope is a
// pair of prefix lists plus three modifiers; a table cell has room for one
// line. This module turns the JSON into the parts of that line — translation
// keys with their arguments, so the screen can render them through t() — and
// holds the path rules a token form validates against before it asks the
// daemon. No imports: it runs under node --test exactly as it runs in the page.

// cleanPath is the browser's copy of the daemon's Normalise: absolute, no
// dot segments, no trailing slash, root spelt "/".
export function cleanPath(p) {
  const parts = [];
  for (const seg of String(p == null ? '' : p).split('/')) {
    if (seg === '' || seg === '.') continue;
    if (seg === '..') { parts.pop(); continue; }
    parts.push(seg);
  }
  return '/' + parts.join('/');
}

// under says whether p lies inside prefix. The comparison is on path
// components, so /workshop is not under /work; "/" covers everything.
export function under(p, prefix) {
  const a = cleanPath(p);
  const b = cleanPath(prefix);
  if (b === '/') return true;
  return a === b || a.startsWith(b + '/');
}

// parsePrefixes reads a textarea of one prefix per line: cleaned, blank
// lines dropped, duplicates after cleaning removed, first spelling kept.
export function parsePrefixes(text) {
  const out = [];
  for (const line of String(text == null ? '' : text).split('\n')) {
    const raw = line.trim();
    if (!raw) continue;
    const p = cleanPath(raw);
    if (!out.includes(p)) out.push(p);
  }
  return out;
}

// zeroTime is what encoding/json writes for a time.Time that was never set;
// omitempty does not skip it, so the page has to.
function zeroTime(s) {
  return !s || String(s).startsWith('0001-01-01');
}

function list(v) {
  return Array.isArray(v) ? v.filter((x) => typeof x === 'string' && x !== '') : [];
}

// scopeParts is the readable summary: an ordered list of { key, args } the
// screen joins after translating each one. An empty read list is the whole
// mount; a null write list means "same as read" and is not repeated; an
// empty non-null write list, or read_only, reads as read only.
export function scopeParts(scope) {
  const s = scope || {};
  const read = list(s.read);
  const write = Array.isArray(s.write) ? list(s.write) : null;
  const parts = [];
  if (read.length) parts.push({ key: 'scope.read', args: [read.join(', ')] });
  else parts.push({ key: 'scope.all', args: [] });
  if (s.read_only || (write !== null && write.length === 0)) parts.push({ key: 'scope.readonly', args: [] });
  else if (write !== null) parts.push({ key: 'scope.write', args: [write.join(', ')] });
  if (s.sandbox) parts.push({ key: 'scope.sandbox', args: [String(s.sandbox)] });
  if (!zeroTime(s.expires_at)) parts.push({ key: 'scope.expires', args: [String(s.expires_at)] });
  return parts;
}

// validateTokenScope is the check a token form runs before the request:
// every prefix absolute, and every write prefix inside some read prefix (an
// empty read list is the whole mount, so anything is inside it). It returns
// the key of the message to show, or '' when the scope is fine.
export function validateTokenScope({ read, write }) {
  const r = list(read);
  const w = list(write);
  for (const p of [...r, ...w]) {
    if (!p.startsWith('/')) return 'tokens.err.relative';
  }
  if (r.length === 0) return '';
  for (const p of w) {
    if (!r.some((prefix) => under(p, prefix))) return 'tokens.err.write_outside_read';
  }
  return '';
}
