import test from 'node:test';
import assert from 'node:assert/strict';

import { planSetup, nextAuthTarget } from '../setup_plan.js';

// The bodies below are shaped like the endpoints answer: GET /accounts is
// {configurable, config_path, types, remotes}, GET /pool/status is {pools,
// configurable, candidates}, GET /mounts is {mounts:[{path, prefix, remote,
// mode, active}]}. planSetup is handed those bodies whole, so a fixture that
// drifts from the Go structs fails here rather than on a NAS.
const accountsWith = (...remotes) => ({
  configurable: true,
  config_path: '/etc/cloudfs/config.yaml',
  types: [{ type: 'gdrive', fields: [], browser_auth: true }, { type: 'dropbox', fields: [], browser_auth: true }],
  remotes,
});

const intentOf = (...drives) => ({ drives, replicas: 2, mountPath: '/mnt/cloud' });

test('a pool with an active mount pointing at it is the finished wizard', () => {
  const got = planSetup({
    accounts: accountsWith({ name: 'gd', type: 'gdrive', has_credentials: true }),
    pools: { pools: [{ name: 'home', replicas: 2 }], configurable: true },
    mounts: { mounts: [{ path: '/mnt/cloud', prefix: '/', remote: 'home', mode: 'writeback', active: true }] },
    intent: intentOf({ name: 'gd', type: 'gdrive' }),
  });
  assert.equal(got.step, 5);
  assert.equal(got.finished, true);
});

test('a pool nobody is serving yet leaves the restart step pending', () => {
  // The pool is in the configuration but this process still serves the layout
  // it started with, so the wizard has to ask for the restart.
  const got = planSetup({
    accounts: accountsWith({ name: 'gd', type: 'gdrive', has_credentials: true }),
    pools: { pools: [{ name: 'home', replicas: 2 }], configurable: true },
    mounts: { mounts: [{ path: '/mnt/cloud', prefix: '/', remote: 'home', mode: 'writeback', active: false }] },
    intent: intentOf({ name: 'gd', type: 'gdrive' }),
  });
  assert.equal(got.step, 5);
  assert.equal(got.finished, false);
});

test('a mount serving some other remote does not finish the pool', () => {
  const got = planSetup({
    accounts: accountsWith({ name: 'gd', type: 'gdrive', has_credentials: true }),
    pools: { pools: [{ name: 'home', replicas: 2 }], configurable: true },
    mounts: { mounts: [{ path: '/mnt/gd', prefix: '/', remote: 'gd', mode: 'writeback', active: true }] },
    intent: intentOf({ name: 'gd', type: 'gdrive' }),
  });
  assert.equal(got.step, 5);
  assert.equal(got.finished, false);
});

test('every intended drive connected sends the wizard to the redundancy step', () => {
  const got = planSetup({
    accounts: accountsWith(
      { name: 'gd', type: 'gdrive', has_credentials: true },
      { name: 'db', type: 'dropbox', has_credentials: true },
    ),
    pools: { pools: [], configurable: true },
    mounts: { mounts: [] },
    intent: intentOf({ name: 'gd', type: 'gdrive' }, { name: 'db', type: 'dropbox' }),
  });
  assert.equal(got.step, 3);
  assert.equal(got.drives.length, 2);
  assert.deepEqual(got.drives.map((d) => d.authorized), [true, true]);
});

test('an intended drive that exists without credentials sends the wizard back to connect it', () => {
  const got = planSetup({
    accounts: accountsWith(
      { name: 'gd', type: 'gdrive', has_credentials: true },
      { name: 'db', type: 'dropbox' },
    ),
    pools: { pools: [], configurable: true },
    mounts: { mounts: [] },
    intent: intentOf({ name: 'gd', type: 'gdrive' }, { name: 'db', type: 'dropbox' }),
  });
  assert.equal(got.step, 2);
  const db = got.drives.find((d) => d.name === 'db');
  assert.deepEqual({ exists: db.exists, authorized: db.authorized }, { exists: true, authorized: false });
  assert.equal(nextAuthTarget(got.drives), 'db', 'the drives must say which one is next');
});

test('an intended drive that is not a remote at all sends the wizard to the connect step', () => {
  const got = planSetup({
    accounts: accountsWith({ name: 'gd', type: 'gdrive', has_credentials: true }),
    pools: { pools: [], configurable: true },
    mounts: { mounts: [] },
    intent: intentOf({ name: 'gd', type: 'gdrive' }, { name: 'db', type: 'dropbox' }),
  });
  assert.equal(got.step, 2);
  const db = got.drives.find((d) => d.name === 'db');
  assert.deepEqual({ exists: db.exists, authorized: db.authorized }, { exists: false, authorized: false });
  assert.equal(db.type, 'dropbox', 'a drive the server does not have keeps the type the plan chose');
});

