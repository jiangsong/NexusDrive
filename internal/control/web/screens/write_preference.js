// The write preference: which member a new file is written to first.
//
// A pool writes one member — the primary — and lets repair make the other
// replicas from the local hold afterwards, so the primary alone decides how
// long a write takes to reach a drive. Placement picks it by free space, which
// on a mixed pool means the roomiest drive wins even when it is many times
// slower than its sibling. The preference is expressed to placement as a
// member class plus one rule that prefers it.
//
// This module is the translation between that pair and the single choice the
// page offers. It is pure so it can be tested without a DOM: the screen owns
// the inputs, this owns what they mean.

// The classes the control writes. They are ordinary member classes; nothing in
// placement knows these two names.
const FAST = 'fast';
const SLOW = 'slow';

// The one rule the control writes: every path prefers the fast member.
// replicas 0 means "inherit the pool's own count" — the preference changes
// where a file is written first, never how many copies it gets.
function generatedRule() {
  return { prefix: '/', replicas: 0, prefer: [FAST], avoid: [], require: [] };
}

function sameRule(r) {
  return r
    && r.prefix === '/'
    && (r.replicas || 0) === 0
    && (r.prefer || []).length === 1 && (r.prefer || [])[0] === FAST
    && (r.avoid || []).length === 0
    && (r.require || []).length === 0;
}

// writeCostLabel turns what one file costs on a member into the key and
// argument the page prints, or null when there is nothing to print.
//
// Null is not zero. A member that has finished no upload has no speed yet, and
// printing 0 would rank it as the fastest drive in the pool — the argument for
// making it every file's primary, from no evidence at all.
export function writeCostLabel(ms, samples) {
  if (!samples) return null;
  if (ms >= 1000) return { key: 'pool.write.seconds', args: [(ms / 1000).toFixed(1)] };
  // Rounding a real cost down to "0 ms/file" makes the same claim the samples
  // guard exists to prevent — instant, therefore the obvious primary. A member
  // that finished an upload is never free, so name the floor instead.
  if (ms < 1) return { key: 'pool.write.subms', args: [] };
  return { key: 'pool.write.ms', args: [Math.round(ms)] };
}

// writePreferenceState reports whether the radio can represent what is saved,
// and which member is the primary if so.
//
// It refuses (simple: false) whenever saving the radio would destroy something
// it cannot show: a rule set with a prefix, a replica count or an avoid list,
// more than one rule, or a member class the control never wrote. In those
// cases the screen shows the class and rule textareas instead, which can
// express all of it.
export function writePreferenceState(members, rules) {
  const list = rules || [];
  const labelled = (members || []).filter((m) => (m.class || []).length > 0);
  for (const m of members || []) {
    for (const c of m.class || []) {
      if (c !== FAST && c !== SLOW) return { simple: false, primary: null };
    }
  }
  if (list.length === 0) {
    // Labels with no rule to act on them are not a preference: placement
    // ignores a class nothing prefers. Saying so would be a lie the next
    // save would make true.
    return { simple: labelled.length === 0, primary: null };
  }
  if (list.length > 1 || !sameRule(list[0])) return { simple: false, primary: null };
  const fast = (members || []).filter((m) => (m.class || []).includes(FAST));
  if (fast.length !== 1) return { simple: false, primary: null };
  return { simple: true, primary: fast[0].remote };
}

// writePreferencePayload turns the chosen primary into the two fields the
// pool configuration endpoint already takes. A null primary clears the
// preference: the rule goes away and so do the labels that rule read.
export function writePreferencePayload(members, primary) {
  const member_classes = {};
  for (const m of members || []) {
    if (primary === null || primary === undefined) member_classes[m.remote] = [];
    else member_classes[m.remote] = m.remote === primary ? [FAST] : [SLOW];
  }
  return { member_classes, rules: primary === null || primary === undefined ? [] : [generatedRule()] };
}
