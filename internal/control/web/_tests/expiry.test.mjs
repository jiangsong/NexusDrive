import test from 'node:test';
import assert from 'node:assert/strict';

import { linkExpiry } from '../expiry.js';

test('a link with no expiry set arrives as the zero time, not as a missing field', () => {
  // FSLinkResponse.ExpiresAt is a time.Time with omitempty, and omitempty
  // does not omit a struct — Go has no zero test for one. So a provider that
  // signs links that never expire still sends expires_at, as the year 1, and
  // the page read the field's presence as "there is a deadline" and printed
  // 1/1/1 as one. The "this link does not expire" sentence beside it was
  // unreachable code.
  assert.equal(linkExpiry('0001-01-01T00:00:00Z'), null);
  assert.equal(linkExpiry('0001-01-01T00:00:00.000Z'), null);
});

test('a missing or unparseable expiry is no expiry', () => {
  assert.equal(linkExpiry(undefined), null);
  assert.equal(linkExpiry(null), null);
  assert.equal(linkExpiry(''), null);
  assert.equal(linkExpiry('not a time'), null);
});

test('a real expiry comes back as the instant it names', () => {
  assert.equal(linkExpiry('2026-09-10T12:30:00Z'), Date.UTC(2026, 8, 10, 12, 30, 0));
  // The Unix epoch is still before anything this daemon could have signed,
  // and a signed link is minutes away rather than decades.
  assert.equal(linkExpiry('1970-01-01T00:00:00Z'), null);
});
