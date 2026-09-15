// The decisions of the memory tab that need no DOM (docs/ui-plan.md F7-3,
// TODO.md T-40): which sibling file is a conflict copy of which fact, how
// many bytes a body is against memory.max_fact_bytes, whether a name fits
// the fact grammar, and what the "merge by hand" draft looks like. No
// imports, so node runs it directly (web/_tests/memory_conflicts.test.mjs).
//
// Pairing follows the store (internal/memory/conflicts.go) and nothing
// more: a conflict copy of <name> is a file in the same facts/ directory
// whose name starts with <name> and is not itself a well-formed fact file.
// Every drive names its copies differently — "style (conflict 2026-09-15)
// .md", "style.md.sb-1a2b", "style (1).md" — and neither side guesses the
// scheme; the daemon lists the candidates, this module keeps the ones that
// sit beside the fact.

// NAME_RE is the fact and agent name grammar, character for character the
// store's memory.ValidName: lower-case, digits, dashes, 1 to 64, no leading
// dash. A test compiles this literal in Go and compares the two.
export const NAME_RE = /^[a-z0-9][a-z0-9-]{0,63}$/;

// FACT_FILE_RE is a well-formed fact file name: <valid name>.md.
const FACT_FILE_RE = /^[a-z0-9][a-z0-9-]{0,63}\.md$/;

export function isValidName(name) {
  return typeof name === 'string' && NAME_RE.test(name);
}

function dirOf(p) {
  const i = p.lastIndexOf('/');
  return i < 0 ? '' : p.slice(0, i);
}

function baseOf(p) {
  return p.slice(p.lastIndexOf('/') + 1);
}

// copiesOf keeps, from a fact's conflicts[], the paths that are a copy of
// it: same directory, basename starting with the name, not the fact's own
// file and not another fact.
export function copiesOf(fact) {
  if (!fact || !isValidName(fact.name) || typeof fact.path !== 'string') return [];
  const dir = dirOf(fact.path);
  const out = [];
  for (const c of Array.isArray(fact.conflicts) ? fact.conflicts : []) {
    if (typeof c !== 'string' || !c) continue;
    if (dirOf(c) !== dir) continue;
    const base = baseOf(c);
    if (!base.startsWith(fact.name) || base === fact.name + '.md' || FACT_FILE_RE.test(base)) continue;
    out.push(c);
  }
  return out;
}

// pairConflicts flattens a listing into one row per (fact, copy) pair, in
// listing order; a fact without copies contributes nothing.
export function pairConflicts(facts) {
  const out = [];
  for (const f of Array.isArray(facts) ? facts : []) {
    for (const copy of copiesOf(f)) out.push({ name: f.name, agent: f.agent, path: f.path, copy });
  }
  return out;
}

// byteCount is the UTF-8 length of a string, which is what the daemon's
// max_fact_bytes counts; a character count would let a body of CJK text
// pass here and be refused there.
export function byteCount(str) {
  if (typeof str !== 'string' || !str) return 0;
  if (typeof TextEncoder === 'function') return new TextEncoder().encode(str).length;
  let n = 0;
  for (let i = 0; i < str.length; i++) {
    const c = str.charCodeAt(i);
    if (c < 0x80) n += 1;
    else if (c < 0x800) n += 2;
    else if (c >= 0xd800 && c < 0xdc00 && i + 1 < str.length) { n += 4; i++; }
    else n += 3;
  }
  return n;
}

// yamlScalar undoes what the store's render() does to one value: a
// double-quoted scalar is close enough to a JSON string to parse as one, a
// single-quoted one doubles its quotes, anything else is plain. This is a
// subset of YAML and is only ever applied to a file the store wrote.
function yamlScalar(raw) {
  const s = raw.trim();
  if (s.startsWith('"') && s.endsWith('"') && s.length >= 2) {
    try { return JSON.parse(s); } catch (_) { return s.slice(1, -1); }
  }
  if (s.startsWith("'") && s.endsWith("'") && s.length >= 2) return s.slice(1, -1).replace(/''/g, "'");
  return s;
}

// splitFrontmatter separates a fact file into its fenced block and its
// body, the way parseFrontmatter does in the store: ok is false, and the
// whole text is the body, when there is no opening fence or no closing
// one. It exists so the conflict overlay can show a copy's body beside the
// fact's and put that body — not the whole file — back as the content.
export function splitFrontmatter(text) {
  const none = { ok: false, meta: {}, body: typeof text === 'string' ? text : '' };
  if (typeof text !== 'string' || !text.startsWith('---\n')) return none;
  const rest = text.slice(4);
  let end;
  if (rest.startsWith('---\n') || rest === '---') end = 0;
  else {
    const i = rest.indexOf('\n---\n');
    if (i >= 0) end = i + 1;
    else if (rest.endsWith('\n---')) end = rest.length - 3;
    else return none;
  }
  const meta = {};
  for (const line of rest.slice(0, end).split('\n')) {
    const colon = line.indexOf(':');
    if (colon <= 0) continue;
    const key = line.slice(0, colon).trim();
    if (!/^[a-z_]+$/.test(key)) continue;
    meta[key] = yamlScalar(line.slice(colon + 1));
  }
  let after = 4 + end + 3;
  if (after < text.length && text[after] === '\n') after++;
  return { ok: true, meta, body: text.slice(Math.min(after, text.length)) };
}

// mergeDraft is what "merge by hand" puts in the editor: both bodies
// between the markers every merge tool uses, so the person sees exactly
// where each side starts and the fact is not saved until they resolve it.
export function mergeDraft(mine, copy) {
  const a = typeof mine === 'string' ? mine : '';
  const b = typeof copy === 'string' ? copy : '';
  const nl = (s) => (s && !s.endsWith('\n') ? s + '\n' : s);
  return '<<<<<<< mine\n' + nl(a) + '=======\n' + nl(b) + '>>>>>>> copy\n';
}
