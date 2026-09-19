import test from 'node:test';
import assert from 'node:assert/strict';

import { setupProgress } from '../setup_plan.js';

const unauthorized = { remotes: [{ name: 'gdrive1', type: 'gdrive', has_credentials: false }] };
const authorized = { remotes: [{ name: 'gdrive1', type: 'gdrive', has_credentials: true }] };
const none = { mounts: [] };

test('a served mount means setup is over, whatever else the configuration lacks', () => {
  const got = setupProgress({ accounts: unauthorized, pools: { pools: [] }, mounts: none, served: [{ path: '/home/x/CloudFS' }] });
  assert.equal(got.show, false);
});

test('no drive at all points at the first step', () => {
  const got = setupProgress({ accounts: { remotes: [] }, pools: { pools: [] }, mounts: none, served: null });
  assert.equal(got.show, true);
  assert.equal(got.key, 'setup.banner.drives');
});

test('a drive without a credential names the drive to connect', () => {
  const got = setupProgress({ accounts: unauthorized, pools: { pools: [] }, mounts: none, served: null });
  assert.equal(got.key, 'setup.banner.connect');
  assert.deepEqual(got.args, ['gdrive1']);
});

test('every drive connected but nothing mounted points at building the pool', () => {
  const got = setupProgress({ accounts: authorized, pools: { pools: [] }, mounts: none, served: null });
  assert.equal(got.key, 'setup.banner.pool');
});

test('a declared mount nobody serves is the restart step, and names the folder', () => {
  const got = setupProgress({
    accounts: authorized, pools: { pools: [{ name: 'home' }] },
    mounts: { mounts: [{ path: '/home/x/CloudFS', remote: 'home', active: false }] }, served: null,
  });
  assert.equal(got.show, true);
  assert.equal(got.key, 'setup.banner.restart');
  assert.deepEqual(got.args, ['/home/x/CloudFS']);
});

test('a status body whose mounts is null, as the setup process sends, is not a served mount', () => {
  const got = setupProgress({ accounts: authorized, pools: { pools: [] }, mounts: none, served: { mounts: null } });
  assert.equal(got.show, true);
});

test('an excluded drive does not keep the banner on the connect step', () => {
  const got = setupProgress({
    accounts: { remotes: [{ name: 'a', type: 'gdrive', has_credentials: true }, { name: 'old', type: 'box', has_credentials: false }] },
    pools: { pools: [] }, mounts: none, served: null, intent: { drives: [{ name: 'a', type: 'gdrive' }], excluded: ['old'] },
  });
  assert.equal(got.key, 'setup.banner.pool');
});
