import { set } from '/ui/store.js';
// A hash router. Overlays (add-drive, connect) are state on top of the current
// screen, not navigations, so the server needs no SPA fallback and the mux's
// "exact path or 404" contract is untouched.
export const routes = {
  '#/connections': 'main-window',
  '#/pool': 'pool-view',
  '#/transfers': 'transfers-view',
  '#/copies': 'copies-view',
  '#/exports': 'exports-view',
  '#/agents': 'agents-view',
  '#/index': 'index-view',
  '#/triggers': 'triggers-view',
  '#/storage': 'storage-view',
  '#/proxy': 'proxy-view',
  '#/diagnostics': 'diagnostics-view',
  '#/settings': 'settings-view',
  // Setup is reachable by hash but is not a nav item: it is a first-run flow
  // someone is sent to, not a place to browse back to.
  '#/setup': 'setup-view',
};
export function currentTag() {
  const base = (location.hash || '#/connections').split('?')[0];
  return routes[base] || 'main-window';
}
export function start(onChange) {
  const apply = () => { set({ route: location.hash }); onChange(currentTag()); };
  window.addEventListener('hashchange', apply);
  apply();
}
export const navItems = [
  { hash: '#/connections', icon: 'cloud', key: 'nav.connections' },
  { hash: '#/pool', icon: 'db', key: 'nav.pool' },
  { hash: '#/transfers', icon: 'transfer', key: 'nav.transfers' },
  { hash: '#/copies', icon: 'file', key: 'nav.copies' },
  { hash: '#/exports', icon: 'download', key: 'nav.exports' },
  // badge names the count the shell draws on the item: the active sessions
  // from /status, so an agent at work is visible from any screen.
  { hash: '#/agents', icon: 'bot', key: 'nav.agents', badge: 'agents' },
  { hash: '#/index', icon: 'layers', key: 'nav.index' },
  // The triggers badge is the number of dead deliveries, the one queue
  // state that waits for a person.
  { hash: '#/triggers', icon: 'bolt', key: 'nav.triggers', badge: 'triggers' },
  { hash: '#/storage', icon: 'db', key: 'nav.storage' },
  { hash: '#/proxy', icon: 'globe', key: 'nav.proxy' },
  { hash: '#/diagnostics', icon: 'wrench', key: 'nav.diagnostics' },
  { hash: '#/settings', icon: 'wrench', key: 'nav.settings' },
];
