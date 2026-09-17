// The shape of a rollback plan as the console shows it. A plan is what
// POST /sessions/{id}/rollback answers, for a dry run and for the real
// thing alike: three lists — restored, skipped, conflict — of
// {seq, op, path, to_path, reason}. The preview and the result overlay
// both draw those three groups, so the grouping lives here, with no DOM and
// no imports, and runs under node in _tests/rollback_plan.test.mjs.

// groupPlan sorts a plan into the three groups the overlays draw. Every
// group is a fresh array, present even when the daemon sent nothing for
// it (an empty plan is three empty groups), so a renderer never branches
// on a missing list.
export function groupPlan(plan) {
  const p = plan || {};
  return { restore: list(p.restored), skip: list(p.skipped), conflict: list(p.conflict) };
}

function list(v) {
  return Array.isArray(v) ? v.slice() : [];
}

// planCounts is the three sizes and their sum, for headings and toasts.
export function planCounts(plan) {
  const g = groupPlan(plan);
  return { restore: g.restore.length, skip: g.skip.length, conflict: g.conflict.length, total: g.restore.length + g.skip.length + g.conflict.length };
}

// shortID is the first eight characters of a session id: what the
// confirmation asks a person to type, and what a row shows beside a client
// name. Session ids are UUIDs, so eight hex characters name one session
// out of anything a single daemon will ever hold.
export function shortID(id) {
  return String(id || '').slice(0, 8);
}

// The order skipped items are grouped in: the reasons a person can act on
// first (a copy that expired, a file over a budget), then the ones that
// are simply facts about the write, then anything the daemon says in
// words. Unknown reasons keep their text as the group key.
const SKIP_ORDER = ['expired', 'too_large', 'too_many', 'not_cached', 'dir', 'already', 'not_empty', 'missing', 'incomplete', 'exists', 'from_exists'];

// groupSkipped sorts the skip group by reason: an array of {reason, items}
// in SKIP_ORDER, unknown reasons after in first-seen order, items without a
// reason last under ''. The preview overlay draws one sub-list per entry.
export function groupSkipped(skipped) {
  const items = list(skipped);
  const by = new Map();
  for (const it of items) {
    const key = it && typeof it.reason === 'string' ? it.reason : '';
    if (!by.has(key)) by.set(key, []);
    by.get(key).push(it);
  }
  const out = [];
  for (const r of SKIP_ORDER) if (by.has(r)) { out.push({ reason: r, items: by.get(r) }); by.delete(r); }
  for (const [r, its] of by) if (r !== '') out.push({ reason: r, items: its });
  if (by.has('')) out.push({ reason: '', items: by.get('') });
  return out;
}

// reversibility is what a session op's row says about undoing it: ok when
// a copy of the previous content is held (or the path did not exist, which
// a rollback undoes by removing it), warn with the reason when the copy is
// missing or has gone, and none when the write predates the record. The
// key is a rollback.pre.* phrase.
export function reversibility(op) {
  const o = op || {};
  if (o.pre_reason === 'not_recorded') return { state: 'none', key: 'rollback.pre.not_recorded' };
  if (o.pre_reason) return { state: 'warn', key: 'rollback.pre.' + o.pre_reason };
  if (o.pre_state === 'absent') return { state: 'ok', key: 'rollback.pre.absent' };
  if (o.pre_state === 'dir') return { state: 'warn', key: 'rollback.pre.dir' };
  return { state: 'ok', key: 'rollback.pre.ok' };
}
