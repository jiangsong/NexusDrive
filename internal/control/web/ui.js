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

// confirmDelete opens a sheet that requires the person to type the target's
// identifier before the destructive button enables. It traps focus, returns it
// on close, and closes on Escape. This is the one place a mis-click has real
// consequences, so it gets the strictest treatment.
export function confirmDelete({ title, body, confirmToken, confirmLabel, danger = true }) {
  return new Promise((resolve) => {
    const opener = document.activeElement;
    const input = el('input', { type: 'text', 'aria-label': confirmToken, autocomplete: 'off', spellcheck: 'false' });
    const go = el('button', { class: danger ? 'danger' : 'primary', disabled: true }, confirmLabel || t('action.delete'));
    const cancel = el('button', {}, t('confirm.cancel'));
    input.addEventListener('input', () => { go.disabled = input.value !== confirmToken; });
    const app = document.getElementById('app');
    const close = (result) => { scrim.remove(); if (app) app.inert = false; if (opener && opener.focus) opener.focus(); resolve(result); };
    go.addEventListener('click', () => close(true));
    cancel.addEventListener('click', () => close(false));
    const sheet = el('div', { class: 'sheet' + (danger ? ' danger' : ''), role: 'dialog', 'aria-modal': 'true' },
      el('h3', {}, title),
      el('p', { class: 'detail' }, body),
      el('p', { class: 'muted', style: 'font-size:12px' }, t('confirm.type', confirmToken)),
      input,
      el('div', { class: 'row', style: 'margin-top:16px' }, go, cancel));
    const scrim = el('div', { class: 'scrim' }, sheet);
    scrim.addEventListener('keydown', (e) => { if (e.key === 'Escape') close(false); });
    document.getElementById('modal-root').append(scrim);
    // Take the background out of the tab order so focus cannot leave the sheet
    // into the destructive buttons behind it (nav, restart, delete rows).
    if (app) app.inert = true;
    input.focus();
  });
}

export function iconEl(name) { return el('span', { class: 'ico', html: icons[name] || '' }); }
