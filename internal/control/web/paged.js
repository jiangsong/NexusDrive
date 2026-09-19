// What a paged table does when a page fails to load.
//
// Every paged screen builds its rows the same way: the first call fills the
// table, and "load more" appends to it. The error path did not make that
// distinction — it replaced the table with a single error row — so a failure
// on a continuation threw away every page already fetched, including the rows
// the reader was looking at, for a failure that cost them nothing.

// pageFailureMode says whether a failed load should replace the table or add
// the error underneath what is already there. A continuation is any load that
// carries a cursor.
export function pageFailureMode(cursor) {
  return cursor ? 'append' : 'replace';
}

// pageCursor is what a paged loader makes of the argument it was called with.
//
// Every one of these loaders is also the thing a refresh button and a change
// event want to run, and wiring one straight to a handler — `onclick: load` —
// hands it the click event in the cursor's place. The event was then encoded
// into `&cursor=`, the daemon answered 400 for a cursor it cannot decode, and
// the table the button was meant to reload showed an error instead. A cursor
// is a string the server issued; anything else is a request for the first
// page.
export function pageCursor(value) {
  return typeof value === 'string' ? value : '';
}

// latestOnly guards a loader whose fetch is awaited between clearing the
// table and filling it. Called again while a fetch is in flight, the loader
// used to clear the table a second time and then both fetches appended their
// rows: a burst of change events during a copy — one per file written under
// the directory — drew the same 500 names two, three, four times over. Each
// call takes a ticket; `current(ticket)` says whether that call is still the
// newest, and a stale one paints nothing.
export function latestOnly() {
  let seq = 0;
  return {
    take: () => ++seq,
    current: (ticket) => ticket === seq,
  };
}

// coalesce turns a burst of calls into one, `wait` ms after the first of
// them; calls that arrive while that run is pending are absorbed by it, and a
// call after it fires starts the next. A directory being copied into raises
// a change event per file; reloading the listing for each one is the request
// storm the daemon saw — hundreds of /fs/list calls, each decorating 500
// entries — while the listing on screen changed once. It fires from the
// first call rather than the last so a copy that runs for minutes still
// refreshes the listing every `wait`, not only when it ends. The timer
// functions are parameters so the behaviour can be run without a clock.
export function coalesce(fn, wait, { setTimer = setTimeout, clearTimer = clearTimeout } = {}) {
  let timer = null;
  const call = () => {
    if (timer) return;
    timer = setTimer(() => { timer = null; fn(); }, wait);
  };
  call.cancel = () => { if (timer) clearTimer(timer); timer = null; };
  return call;
}

// listingAffected says whether a change event can alter what a directory
// listing shows: the directory's own rows. A row is added, removed or
// changes state when a path whose parent is the listed directory changes;
// the listing itself goes away when the directory, or an ancestor of it,
// is renamed or removed (`subtree`); and a rescan means re-read everything.
// A change deeper down — a file landing in a subdirectory — alters nothing
// on screen. The listing used to reload for those too: while a repository
// was uploading, every object finishing under .git/objects/xx reloaded the
// listing of the repository's root, one /fs/list and one heat query every
// few hundred milliseconds for as long as the queue ran, for a table whose
// five rows never changed.
export function listingAffected(change, cwd) {
  if (!change) return false;
  if (change.rescan) return true;
  const dir = cwd === '/' ? '/' : cwd.replace(/\/+$/, '');
  for (const p of change.paths || []) {
    if (p === dir) return true;
    const parent = p.lastIndexOf('/') <= 0 ? '/' : p.slice(0, p.lastIndexOf('/'));
    if (parent === dir) return true;
    if (change.subtree && dir.startsWith(p + '/')) return true;
  }
  return false;
}
