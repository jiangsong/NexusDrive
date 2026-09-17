import { get, set, subscribe } from '/ui/store.js';
import { events } from '/ui/api.js';
import { start as startRouter, currentTag, navItems, routes } from '/ui/router.js';
import { el, fill, iconEl } from '/ui/ui.js';
import { t, locale, setLocale, LOCALES } from '/ui/i18n.js';
import { renderMain } from '/ui/screens/main.js';
import { renderPool } from '/ui/screens/pool.js';
import { renderTransfers } from '/ui/screens/transfers.js';
import { renderCopies } from '/ui/screens/copies.js';
import { renderExports } from '/ui/screens/exports.js';
import { renderStorage } from '/ui/screens/storage.js';
import { renderProxy } from '/ui/screens/proxy.js';
import { renderDiagnostics } from '/ui/screens/diagnostics.js';
import { renderSetup } from '/ui/screens/setup.js';
import { renderAgents } from '/ui/screens/agents.js';
import { renderIndex } from '/ui/screens/index.js';
import { renderTriggers } from '/ui/screens/triggers.js';
import { renderSettings } from '/ui/screens/settings.js';
import { renderFs } from '/ui/screens/fs.js';

const screens = {
  'main-window': renderMain,
  'setup-view': renderSetup,
  'pool-view': renderPool,
  'transfers-view': renderTransfers,
  'copies-view': renderCopies,
  'exports-view': renderExports,
  'storage-view': renderStorage,
  'proxy-view': renderProxy,
  'diagnostics-view': renderDiagnostics,
  'agents-view': renderAgents,
  'index-view': renderIndex,
  'triggers-view': renderTriggers,
  'settings-view': renderSettings,
  'fs-view': renderFs,
};

// healthOf reads the structured snapshot, never the warning text: the daemon
// renders warnings in the reader's language, so matching words in them was a
// dot that turned the wrong colour the moment the language changed.
function healthOf(status) {
  if (!status) return 'unknown';
  const u = status.uploads || {};
  const c = status.cache || {};
  const breaker = (status.remotes || []).some((r) => r.breaker_open);
  const full = c.max_bytes > 0 && c.bytes > c.max_bytes * 0.9;
  if (u.dead > 0 || breaker || full) return 'bad';
  if ((status.warnings || []).length) return 'warn';
  return 'ok';
}

// languagePicker is a select rather than a link: the choice is stored and the
// page reloads, so the daemon re-renders its own strings in the same language.
function languagePicker() {
  const sel = el('select', {
    class: 'lang',
    'aria-label': t('app.language'),
    onchange: (e) => setLocale(e.target.value),
  }, ...LOCALES.map((l) => el('option', { value: l.code }, l.label)));
  sel.value = locale();
  return sel;
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
      status ? (status.uptime || t('app.daemon')) : '…'),
    languagePicker());
}

// navBadge is the count drawn over a nav item, read from the last status
// tick: the agents item shows how many MCP sessions are active, the
// triggers item how many deliveries are dead and wait for a retry. A daemon
// without a trigger engine sends no triggers line, which counts as zero.
// Nothing is drawn at zero, so a quiet daemon has a quiet sidebar.
function navBadge(item, status) {
  if (item.badge === 'agents') {
    const n = status && status.agent ? status.agent.active_sessions : 0;
    return n > 0 ? el('span', { class: 'badge', 'aria-label': t('nav.agents.active', n) }, String(n)) : null;
  }
  if (item.badge === 'triggers') {
    const n = status && status.triggers ? (status.triggers.dead || 0) : 0;
    return n > 0 ? el('span', { class: 'badge bad', 'aria-label': t('nav.triggers.dead', n) }, String(n)) : null;
  }
  return null;
}

function nav(activeTag) {
  return el('nav', { class: 'nav' },
    navItems.map((item) => {
      const a = el('a', { href: item.hash }, iconEl(item.icon), el('span', {}, t(item.key)), navBadge(item, get().status));
      // The router owns the hash-to-screen table; a second copy here meant
      // every new screen had to be added in two places or silently never
      // highlighted.
      if (routes[item.hash] === activeTag) a.classList.add('active');
      return a;
    }));
}