test('a remote the saved plan never mentioned is adopted rather than ignored', () => {
  // `cloudfs config add` in a terminal, or a re-exec that dropped the tab's
  // memory, leaves the server holding drives the plan does not list. Ignoring
  // them would build a pool that quietly excludes a drive already paid for.
  const got = planSetup({
    accounts: accountsWith(
      { name: 'gd', type: 'gdrive', has_credentials: true },
      { name: 'box', type: 'box', has_credentials: true },
    ),
    pools: { pools: [], configurable: true },
    mounts: { mounts: [] },
    intent: intentOf({ name: 'gd', type: 'gdrive' }),
  });
  assert.equal(got.resumed, true);
  assert.deepEqual(got.drives.map((d) => d.name), ['gd', 'box']);
  const box = got.drives.find((d) => d.name === 'box');
  assert.deepEqual({ exists: box.exists, authorized: box.authorized, type: box.type }, { exists: true, authorized: true, type: 'box' });
  assert.equal(got.step, 3);
});

test('the server decides a drive type the saved plan disagrees with', () => {
  const got = planSetup({
    accounts: accountsWith({ name: 'gd', type: 'dropbox', has_credentials: true }),
    pools: { pools: [], configurable: true },
    mounts: { mounts: [] },
    intent: intentOf({ name: 'gd', type: 'gdrive' }),
  });
  assert.equal(got.drives[0].type, 'dropbox');
});

test('a blank first run starts at the welcome step', () => {
  const got = planSetup({
    accounts: accountsWith(),
    pools: { pools: [], configurable: true },
    mounts: { mounts: [] },
    intent: null,
  });
  assert.deepEqual(got, { step: 0, drives: [], resumed: false, finished: false });
});

test('a saved plan that chose nothing is still a first run', () => {
  const got = planSetup({ accounts: accountsWith(), pools: { pools: [] }, mounts: { mounts: [] }, intent: { drives: [] } });
  assert.equal(got.step, 0);
  assert.equal(got.resumed, false);
});

test('missing and misshapen state plans a first run instead of throwing', () => {
  // Every one of these is a real first paint: the fetches have not landed, the
  // daemon is mid re-exec, or the endpoint answered an error body.
  for (const args of [{}, { accounts: undefined, pools: undefined, mounts: undefined, intent: null },
    { accounts: {}, pools: {}, mounts: {}, intent: {} },
    { accounts: { remotes: null }, pools: { pools: null }, mounts: { mounts: null }, intent: { drives: null } },
    { accounts: 'nonsense', pools: 7, mounts: false, intent: 'gone' }]) {
    const got = planSetup(args);
    assert.equal(got.step, 0);
    assert.deepEqual(got.drives, []);
    assert.equal(got.finished, false);
  }
  assert.equal(planSetup().step, 0, 'called with no argument at all');
});

test('the endpoint bodies are also accepted as bare arrays', () => {
  // /mounts and /pool/status answer objects, but every caller in the app
  // unwraps them with `.mounts || []` before passing them on. Taking both
  // shapes means a caller that unwraps first is not a blank wizard.
  const got = planSetup({
    accounts: accountsWith({ name: 'gd', type: 'gdrive', has_credentials: true }),
    pools: [{ name: 'home', replicas: 2 }],
    mounts: [{ path: '/mnt/cloud', prefix: '/', remote: 'home', active: true }],
    intent: intentOf({ name: 'gd', type: 'gdrive' }),
  });
  assert.equal(got.step, 5);
  assert.equal(got.finished, true);
});

test('planning never mutates the state it was given', () => {
  const remote = Object.freeze({ name: 'gd', type: 'gdrive', has_credentials: true });
  const accounts = Object.freeze({ configurable: true, types: [], remotes: Object.freeze([remote]) });
  const wanted = Object.freeze({ name: 'db', type: 'dropbox' });
  const intent = Object.freeze({ drives: Object.freeze([wanted]), replicas: 2, mountPath: '/mnt/cloud' });
  const pools = Object.freeze({ pools: Object.freeze([]) });
  const mounts = Object.freeze({ mounts: Object.freeze([]) });

  const got = planSetup({ accounts, pools, mounts, intent });

  assert.equal(got.step, 2);
  assert.deepEqual(intent.drives, [{ name: 'db', type: 'dropbox' }]);
  assert.deepEqual(accounts.remotes, [{ name: 'gd', type: 'gdrive', has_credentials: true }]);
  assert.notEqual(got.drives[0], wanted, 'a returned drive must be a new object, not the caller\'s');
  assert.equal(wanted.exists, undefined, 'the saved plan must not gain fields from being planned against');
});

test('only one drive is ever offered for authorization at a time', () => {
  // The OAuth callback binds one fixed loopback port (127.0.0.1:53682), and
  // providers that require an exactly-registered redirect URI leave no room
  // for an ephemeral one, so a second authorization dies on "address already
  // in use". This is the whole reason the function returns a name, not a list.
  const drives = [
    { name: 'gd', type: 'gdrive', exists: true, authorized: true },
    { name: 'db', type: 'dropbox', exists: true, authorized: false },
    { name: 'box', type: 'box', exists: true, authorized: false },
  ];
  assert.equal(nextAuthTarget(drives), 'db');
});

