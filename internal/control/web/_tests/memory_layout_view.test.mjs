import test from 'node:test';
import assert from 'node:assert/strict';

import { groupByOwner, mergeRows, adoptable } from '../memory_layout_view.js';

test('v1 is one group, v2 groups by owner with me first and shared last', () => {
  const agents = [{ name: 'shared' }, { name: 'bob/codex', owner: 'bob' }, { name: 'alice/claude-code', owner: 'alice' }, { name: 'alice/codex', owner: 'alice' }];
  assert.deepEqual(groupByOwner(agents, 'v1', 'alice'), [{ owner: '', agents }]);
  const g = groupByOwner(agents, 'v2', 'bob');
  assert.deepEqual(g.map((x) => x.owner), ['bob', 'alice', '']);
  assert.deepEqual(g[1].agents.map((a) => a.name), ['alice/claude-code', 'alice/codex']);
  assert.deepEqual(groupByOwner(null, 'v2', 'me'), []);
});

test('merge rows split conflict blocks from agreed lines', () => {
  const rows = mergeRows('a\n<<<<<<< this device\nB\n=======\nX\n>>>>>>> conflict copy\nc');
  assert.deepEqual(rows, [{ kind: 'line', text: 'a' }, { kind: 'conflict', ours: ['B'], theirs: ['X'] }, { kind: 'line', text: 'c' }]);
  assert.deepEqual(mergeRows('<<<<<<< x\nonly'), [{ kind: 'conflict', ours: ['only'], theirs: [] }]);
  assert.deepEqual(mergeRows(''), [{ kind: 'line', text: '' }]);
});

test('a proposal is adoptable only when clean and marker-free', () => {
  assert.equal(adoptable({ clean: true, merged: 'a\nb' }), true);
  assert.equal(adoptable({ clean: false, merged: 'a' }), false);
  assert.equal(adoptable({ clean: true, merged: '<<<<<<< a\nx\n=======\ny\n>>>>>>> b' }), false);
  assert.equal(adoptable(null), false);
});
