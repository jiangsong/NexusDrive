// Whether a timestamp from the daemon is a deadline at all.
//
// Go's `omitempty` does not omit a struct, and time.Time is a struct: a reply
// that leaves an expiry unset still carries the field, as the zero time
// "0001-01-01T00:00:00Z". Treating "the field is there" as "there is an
// expiry" put the year 1 on screen as a deadline and made the sentence that
// says a link does not expire unreachable.
//
// Anything before 2000 is no expiry. The daemon signs links that live for
// minutes, so no correct value can land in the last century, and this catches
// the Unix epoch — the other zero a timestamp arrives as — along with Go's.
const NOT_BEFORE = Date.UTC(2000, 0, 1);

export function linkExpiry(value) {
  if (!value) return null;
  const at = new Date(value).getTime();
  if (!Number.isFinite(at) || at < NOT_BEFORE) return null;
  return at;
}
