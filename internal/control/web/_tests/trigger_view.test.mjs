import test from 'node:test';
import assert from 'node:assert/strict';

import { selfTriggerRisk, argvCells, ruleSummary, deliveryState, splitOutput, looksLikeWebhookOutput, VERIFY_SNIPPET, EXEC_EXAMPLE, WEBHOOK_EXAMPLE } from '../trigger_view.js';

const exec = (origins) => ({ name: 'r', paths: ['/work/**'], origins, action: { type: 'exec', exec: { command: ['/bin/x', '{path}'] } } });
const hook = (origins) => ({ name: 'r', paths: ['/work/**'], origins, action: { type: 'webhook', webhook: { url: 'https://h/', secret_configured: true } } });

test('an exec rule that still listens to api writes can be fired by the agent it runs', () => {
  // The default origins are all of them, so a rule that says nothing about
  // origins is as exposed as one that names api.
  assert.equal(selfTriggerRisk(exec([])), true);
  assert.equal(selfTriggerRisk(exec(undefined)), true);
  assert.equal(selfTriggerRisk(exec(['kernel', 'api'])), true);
  assert.equal(selfTriggerRisk(exec(['api'])), true);
});

test('an exec rule that excludes api is not at risk, and a webhook never is', () => {
  assert.equal(selfTriggerRisk(exec(['kernel', 'remote'])), false);
  assert.equal(selfTriggerRisk(exec(['kernel'])), false);
  assert.equal(selfTriggerRisk(hook([])), false);
  assert.equal(selfTriggerRisk(hook(['api'])), false);
  assert.equal(selfTriggerRisk({ name: 'r', action: { type: 'exec' } }), true);
  assert.equal(selfTriggerRisk(null), false);
});

test('argv cells stay one element each and are never joined', () => {
  const argv = ['echo', '{path}; rm -rf /', ''];
  const cells = argvCells(argv);
  assert.deepEqual(cells, ['echo', '{path}; rm -rf /', '']);
  assert.equal(cells.length, 3);
  // A shell-looking element is left exactly as written, so the reader sees
  // that it is one argument, not a command line.
  assert.equal(cells[1], '{path}; rm -rf /');
  assert.deepEqual(argvCells(undefined), []);
  assert.deepEqual(argvCells('echo hi'), []);
  assert.deepEqual(argvCells([1, null]), ['1', '']);
});

test('a rule summary normalises the lists the daemon may leave empty', () => {
  const s = ruleSummary({ name: 'n', paths: ['/a/**'], events: ['write'], origins: [], debounce: '2s', on_rescan: 'ignore', action: { type: 'exec', exec: { command: ['x'] } } });
  assert.equal(s.name, 'n');
  assert.deepEqual(s.paths, ['/a/**']);
  assert.deepEqual(s.events, ['write']);
  assert.deepEqual(s.origins, []);
  assert.equal(s.action, 'exec');
  assert.equal(s.debounce, '2s');
  assert.equal(s.onRescan, 'ignore');
  const bare = ruleSummary({ name: 'b', action: { type: 'webhook', webhook: { url: 'https://h/' } } });
  assert.deepEqual(bare.paths, []);
  assert.deepEqual(bare.events, []);
  assert.deepEqual(bare.origins, []);
  assert.equal(bare.action, 'webhook');
  assert.equal(bare.debounce, '');
  assert.equal(ruleSummary({}).action, '');
});

test('a delivery state maps to a dot and a catalog key', () => {
  assert.deepEqual(deliveryState({ state: 'pending' }), { dot: '', key: 'triggers.state.pending' });
  assert.deepEqual(deliveryState({ state: 'running' }), { dot: 'warn', key: 'triggers.state.running' });
  assert.deepEqual(deliveryState({ state: 'done' }), { dot: 'ok', key: 'triggers.state.done' });
  assert.deepEqual(deliveryState({ state: 'dead' }), { dot: 'bad', key: 'triggers.state.dead' });
  assert.deepEqual(deliveryState({ state: 'weird' }), { dot: '', key: 'triggers.state.unknown' });
  assert.deepEqual(deliveryState(null), { dot: '', key: 'triggers.state.unknown' });
});

test('the output splits on the runner marker into stdout and stderr', () => {
  assert.deepEqual(splitOutput('out\n--- stderr ---\nerr'), { stdout: 'out', stderr: 'err' });
  assert.deepEqual(splitOutput('only out'), { stdout: 'only out', stderr: '' });
  assert.deepEqual(splitOutput('\n--- stderr ---\n'), { stdout: '', stderr: '' });
  assert.deepEqual(splitOutput(''), { stdout: '', stderr: '' });
  assert.deepEqual(splitOutput(undefined), { stdout: '', stderr: '' });
  // Only the first marker splits: a command that prints the marker itself
  // does not get to move its stdout into the stderr pane.
  assert.deepEqual(splitOutput('a\n--- stderr ---\nb\n--- stderr ---\nc'), { stdout: 'a', stderr: 'b\n--- stderr ---\nc' });
});

test('a webhook output is told by its status line, a note before it included', () => {
  assert.equal(looksLikeWebhookOutput('HTTP 200 OK\nok'), true);
  assert.equal(looksLikeWebhookOutput('download_url unavailable: x\nHTTP 502 Bad Gateway\n'), true);
  // An exec with an empty stderr has no marker; it is still an exec.
  assert.equal(looksLikeWebhookOutput('hello /notes/a.txt\n'), false);
  assert.equal(looksLikeWebhookOutput(''), false);
  assert.equal(looksLikeWebhookOutput(undefined), false);
});

test('the empty-state examples show the safe origins and the keyring secret', () => {
  assert.match(EXEC_EXAMPLE, /origins: \[kernel, remote\]/);
  assert.match(EXEC_EXAMPLE, /command: \[/);
  assert.match(WEBHOOK_EXAMPLE, /secret: keyring:/);
  assert.match(WEBHOOK_EXAMPLE, /url: https:/);
  assert.match(VERIFY_SNIPPET, /X-CloudFS-Timestamp/);
  assert.match(VERIFY_SNIPPET, /X-CloudFS-Signature/);
  assert.match(VERIFY_SNIPPET, /hmac\.Equal/);
  assert.match(VERIFY_SNIPPET, /5\*time\.Minute/);
});