let disposeScreen = null;
let titlebarEl = null;
function render() {
  const app = document.getElementById('app');
  const tag = currentTag();
  const status = get().status;
  if (disposeScreen) { disposeScreen(); disposeScreen = null; }
  titlebarEl = titlebar(status);
  fill(app,
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
  const picker = titlebarEl.querySelector('select.lang');
  // Opening a native select focuses it. Replacing that node on an SSE status
  // tick closes the menu before a person can choose a language.
  if (picker && document.activeElement === picker) return;
  const next = titlebar(get().status);
  titlebarEl.replaceWith(next);
  titlebarEl = next;
  refreshNavBadges();
}

// The nav is drawn once per screen, not per status tick, so the badge a
// status tick changes is swapped in place: the old one comes off the link
// and the new one — or nothing — goes on.
function refreshNavBadges() {
  for (const item of navItems) {
    if (!item.badge) continue;
    const link = document.querySelector('.nav a[href="' + item.hash + '"]');
    if (!link) continue;
    const old = link.querySelector('.badge');
    if (old) old.remove();
    const badge = navBadge(item, get().status);
    if (badge) link.append(badge);
  }
}

// One event stream feeds the whole app: status ticks update the title bar and
// any screen watching, change events let the file browser refresh the affected
// directory without a polling storm. These are declared before startRouter,
// which renders synchronously — renderMain calls onFsChange during that first
// render, so changeHandlers must already exist.
const changeHandlers = new Set();
export function onFsChange(fn) { changeHandlers.add(fn); return () => changeHandlers.delete(fn); }
const exportHandlers = new Set();
export function onExportChange(fn) { exportHandlers.add(fn); return () => exportHandlers.delete(fn); }
// Audit rows and session changes share one subscription: a listener gets
// { kind: 'audit' | 'session', data } and picks what it shows.
const agentHandlers = new Set();
export function onAgentEvent(fn) { agentHandlers.add(fn); return () => agentHandlers.delete(fn); }
// Index progress: one snapshot per frame, at most once a second, for the
// progress bar on the index screen.
const indexHandlers = new Set();
export function onIndexChange(fn) { indexHandlers.add(fn); return () => indexHandlers.delete(fn); }
// Trigger deliveries: one event per state change, for the triggers screen
// and for the dead-count badge between two status ticks.
const triggerHandlers = new Set();
export function onTriggerEvent(fn) { triggerHandlers.add(fn); return () => triggerHandlers.delete(fn); }

// deadDeliveryChanged moves the badge as soon as a delivery dies rather
// than at the next status tick. Only the death is counted here: a retry
// shows up as a pending row the frame cannot tell from a backoff, so the
// tick — which recounts from the queue — is what brings the number down.
function deadDeliveryChanged(ev) {
  const status = get().status;
  if (!ev || ev.state !== 'dead' || !status) return;
  const triggers = status.triggers || { enabled: true, pending: 0, dead: 0 };
  set({ status: { ...status, triggers: { ...triggers, dead: (triggers.dead || 0) + 1 } } });
}

subscribe(() => refreshTitlebar());
startRouter(() => render()); // performs the initial render
events({
  onStatus: (s) => set({ status: s, health: healthOf(s), connected: true }),
  onChange: (c) => { for (const fn of changeHandlers) fn(c); },
  onExport: (e) => { for (const fn of exportHandlers) fn(e); },
  onAudit: (d) => { for (const fn of agentHandlers) fn({ kind: 'audit', data: d }); },
  onSession: (d) => { for (const fn of agentHandlers) fn({ kind: 'session', data: d }); },
  onIndex: (p) => { for (const fn of indexHandlers) fn(p); },
  onTrigger: (d) => { deadDeliveryChanged(d); for (const fn of triggerHandlers) fn(d); refreshNavBadges(); },
});
