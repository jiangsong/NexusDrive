import test from 'node:test';
import assert from 'node:assert/strict';

import { yamlValue, yamlBlock, readTextTokens, sections } from '../settings_view.js';

test('yaml scalars and lists render the way the file accepts them', () => {
  assert.equal(yamlValue(true), 'true');
  assert.equal(yamlValue(20000), '20000');
  assert.equal(yamlValue(''), '""');
  assert.equal(yamlValue('127.0.0.1:8765'), '127.0.0.1:8765');
  assert.equal(yamlValue('a b'), '"a b"');
  assert.equal(yamlValue(['/work', '/x y']), '[/work, "/x y"]');
  assert.equal(yamlValue([]), '[]');
});

test('a block nests plain objects with two-space indentation', () => {
  assert.equal(yamlBlock('mcp', { http: '', limits: { max_tokens: 1 }, allow: ['/w'] }), 'mcp:\n  http: ""\n  limits:\n    max_tokens: 1\n  allow: [/w]');
});

test('the read_text budget hint follows max_tokens: default, explicit, off', () => {
  assert.equal(readTextTokens(0), 20000);
  assert.equal(readTextTokens(undefined), 20000);
  assert.equal(readTextTokens(12000), 12000);
  assert.equal(readTextTokens(-1), 0);
});

test('sections mirror the /settings answer and tolerate an empty one', () => {
  const s = sections({ mcp: { http: '127.0.0.1:8765', allow: ['/work'], read_only: false, max_tokens: 12000, install_transport: 'auto', audit_retain: '2160h',
    session: { idle: '30m', retain: '720h', retain_blobs: '168h', max_preimage_bytes: 33554432, preimage_files: 500 } },
    hooks: { context: 'full', changed_max: 20, memory_head_lines: 30 }, memory: { root: '/work/.agent', max_fact_bytes: 65536 }, index: { enabled: true, pinned: false, rules: 2 }, heat: { enabled: true } });
  assert.deepEqual(s.map((x) => x.id), ['mcp', 'session', 'hooks', 'heat', 'memory', 'index']);
  const mcp = s[0];
  const mt = mcp.rows.find((r) => r.key === 'settings.mcp.max_tokens');
  assert.equal(mt.value, 12000);
  assert.deepEqual(mt.hint, { key: 'settings.mcp.max_tokens.hint', arg: 12000 });
  assert.ok(mcp.yaml.includes('  limits:\n    max_tokens: 12000'));
  assert.ok(mcp.yaml.includes('  install:\n    transport: auto'));
  assert.ok(s[1].yaml.includes('    retain_blobs: 168h'));
  assert.equal(s[2].rows[1].builtin, true);
  assert.equal(s[2].yaml, 'hooks:\n  context: full');
  assert.equal(s[3].yaml, '');
  const empty = sections(null);
  assert.equal(empty.length, 6);
  assert.deepEqual(empty[0].rows.find((r) => r.key === 'settings.mcp.max_tokens').hint, { key: 'settings.mcp.max_tokens.hint', arg: 20000 });
});
