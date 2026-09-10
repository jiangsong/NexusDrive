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
