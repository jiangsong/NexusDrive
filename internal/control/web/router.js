import { set } from '/ui/store.js';
// A hash router. Overlays (add-drive, connect) are state on top of the current
// screen, not navigations, so the server needs no SPA fallback and the mux's
// "exact path or 404" contract is untouched.
const routes = {
  '#/connections': 'main-window',
  '#/pool': 'pool-view',
  '#/transfers': 'transfers-view',
  '#/storage': 'storage-view',
  '#/proxy': 'proxy-view',
  '#/diagnostics': 'diagnostics-view',
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
  { hash: '#/storage', icon: 'db', key: 'nav.storage' },
  { hash: '#/proxy', icon: 'globe', key: 'nav.proxy' },
  { hash: '#/diagnostics', icon: 'wrench', key: 'nav.diagnostics' },
];
