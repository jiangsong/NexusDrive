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
