import test from 'node:test';
import assert from 'node:assert/strict';

import { pairConflicts, copiesOf, byteCount, isValidName, splitFrontmatter, mergeDraft, NAME_RE } from '../memory_conflicts.js';

// facts is what GET /memory/{agent} lists (internal/memory FactMeta): the
// fact's own path and the conflict copies the store found beside it.
const dir = '/work/.agent/memory/codex/facts';
const style = { name: 'style', agent: 'codex', path: dir + '/style.md', conflicts: [dir + '/style (conflict 2026-09-15).md', dir + '/style.md.sb-1a2b'] };

test('a copy is a sibling in the same directory whose name starts with the fact name', () => {
  assert.deepEqual(copiesOf(style), [dir + '/style (conflict 2026-09-15).md', dir + '/style.md.sb-1a2b']);
  assert.deepEqual(pairConflicts([style]), [
    { name: 'style', agent: 'codex', path: dir + '/style.md', copy: dir + '/style (conflict 2026-09-15).md' },
    { name: 'style', agent: 'codex', path: dir + '/style.md', copy: dir + '/style.md.sb-1a2b' },
  ]);
});

test('the fact itself, another fact and a file elsewhere are never copies', () => {
  const noisy = { ...style, conflicts: [
    dir + '/style.md',                 // the fact itself
    dir + '/style-guide.md',           // a well-formed fact that shares the prefix
    dir + '/style-v2 (1).md',          // shares the prefix, is not a fact file: a copy
    dir + '/other (1).md',             // another prefix
    '/work/elsewhere/style (1).md',    // right name, wrong directory
    dir + '/sub/style (1).md',         // a subdirectory is not the same directory
    '',
    null,
  ] };
  assert.deepEqual(copiesOf(noisy), [dir + '/style-v2 (1).md']);
});

test('pairing never invents a naming scheme: an unrelated name in the list is dropped', () => {
  const f = { name: 'notes', agent: 'shared', path: '/m/shared/facts/notes.md', conflicts: ['/m/shared/facts/notes.conflict-3', '/m/shared/facts/backup-of-notes.md'] };
  assert.deepEqual(copiesOf(f), ['/m/shared/facts/notes.conflict-3']);
  assert.deepEqual(pairConflicts([]), []);
  assert.deepEqual(pairConflicts(null), []);
  assert.deepEqual(copiesOf({ name: 'x', path: '/a/x.md' }), []);
  assert.deepEqual(copiesOf({ name: '', path: '/a/x.md', conflicts: ['/a/x (1).md'] }), []);
});

test('byteCount is UTF-8 bytes, not characters', () => {
  assert.equal(byteCount(''), 0);
  assert.equal(byteCount('abc'), 3);
  assert.equal(byteCount('\u00e9'), 2);
  assert.equal(byteCount('\u4e2d\u6587'), 6);
  assert.equal(byteCount('\ud83d\ude00'), 4);
  assert.equal(byteCount(null), 0);
});

test('the name grammar is the store\'s: lower-case, digits, dashes, no leading dash, 1 to 64', () => {
  assert.equal(String(NAME_RE), '/^[a-z0-9][a-z0-9-]{0,63}$/');
  for (const ok of ['a', 'style', 'a-b-1', 'a'.repeat(64)]) assert.equal(isValidName(ok), true, ok);
  for (const bad of ['', '-a', 'A', 'a b', 'a'.repeat(65), 'a..b', 'a/b', 'style.md', null, 12]) assert.equal(isValidName(bad), false, String(bad));
});

test('splitFrontmatter separates the fenced block from the body', () => {
  const file = '---\nname: style\ndescription: "How I write: short"\ntype: preference\nupdated_at: 2026-09-15T10:00:00Z\n---\nBe brief.\n';
  assert.deepEqual(splitFrontmatter(file), {
    ok: true,
    meta: { name: 'style', description: 'How I write: short', type: 'preference', updated_at: '2026-09-15T10:00:00Z' },
    body: 'Be brief.\n',
  });
  assert.deepEqual(splitFrontmatter("---\nname: 'it''s'\n---\n"), { ok: true, meta: { name: "it's" }, body: '' });
  // No fence: the whole text is the body and there is no meta.
  assert.deepEqual(splitFrontmatter('just text'), { ok: false, meta: {}, body: 'just text' });
  // An opening fence with no closing one is not frontmatter either.
  assert.deepEqual(splitFrontmatter('---\nname: x\nno end'), { ok: false, meta: {}, body: '---\nname: x\nno end' });
  assert.deepEqual(splitFrontmatter(null), { ok: false, meta: {}, body: '' });
});

test('a merge draft holds both bodies between markers and nothing else', () => {
  const d = mergeDraft('mine\n', 'theirs');
  assert.equal(d, '<<<<<<< mine\nmine\n=======\ntheirs\n>>>>>>> copy\n');
  assert.equal(mergeDraft('', ''), '<<<<<<< mine\n=======\n>>>>>>> copy\n');
});
