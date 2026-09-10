import test from 'node:test';
import assert from 'node:assert/strict';

import { authFailureMode } from '../auth_policy.js';

// The statuses are the daemon's, from internal/control/auth.go: 400
// err.auth_terminal_only, 501 err.auth_unwired, 409 err.auth_in_progress and
// err.auth_port_busy, 502 err.auth_start_failed.

test('a backend with no browser flow falls through to the terminal command', () => {
  // 400 err.auth_terminal_only: the credential is a password or an externally
  // issued token, so there is no authorization server to drive. 501
  // err.auth_unwired: this build has no browser flow for the type at all.
  // Both mean "there was never going to be a browser flow", and the exact
  // command to run is the honest answer.
  assert.equal(authFailureMode(400), 'terminal');
  assert.equal(authFailureMode(501), 'terminal');
});

test('a refused start is shown, not disguised as a backend without a browser flow', () => {
  // Swallowing every failure printed "run this in a terminal" for a drive
  // that has a perfectly good browser flow. The 409s are the one-at-a-time
  // rule — another authorization holds the one loopback callback port — and
  // 502 is the provider refusing to start one. A person who is told to open
  // a terminal instead never learns to finish the flow already running.
  assert.equal(authFailureMode(409), 'surface');
  assert.equal(authFailureMode(502), 'surface');
  assert.equal(authFailureMode(500), 'surface');
  assert.equal(authFailureMode(403), 'surface');
});

test('a failure that carried no status at all is shown too', () => {
  // A dropped connection throws a TypeError, not an ApiError: there is no
  // status to reason from, and guessing "no browser flow" from a network
  // fault is the same lie in a different hat.
  assert.equal(authFailureMode(undefined), 'surface');
  assert.equal(authFailureMode(0), 'surface');
  assert.equal(authFailureMode('400'), 'surface');
});
