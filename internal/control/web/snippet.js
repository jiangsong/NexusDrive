// Snippets of file content for the content search: which runs of a
// snippet the query hit, and a window of at most so many characters around
// the first hit. The text comes from files on the drive, so nothing here
// produces markup: every function returns strings or { text, hit } parts
// that the caller appends as text nodes. The module has no imports and
// touches no DOM, so it runs under node.
//
// Lengths are counted in code points, never in UTF-16 units or bytes: a
// window cut inside a surrogate pair would show a broken character, and a
// CJK character is one character whatever its byte length.

// terms splits a query into the words to highlight. Double quotes group a
// phrase, matching what MatchQuery on the daemon side treats as one phrase;
// a bare word is one term. Case is folded per code point (see foldCase).
function terms(query) {
  const out = [];
  const re = /"([^"]*)"|(\S+)/g;
  let m;
  while ((m = re.exec(String(query || ''))) !== null) {
    const w = (m[1] !== undefined ? m[1] : m[2]).trim();
    if (w) out.push(w);
  }
  return out;
}

// foldCase lowercases a string one code point at a time and keeps a code
// point whose lowercase form has a different UTF-16 length as it is. The
// result is then index-for-index aligned with the input, so a match found
// in the folded string slices the original at the same positions. (A few
// letters, such as İ, lowercase to more units than they occupy.)
function foldCase(s) {
  let out = '';
  for (const cp of s) {
    const low = cp.toLowerCase();
    out += low.length === cp.length ? low : cp;
  }
  return out;
}

// truncateRunes cuts text to at most max code points and marks the cut
// with an ellipsis. A surrogate pair is one code point, so it is either
// kept whole or dropped whole.
export function truncateRunes(text, max) {
  const cps = Array.from(String(text));
  return cps.length <= max ? cps.join('') : cps.slice(0, max).join('') + '…';
}

// highlightParts cuts text into the runs a query hit and the runs it did
// not, in order, as [{ text, hit }]. Matching is case-insensitive and keeps
// the original casing in the parts. Every term of the query is looked for;
// at each position the earliest match wins and a longer one at the same
// position wins over a shorter one. An empty query gives one plain part.
export function highlightParts(text, query) {
  const s = String(text);
  const words = terms(query).map(foldCase).filter(Boolean);
  if (!words.length || !s) return [{ text: s, hit: false }];
  const folded = foldCase(s);
  const parts = [];
  let i = 0;
  for (;;) {
    let at = -1;
    let len = 0;
    for (const w of words) {
      const j = folded.indexOf(w, i);
      if (j < 0) continue;
      if (at < 0 || j < at || (j === at && w.length > len)) { at = j; len = w.length; }
    }
    if (at < 0) break;
    if (at > i) parts.push({ text: s.slice(i, at), hit: false });
    parts.push({ text: s.slice(at, at + len), hit: true });
    i = at + len;
  }
  if (i < s.length) parts.push({ text: s.slice(i), hit: false });
  return parts;
}

// snippetParts is highlightParts over a window of at most max code points
// centred on the first hit, with an ellipsis on each side that was cut. A
// snippet without a hit shows its beginning.
export function snippetParts(text, query, max = 240) {
  const s = String(text);
  const cps = Array.from(s);
  if (cps.length <= max) return highlightParts(s, query);
  const parts = highlightParts(s, query);
  const first = parts.findIndex((p) => p.hit);
  let start = 0;
  if (first > 0) {
    const before = parts.slice(0, first).reduce((n, p) => n + Array.from(p.text).length, 0);
    const hitLen = Math.min(Array.from(parts[first].text).length, max);
    start = Math.min(Math.max(0, before - Math.floor((max - hitLen) / 2)), cps.length - max);
  }
  const window = cps.slice(start, start + max).join('');
  const lead = start > 0 ? '\u2026' : '';
  const tail = start + max < cps.length ? '\u2026' : '';
  return highlightParts(lead + window + tail, query);
}
