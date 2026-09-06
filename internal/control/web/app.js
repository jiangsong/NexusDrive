import { get, set, subscribe } from '/ui/store.js';
import { events } from '/ui/api.js';
import { start as startRouter, currentTag, navItems } from '/ui/router.js';
import { el, iconEl } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { renderMain } from '/ui/screens/main.js';
import { renderTransfers } from '/ui/screens/transfers.js';
import { renderStorage } from '/ui/screens/storage.js';
import { renderProxy } from '/ui/screens/proxy.js';
import { renderDiagnostics } from '/ui/screens/diagnostics.js';

const screens = {
  'main-window': renderMain,
  'transfers-view': renderTransfers,
  'storage-view': renderStorage,
  'proxy-view': renderProxy,
  'diagnostics-view': renderDiagnostics,
};

function healthOf(status) {
  if (!status) return 'unknown';
  if ((status.warnings || []).some((w) => /失败|dead|risk|full/i.test(w))) return 'bad';
  if ((status.uploads && status.uploads.dead) || (status.warnings || []).length) return 'warn';
  return 'ok';
}

function titlebar(status) {
  const u = (status && status.uploads) || {};
  const c = (status && status.cache) || {};
  const active = (u.pending || 0) + (u.uploading || 0);
  return el('div', { class: 'titlebar' },
    el('div', { class: 'brand' }, 'CloudFS'),
    el('div', { class: 'grow' }),
    el('div', { class: 'chip' }, iconEl('up'), `${t('app.queue')} ${active}`),
    el('div', { class: 'chip' }, iconEl('db'), `${t('app.cache')} ${c.bytes_human || '0 B'}`),
    el('div', { class: 'chip' }, el('span', { class: 'dot ' + healthOf(status) }),
      status ? (status.uptime || t('app.daemon')) : '…'));
}

function nav(activeTag) {
  return el('nav', { class: 'nav' },
    navItems.map((item) => {
      const a = el('a', { href: item.hash }, iconEl(item.icon), el('span', {}, t(item.key)));
      if (currentTag() === activeTag && screenForHash(item.hash) === activeTag) a.classList.add('active');
      return a;
    }));
}
function screenForHash(hash) {
  const map = { '#/connections': 'main-window', '#/transfers': 'transfers-view', '#/storage': 'storage-view', '#/proxy': 'proxy-view', '#/diagnostics': 'diagnostics-view' };
  return map[hash];
}

let disposeScreen = null;
let titlebarEl = null;
function render() {
  const app = document.getElementById('app');
  const tag = currentTag();
  const status = get().status;
  if (disposeScreen) { disposeScreen(); disposeScreen = null; }
  titlebarEl = titlebar(status);
  app.replaceChildren(
    titlebarEl,
    el('div', { class: 'body' },
      nav(tag),
      (() => {
        const host = el('div', { class: 'content' });
        const fn = screens[tag] || renderMain;
        disposeScreen = fn(host);
        return host;
      })()));
}

// A status tick arrives every couple of seconds. It must update only the title
// bar, never remount the active screen: remounting re-ran side-effecting loads
// (/doctor/run, /proxy/check, /accounts, /fs/list) on a timer and destroyed the
// focus, scroll and half-typed search of whoever was using the page. Screens
// that want live data subscribe themselves (transfers) or listen for change
// events (the file browser, via onFsChange); the shell just repaints the chips.
function refreshTitlebar() {
  if (!titlebarEl) return;
  const next = titlebar(get().status);
  titlebarEl.replaceWith(next);
  titlebarEl = next;
}

startRouter(() => render());
subscribe(() => refreshTitlebar());

// One event stream feeds the whole app: status ticks update the title bar and
// any screen watching, change events let the file browser refresh the affected
// directory without a polling storm.
const changeHandlers = new Set();
export function onFsChange(fn) { changeHandlers.add(fn); return () => changeHandlers.delete(fn); }
events({
  onStatus: (s) => set({ status: s, health: healthOf(s), connected: true }),
  onChange: (c) => { for (const fn of changeHandlers) fn(c); },
});
render();
