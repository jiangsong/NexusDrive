import test from 'node:test';
import assert from 'node:assert/strict';

import { groupPlan, planCounts, shortID } from '../rollback_plan.js';

const plan = {
  session_id: 'a1b2c3d4-e5f6-7890-abcd-ef1234567890',
  dry_run: true,
  restored: [{ seq: 3, op: 'overwrite', path: '/work/a.md' }, { seq: 1, op: 'create', path: '/work/new.md' }],
  skipped: [{ seq: 2, op: 'delete', path: '/work/big.bin', reason: 'too_large' }],
  conflict: [{ seq: 4, op: 'edit', path: '/work/b.md', reason: 'modified' }],
};

test('the three outcomes become three groups in plan order', () => {
  const g = groupPlan(plan);
  assert.deepEqual(Object.keys(g), ['restore', 'skip', 'conflict']);
  assert.deepEqual(g.restore.map((i) => i.seq), [3, 1]);
  assert.deepEqual(g.skip.map((i) => i.path), ['/work/big.bin']);
  assert.deepEqual(g.conflict.map((i) => i.path), ['/work/b.md']);
});

test('reasons and rename targets are kept on the items', () => {
  const g = groupPlan({ restored: [], skipped: [{ seq: 1, op: 'rename', path: '/a', to_path: '/b', reason: 'from_exists' }], conflict: [] });
  assert.equal(g.skip[0].reason, 'from_exists');
  assert.equal(g.skip[0].to_path, '/b');
  assert.deepEqual(groupPlan(plan).conflict[0].reason, 'modified');
});

test('an empty plan is three empty groups', () => {
  assert.deepEqual(groupPlan({}), { restore: [], skip: [], conflict: [] });
  assert.deepEqual(groupPlan(null), { restore: [], skip: [], conflict: [] });
  assert.deepEqual(groupPlan(undefined), { restore: [], skip: [], conflict: [] });
  // A daemon that sends null for a missing slice is tolerated too.
  assert.deepEqual(groupPlan({ restored: null, skipped: undefined }), { restore: [], skip: [], conflict: [] });
});

test('groups do not alias the plan', () => {
  const p = { restored: [{ seq: 1, op: 'create', path: '/x' }], skipped: [], conflict: [] };
  const g = groupPlan(p);
  g.restore.push({ seq: 2 });
  assert.equal(p.restored.length, 1);
});

test('planCounts counts each group', () => {
  assert.deepEqual(planCounts(plan), { restore: 2, skip: 1, conflict: 1, total: 4 });
  assert.deepEqual(planCounts({}), { restore: 0, skip: 0, conflict: 0, total: 0 });
});

test('shortID is the first eight characters', () => {
  assert.equal(shortID('a1b2c3d4-e5f6-7890-abcd-ef1234567890'), 'a1b2c3d4');
  assert.equal(shortID('abc'), 'abc');
  assert.equal(shortID(''), '');
  assert.equal(shortID(undefined), '');
});
