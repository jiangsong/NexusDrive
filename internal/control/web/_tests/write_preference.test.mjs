import test from 'node:test';
import assert from 'node:assert/strict';

import { writePreferenceState, writePreferencePayload, writeCostLabel } from '../screens/write_preference.js';

const members = [{ remote: 'gdrive-1', class: [] }, { remote: 'quark', class: [] }];

test('a pool with no rules has no write preference yet and can take one', () => {
  const state = writePreferenceState(members, []);
  assert.equal(state.simple, true);
  assert.equal(state.primary, null);
});

test('the generated rule reads back as the preference it was saved from', () => {
  const saved = [{ remote: 'gdrive-1', class: ['fast'] }, { remote: 'quark', class: ['slow'] }];
  const rules = [{ prefix: '/', replicas: 0, prefer: ['fast'], avoid: [], require: [] }];
  const state = writePreferenceState(saved, rules);
  assert.equal(state.simple, true);
  assert.equal(state.primary, 'gdrive-1');
});

// The radio writes both textareas. Rendering it over a rule set it cannot
// represent would silently throw that rule set away on the next save.
test('a rule the radio cannot represent leaves editing to the textareas', () => {
  const rules = [{ prefix: '/photos', replicas: 3, prefer: ['fast'], avoid: [], require: [] }];
  const state = writePreferenceState(members, rules);
  assert.equal(state.simple, false);
  assert.equal(state.primary, null);
});

test('more than one rule leaves editing to the textareas', () => {
  const rules = [
    { prefix: '/', replicas: 0, prefer: ['fast'], avoid: [], require: [] },
    { prefix: '/cold', replicas: 1, prefer: [], avoid: [], require: [] },
  ];
  assert.equal(writePreferenceState(members, rules).simple, false);
});

// A class the radio did not write is a label the person uses for something
// else; overwriting every member's classes would destroy it.
test('a class outside fast and slow leaves editing to the textareas', () => {
  const saved = [{ remote: 'gdrive-1', class: ['fast'] }, { remote: 'quark', class: ['archive'] }];
  const rules = [{ prefix: '/', replicas: 0, prefer: ['fast'], avoid: [], require: [] }];
  assert.equal(writePreferenceState(saved, rules).simple, false);
});

test('choosing a primary labels it fast and every other member slow', () => {
  const payload = writePreferencePayload(members, 'gdrive-1');
  assert.deepEqual(payload.member_classes, { 'gdrive-1': ['fast'], quark: ['slow'] });
  assert.deepEqual(payload.rules, [{ prefix: '/', replicas: 0, prefer: ['fast'], avoid: [], require: [] }]);
});

test('clearing the preference removes the rule and the labels it wrote', () => {
  const payload = writePreferencePayload(members, null);
  assert.deepEqual(payload.member_classes, { 'gdrive-1': [], quark: [] });
  assert.deepEqual(payload.rules, []);
});

// A member with no finished upload has no speed to report. Printing 0 would
// read as the fastest drive in the pool, which is the opposite of the truth.
test('a member with no samples has no cost to show', () => {
  assert.equal(writeCostLabel(0, 0), null);
  assert.equal(writeCostLabel(1234, 0), null);
});

// Seconds is the unit the slow case lives in — 36000 ms is the real figure
// this column exists to expose — and one decimal is all of it that means
// anything.
test('a cost of a second or more reads in seconds', () => {
  assert.deepEqual(writeCostLabel(36040, 12), { key: 'pool.write.seconds', args: ['36.0'] });
  assert.deepEqual(writeCostLabel(2070, 9), { key: 'pool.write.seconds', args: ['2.1'] });
});

test('a sub-second cost reads in whole milliseconds', () => {
  assert.deepEqual(writeCostLabel(340.6, 4), { key: 'pool.write.ms', args: [341] });
});

// Rounding a fast member's real cost down to "0 ms/file" prints the same claim
// the samples guard exists to prevent: instant, therefore the best possible
// primary. A member that finished uploads is never free, so say "under a
// millisecond" instead of rounding the evidence away.
test('a cost under a millisecond never rounds down to zero', () => {
  assert.deepEqual(writeCostLabel(0.287, 9), { key: 'pool.write.subms', args: [] });
  assert.deepEqual(writeCostLabel(0.9, 2), { key: 'pool.write.subms', args: [] });
  assert.deepEqual(writeCostLabel(1.4, 2), { key: 'pool.write.ms', args: [1] });
});

// Round trip: what the radio saves is what it reads back, or the control
// would disappear the moment it was used.
test('a saved preference round trips through state', () => {
  const payload = writePreferencePayload(members, 'quark');
  const saved = members.map((m) => ({ remote: m.remote, class: payload.member_classes[m.remote] }));
  assert.equal(writePreferenceState(saved, payload.rules).primary, 'quark');
});
