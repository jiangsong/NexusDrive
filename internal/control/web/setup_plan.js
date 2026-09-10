// Where the guided setup resumes, decided from what the server holds rather
// than from what the tab remembers.
//
// The wizard cannot keep its place in the page. The daemon re-execs partway
// through — same PID, same control address, so a fetch that succeeds either
// side of it says nothing about which side it was — and the whole flow is a
// sequence of durable configuration edits: an account written by `cloudfs
// config add` in a terminal is as real as one written by the sheet, and a
// credential obtained by `cloudfs config auth` is as real as one obtained by
// the browser. So the saved plan in localStorage is a hint about *intent* —
// which drives someone meant to pool, how many copies they wanted, where they
// wanted it — and never evidence about state. Every fact this module decides
// on (does the account exist, does it carry a credential, is there a pool, is
// anything serving it) is read back from /accounts, /pool/status and /mounts.
//
// The consequence worth stating: when the two disagree, the server wins and
// the plan is widened, never trimmed. A drive the server has and the plan does
// not is adopted into the list. Building a pool that silently leaves out a
// drive somebody already connected and paid for is a far worse failure than
// showing them one they have to untick.
//
// The one thing that does trim it is an explicit "no": `intent.excluded` are
// the drives someone removed from the list by hand. Without it, removing a
// row and pressing Next put the drive back at the next step and then into the
// pool, because adoption cannot tell "never heard of it" from "taken out".
//
// Nothing here touches the document, the network or localStorage, and it
// imports nothing: the caller hands in the three response bodies and the saved
// plan, and gets data back. No sentence in here is ever shown to anyone — the
// screen owns every string, so this returns numbers, names and booleans.

// The steps, as the screen orders them: 0 welcome, 1 choose drives, 2 connect
// them one at a time, 3 redundancy, 4 location, 5 restart and finish.
//
// planSetup answers only 0, 2, 3 and 5, and that is deliberate rather than an
// omission. 1 and 4 are questions being *asked* — a choice of drives that has
// not been made yet, a mount path that has not been typed — and a question in
// flight leaves no trace on the server, so there is nothing to resume it from.
// The screen walks 1 and 4 forward itself and writes the answers into the
// intent; what comes back here is the step to land on after a reload, a
// re-exec or a change made in another window.
const STEP_WELCOME = 0;
const STEP_CONNECT = 2;
const STEP_REDUNDANCY = 3;
const STEP_FINISH = 5;

// The endpoints answer objects — {remotes}, {pools}, {mounts} — but every
// caller in this app unwraps them at the fetch (`(m.mounts || [])`) before
// passing them on, so both shapes arrive here in practice. Anything else,
// including a body that came back as an error object or has not landed yet,
// is an empty list: a first paint must reach step 0, never an exception.
function listOf(body, key) {
  if (Array.isArray(body)) return body;
  if (body && typeof body === 'object' && Array.isArray(body[key])) return body[key];
  return [];
}

