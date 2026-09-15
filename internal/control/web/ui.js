// Small DOM helpers and shared UI: toasts and the typed-confirmation sheet.
import { t } from '/ui/i18n.js';
import { icons } from '/ui/icons.js';

export function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') node.className = v;
    else if (k === 'html') node.innerHTML = v;
    else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
    else if (v === true) node.setAttribute(k, '');
    else if (v !== false && v != null) node.setAttribute(k, v);
  }
  return append(node, children);
}

// fill replaces a node's children the way el() adds them: a child that is null
// or false is a row the caller decided not to render, so it is skipped. The DOM
// method behind this, replaceChildren, stringifies instead — a conditional row
// that evaluates to null lands in the panel as the word "null". Every screen
// fills through here for that reason. el() appends without clearing, because
// clearing would also throw away what the `html` attribute just set.
export function fill(node, ...children) {
  node.replaceChildren();
  return append(node, children);
}

function append(node, children) {
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    node.append(c.nodeType ? c : document.createTextNode(String(c)));
  }
  return node;
}

export function bytes(n) {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0; let v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${u[i]}`;
}

export function toast(message, kind) {
  const host = document.getElementById('toasts');
  const node = el('div', { class: 'toast' + (kind === 'bad' ? ' bad' : '') }, message);
  host.append(node);
  setTimeout(() => node.remove(), 6000);
}

// releasePage gives the page back to the reader, but only once the last
// sheet is gone. Sheets nest — unbinding a mount from the connection
// settings, discarding an upload from the queue — and each of them used to
// clear `inert` on its own way out, handing focus and clicks back to content
// still covered by the sheet underneath.
export function releasePage() {
  const app = document.getElementById('app');
  const root = document.getElementById('modal-root');
  if (app && (!root || !root.querySelector('.scrim'))) app.inert = false;
}

// promptText is the non-blocking, WebView-safe replacement for window.prompt.
// It keeps input inside the application DOM so keyboard focus, accessibility,
// and browser automation all observe the same flow.
export function promptText({ title, body = '', label, initial = '', confirmLabel }) {
  return new Promise((resolve) => {
    const input = el('input', { type: 'text', value: initial, 'aria-label': label || title, autocomplete: 'off', spellcheck: 'false' });
    const go = el('button', { class: 'primary', disabled: !initial.trim() }, confirmLabel || t('action.add'));
    const cancel = el('button', {}, t('confirm.cancel'));
    const close = openPanel({
      title,
      content: el('div', {}, body ? el('p', { class: 'detail' }, body) : null, input),
      footer: el('div', { class: 'row', style: 'margin-top:16px' }, go, cancel),
      onEscape: () => resolve(close(null)),
    });
    go.addEventListener('click', () => resolve(close(input.value.trim())));
    cancel.addEventListener('click', () => resolve(close(null)));
    input.addEventListener('input', () => { go.disabled = !input.value.trim(); });
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !go.disabled) { e.preventDefault(); go.click(); }
    });
  });
}

// confirmDelete opens a sheet that requires the person to type the target's
// identifier before the destructive button enables. It traps focus, returns it
// on close, and closes on Escape. This is the one place a mis-click has real
// consequences, so it gets the strictest treatment.
export function confirmDelete({ title, body, confirmToken, confirmLabel, danger = true }) {
  return new Promise((resolve) => {
    const input = el('input', { type: 'text', 'aria-label': confirmToken, autocomplete: 'off', spellcheck: 'false' });
    const go = el('button', { class: danger ? 'danger' : 'primary', disabled: true }, confirmLabel || t('action.delete'));
    const cancel = el('button', {}, t('confirm.cancel'));
    const close = openPanel({
      title, danger,
      content: el('div', {},
        body ? el('p', { class: 'detail' }, body) : null,
        el('p', { class: 'muted', style: 'font-size:12px' }, t('confirm.type', confirmToken)),
        input),
      footer: el('div', { class: 'row', style: 'margin-top:16px' }, go, cancel),
      onEscape: () => resolve(close(false)),
    });
    go.addEventListener('click', () => resolve(close(true)));
    cancel.addEventListener('click', () => resolve(close(false)));
    input.addEventListener('input', () => { go.disabled = input.value !== confirmToken; });
  });
}

// openForm is a modal with several labelled controls, resolving true when the
// person confirms it. The sheets above take exactly one input; a record with
// five or six fields (a proxy outbound, a group) needs this instead. It is
// here rather than in a screen so the focus trap, the Escape handling and the
// focus restore have one implementation, not one per screen that wanted a
// form.
let formControlID = 0;

export function openForm({ title, rows, note, confirmLabel, width = 480, validate }) {
  return new Promise((resolve) => {
    const ok = el('button', { class: 'primary' }, confirmLabel || t('proxy.apply'));
    const cancel = el('button', {}, t('confirm.cancel'));
    const error = el('div', { role: 'alert', class: 'detail', style: 'display:none;color:var(--bad)' });
    const content = el('div', { style: 'display:grid;gap:10px' },
      ...rows.map(([label, control]) => {
        const id = control.id || `form-control-${++formControlID}`;
        control.id = id;
        return el('div', {},
          el('label', { for: id, style: 'display:block;font-size:12px;margin-bottom:4px' }, label), control);
      }),
      error, note || null);
    const close = openPanel({
      title, width, content,
      footer: el('div', { class: 'row', style: 'margin-top:16px;justify-content:flex-end' }, cancel, ok),
      onEscape: () => resolve(close(false)),
    });
    ok.addEventListener('click', () => {
      const message = validate ? validate() : '';
      if (message) {
        error.textContent = String(message);
        error.style.display = '';
        return;
      }
      resolve(close(true));
    });
    cancel.addEventListener('click', () => resolve(close(false)));
  });
}

// showPanel presents something to read — a preview, a link — with nothing to
// decide. It resolves when the panel closes.
export function showPanel({ title, content, closeLabel, width = 560 }) {
  return new Promise((resolve) => {
    const done = el('button', { class: 'primary' }, closeLabel || t('conn.close'));
    const close = openPanel({
      title, width, content,
      footer: el('div', { class: 'row', style: 'margin-top:16px;justify-content:flex-end' }, done),
      onEscape: () => resolve(close()),
    });
    done.addEventListener('click', () => resolve(close()));
  });
}

// openPanel is the shared shell: scrim, dialog semantics, focus trap, focus
// restore, Escape. Everything modal in the app goes through it — the sheets
// below, and the two screens that render their own body into it.
export function openPanel({ title, content, footer, width = 480, danger = false, onEscape }) {
  const opener = document.activeElement;
  const app = document.getElementById('app');
  const sheet = el('div', { class: 'sheet' + (danger ? ' danger' : ''), role: 'dialog', 'aria-modal': 'true', 'aria-label': title, style: `width:min(${width}px,calc(100% - 32px))` },
    el('h3', {}, title), content, footer);
  const scrim = el('div', { class: 'scrim' }, sheet);
  const close = (result) => {
    scrim.remove();
    releasePage();
    if (opener && opener.focus) opener.focus();
    return result;
  };
  scrim.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { e.preventDefault(); onEscape(); return; }
    if (e.key !== 'Tab') return;
    const focusable = [...sheet.querySelectorAll('a[href],button,input,select,textarea')].filter((n) => !n.disabled && n.getClientRects().length);
    if (!focusable.length) return;
    const at = focusable.indexOf(document.activeElement);
    if (e.shiftKey && at <= 0) { e.preventDefault(); focusable[focusable.length - 1].focus(); }
    else if (!e.shiftKey && at === focusable.length - 1) { e.preventDefault(); focusable[0].focus(); }
  });
  document.getElementById('modal-root').append(scrim);
  if (app) app.inert = true;
  const first = sheet.querySelector('input,select,textarea,button');
  if (first) first.focus();
  return close;
}


// moreRow is the last row of a paged table: the control that fetches the next
// page. It lives in the table so it cannot drift away from the list it
// extends, and it disables itself on the first click — a second click would
// append the same page twice.
export function moreRow(columns, onMore) {
  const go = el('button', {}, t('page.more'));
  go.addEventListener('click', () => {
    go.disabled = true;
    go.closest('tr').remove();
    onMore();
  });
  return el('tr', {}, el('td', { colspan: String(columns), style: 'text-align:center;padding:12px' }, go));
}


// copyBtn hands a string to the clipboard and says so, then goes back to
// offering. Where the clipboard is unavailable — an insecure origin, a
// WebView without permission — the text is shown instead, so there is always
// a way to get it.
export function copyBtn(text) {
  const b = el('button', {}, t('add.copy'));
  b.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(text);
      b.textContent = t('add.copied');
      setTimeout(() => { b.textContent = t('add.copy'); }, 1500);
    } catch (_) { toast(text); }
  });
  return b;
}

export function iconEl(name) { return el('span', { class: 'ico', html: icons[name] || '' }); }
