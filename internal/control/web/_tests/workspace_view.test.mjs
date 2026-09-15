import test from 'node:test';
import assert from 'node:assert/strict';

import { workspaceMark, sessionDirOf } from '../workspace_view.js';

const ws = '/work/.agent';

test('the workspace root is marked as root', () => {
  assert.equal(workspaceMark('/work/.agent', ws), 'root');
  // The daemon canonicalises the workspace; a trailing slash or a dot
  // segment on either side must not hide the mark.
  assert.equal(workspaceMark('/work/.agent/', ws), 'root');
  assert.equal(workspaceMark('/work/./.agent', ws + '/'), 'root');
});

test('a direct child directory is a session directory', () => {
  assert.equal(workspaceMark('/work/.agent/codex-20260914-ab12cd34', ws), 'session');
});

test('deeper paths and unrelated paths carry no mark', () => {
  assert.equal(workspaceMark('/work/.agent/codex-20260914-ab12cd34/out.md', ws), '');
  assert.equal(workspaceMark('/work/.agentx', ws), '');
  assert.equal(workspaceMark('/work', ws), '');
  assert.equal(workspaceMark('/work/.agent', ''), '');
  assert.equal(workspaceMark('/work/.agent', undefined), '');
});

test('anything inside a session directory resolves to that directory', () => {
  assert.equal(sessionDirOf('/work/.agent/codex-20260914-ab12cd34/a/b.md', ws), '/work/.agent/codex-20260914-ab12cd34');
  assert.equal(sessionDirOf('/work/.agent/codex-20260914-ab12cd34', ws), '/work/.agent/codex-20260914-ab12cd34');
  assert.equal(sessionDirOf('/work/.agent', ws), '');
  assert.equal(sessionDirOf('/work/notes.md', ws), '');
  assert.equal(sessionDirOf('/work/.agent/x', ''), '');
});

test('a parent-directory segment cannot escape the workspace', () => {
  assert.equal(sessionDirOf('/work/.agent/../notes.md', ws), '');
  assert.equal(workspaceMark('/work/.agent/s/..', ws), 'root');
});