// planSetup returns { step, drives, resumed, finished }.
//
// drives is the merged list the screen renders, one entry per drive with at
// least { name, type, exists, authorized }. finished separates the two ways to
// be on the last step: the pool is built and being served, or the pool is
// built and the daemon has yet to restart onto it. resumed says the server
// held drives this plan had to adopt, so the screen can explain why the list
// is longer than the one that was saved.
export function planSetup({ accounts, pools, mounts, intent } = {}) {
  const remotes = listOf(accounts, 'remotes').filter((r) => r && r.name);
  const wanted = (intent && typeof intent === 'object' ? listOf(intent.drives, 'drives') : []).filter((d) => d && d.name);
  // Drives the person took out of the list with "remove this row". Widening
  // the plan from the server is right for a drive nobody here has an opinion
  // about, and wrong for one they have already said no to: removal used to
  // drop the row from the intent alone, so the next load adopted the account
  // straight back and it turned up in the pool anyway. This is the record of
  // the opinion. It is not a claim about the server — the account stays in
  // the configuration, and `cloudfs config remove` is what deletes it.
  const excluded = new Set((intent && typeof intent === 'object' ? listOf(intent.excluded, 'excluded') : [])
    .filter((n) => typeof n === 'string' && n));

  const byName = new Map(remotes.map((r) => [r.name, r]));
  const named = new Set(wanted.map((d) => d.name));

  // An exclusion only ever stops an adoption. A name in both lists is someone
  // who removed a drive and then added it back, and the plan is where they
  // said the second thing.
  const adopted = remotes.filter((r) => !named.has(r.name) && !excluded.has(r.name));

  // The intent's order is the order someone chose, so it leads; adopted
  // remotes follow in the order the server lists them. Each entry is built
  // fresh — the arguments are the caller's state, and a screen that re-plans
  // on every poll would otherwise be decorating the objects it saved.
  const drives = [
    ...wanted.map((d) => describe(d.name, d.type, byName.get(d.name))),
    ...adopted.map((r) => describe(r.name, r.type, r)),
  ];
  const resumed = adopted.length > 0;

  // A pool in the configuration is the point of no return: it is the thing the
  // wizard was for, and it exists whether or not this process serves it yet.
  // Everything before it is a step someone could still be in the middle of, so
  // the pool is tested first and the drive list never sends them backwards
  // through work already committed.
  const poolNames = new Set(listOf(pools, 'pools').filter((p) => p && p.name).map((p) => p.name));
  if (poolNames.size) {
    // Mount edits are durable at once and take effect at the next start, so
    // the configured layout and the served one differ on purpose for as long
    // as it takes to restart. `active` is the daemon's own answer to "am I
    // serving this one", and it is per layout entry: a mount that is active
    // for some other remote says nothing about the pool.
    const served = listOf(mounts, 'mounts').some((m) => m && m.active && poolNames.has(m.remote));
    return { step: STEP_FINISH, drives, resumed, finished: served };
  }

  // No drives at all — no saved plan and nothing on the server — is a first
  // run. An emptied plan counts the same: it names no drive to resume.
  if (!drives.length) return { step: STEP_WELCOME, drives, resumed, finished: false };

  // One missing credential is enough to go back to connecting, and so is one
  // account that was never created: both are answered by the same step, and
  // nextAuthTarget picks which drive it opens on.
  if (drives.some((d) => !d.exists || !d.authorized)) {
    return { step: STEP_CONNECT, drives, resumed, finished: false };
  }
  return { step: STEP_REDUNDANCY, drives, resumed, finished: false };
}

// describe merges one intended drive with the remote of that name, if there is
// one. The server's type wins over the saved one for the same reason the rest
// of this module defers to it: the account is already written, and `type` is
// what the daemon will actually drive it with. The saved type survives only
// for a drive that does not exist yet, where it is the only answer there is.
function describe(name, wantedType, remote) {
  return {
    name,
    type: (remote && remote.type) || wantedType || '',
    exists: Boolean(remote),
    authorized: Boolean(remote && remote.has_credentials),
  };
}

// nextAuthTarget names the one drive to offer for authorization, or null.
//
// At most one, and this is a hard constraint rather than a nicety about
// focus. The OAuth callback binds a fixed loopback port — 127.0.0.1:53682,
// the address `cloudfs config auth` uses and the one that has to be
// registered as the redirect URI in the provider's own console. Providers
// that match the redirect URI exactly leave no room for an ephemeral port, so
// two flows in the air at once means the second one fails to bind with
// "address already in use", and the daemon refuses it with a 409 anyway. A
// screen that offered three "connect" buttons would be offering two that
// cannot work.
//
// Order: a drive that exists but has no credential comes first, because the
// account is already in the configuration and the only thing left to do to it
// is the authorization. Then a drive that has yet to be created, which costs a
// POST /accounts before the same flow can start.
//
// Nothing here consults a "refused" or "skipped" mark, and there is no such
// field to consult. A refusal is not a decision about the pool — the wrong
// account was picked in the provider's chooser, a token expired mid-flow, a
// phone did not scan in time — and a drive dropped on that evidence would
// never be asked about again, leaving a wizard that cannot finish and cannot
// say why. Refusals are the screen's to handle, in the moment, by asking again.
export function nextAuthTarget(drives) {
  if (!Array.isArray(drives)) return null;
  const unauthorized = drives.filter((d) => d && d.name && !d.authorized);
  const existing = unauthorized.find((d) => d.exists);
  if (existing) return existing.name;
  return unauthorized.length ? unauthorized[0].name : null;
}