test('a drive that exists is authorized before one that has yet to be created', () => {
  const drives = [
    { name: 'box', type: 'box', exists: false, authorized: false },
    { name: 'db', type: 'dropbox', exists: true, authorized: false },
  ];
  assert.equal(nextAuthTarget(drives), 'db');
});

test('a drive that does not exist yet is offered once nothing else is waiting', () => {
  const drives = [
    { name: 'gd', type: 'gdrive', exists: true, authorized: true },
    { name: 'box', type: 'box', exists: false, authorized: false },
    { name: 'db', type: 'dropbox', exists: false, authorized: false },
  ];
  assert.equal(nextAuthTarget(drives), 'box');
});

test('a drive whose authorization was refused earlier is offered again, not skipped', () => {
  // A refusal is not a decision about the pool: the person clicked the wrong
  // account, or the provider timed out. Skipping it would strand the wizard
  // with a drive it will never ask about again.
  const drives = [
    { name: 'gd', type: 'gdrive', exists: true, authorized: true },
    { name: 'db', type: 'dropbox', exists: true, authorized: false, refused: true },
  ];
  assert.equal(nextAuthTarget(drives), 'db');
});

test('nothing is offered once every drive is authorized', () => {
  assert.equal(nextAuthTarget([
    { name: 'gd', type: 'gdrive', exists: true, authorized: true },
    { name: 'db', type: 'dropbox', exists: true, authorized: true },
  ]), null);
});

test('an absent drive list offers nothing rather than throwing', () => {
  assert.equal(nextAuthTarget(undefined), null);
  assert.equal(nextAuthTarget(null), null);
  assert.equal(nextAuthTarget([]), null);
  assert.equal(nextAuthTarget('nonsense'), null);
});

test('nextAuthTarget reads the drives planSetup produced without being told anything else', () => {
  const plan = planSetup({
    accounts: accountsWith({ name: 'gd', type: 'gdrive', has_credentials: true }),
    pools: { pools: [] },
    mounts: { mounts: [] },
    intent: intentOf({ name: 'gd', type: 'gdrive' }, { name: 'db', type: 'dropbox' }),
  });
  assert.equal(nextAuthTarget(plan.drives), 'db');
  assert.equal(plan.step, 2);
});

test('a drive removed at the choose step stays removed instead of being adopted back', () => {
  // "Remove this row" only dropped the drive from the intent, and the next
  // load adopted it straight back off /accounts — so a drive taken out at
  // step 1 reappeared at step 2 and joined the pool. A removal is a decision
  // about the pool and has to be recorded as one.
  const got = planSetup({
    accounts: accountsWith(
      { name: 'gd', type: 'gdrive', has_credentials: true },
      { name: 'box-1', type: 'box', has_credentials: true },
    ),
    pools: { pools: [], configurable: true },
    mounts: { mounts: [] },
    intent: { ...intentOf({ name: 'gd', type: 'gdrive' }), excluded: ['box-1'] },
  });
  assert.deepEqual(got.drives.map((d) => d.name), ['gd']);
  assert.equal(got.resumed, false, 'an excluded drive is not something to explain as adopted');
});

test('removing one drive does not remove the others the server holds', () => {
  const got = planSetup({
    accounts: accountsWith(
      { name: 'gd', type: 'gdrive', has_credentials: true },
      { name: 'box-1', type: 'box', has_credentials: true },
      { name: 'box-2', type: 'box', has_credentials: true },
    ),
    pools: { pools: [], configurable: true },
    mounts: { mounts: [] },
    intent: { ...intentOf({ name: 'gd', type: 'gdrive' }), excluded: ['box-1'] },
  });
  assert.deepEqual(got.drives.map((d) => d.name), ['gd', 'box-2']);
  assert.equal(got.resumed, true);
});

test('a drive named in the plan is wanted even if an old removal still lists it', () => {
  // Adding a drive back under the name it had is the person changing their
  // mind, and the plan is where they said so. An exclusion only ever stops a
  // drive being adopted from the server behind their back.
  const got = planSetup({
    accounts: accountsWith({ name: 'box-1', type: 'box', has_credentials: true }),
    pools: { pools: [], configurable: true },
    mounts: { mounts: [] },
    intent: { ...intentOf({ name: 'box-1', type: 'box' }), excluded: ['box-1'] },
  });
  assert.deepEqual(got.drives.map((d) => d.name), ['box-1']);
  assert.equal(got.drives[0].exists, true);
});

test('a plan with no exclusions at all still adopts everything', () => {
  // The field is new, so every saved intent in a browser predates it.
  for (const excluded of [undefined, null, [], 'box-1', { drives: [] }]) {
    const got = planSetup({
      accounts: accountsWith({ name: 'box-1', type: 'box', has_credentials: true }),
      pools: { pools: [], configurable: true },
      mounts: { mounts: [] },
      intent: { drives: [], replicas: 2, mountPath: '/mnt/cloud', excluded },
    });
    assert.deepEqual(got.drives.map((d) => d.name), ['box-1'], String(excluded));
  }
});
