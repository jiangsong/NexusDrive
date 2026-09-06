// A ~30-line pub/sub store: one mutable object, a set of subscribers, shallow
// merge on set. Components subscribe in connectedCallback and unsubscribe in
// disconnectedCallback. Not Redux-shaped; it exists so the title bar, the
// status chips and any screen react to the same slice without prop drilling.
const state = {
  status: null,     // latest /status document
  health: 'unknown',// ok | warn | bad | unknown, derived from status
  route: location.hash || '#/connections',
  connected: false, // SSE connected
};
const subs = new Set();

export function get() { return state; }
export function set(patch) {
  Object.assign(state, patch);
  for (const fn of subs) fn(state);
}
export function subscribe(fn) {
  subs.add(fn);
  return () => subs.delete(fn);
}
