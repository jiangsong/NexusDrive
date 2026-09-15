// One account's authorization, as a piece of screen that can be placed
// anywhere.
//
// This used to live inside the add-drive modal. The guided setup runs the same
// exchange four times over, and a second copy of it would be a second place to
// forget that a refused authorization must clear the pending session, or that
// polling has to stop when the surface goes away. One implementation, two
// callers.
//
// OAuth hands this component a URL to open. Device flows hand it only a random
// session id; the QR payload remains server-side and is rendered through a
// same-origin PNG route. Credentials never cross this boundary — the daemon
// completes the exchange and saves them itself.

import { api, ApiError } from '/ui/api.js';
import { el, fill, copyBtn, toast } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { authFailureMode } from '/ui/auth_policy.js';

// httpsOnly keeps anything but an https URL out of an href. The daemon builds
// this URL, but from an endpoint that is an editable per-account field, so it
// is not beyond doubt; the content security policy would stop a javascript:
// URL from running, and this stops it from being offered.
export function httpsOnly(url) {
  try { return new URL(url).protocol === 'https:' ? url : ''; } catch (_) { return ''; }
}

export function codeBox(text) {
  return el('div', { style: 'display:flex;gap:9px;align-items:flex-start' },
    el('div', { style: 'flex-grow:1;font-family:ui-monospace,monospace;font-size:12px;background:#0a0f16;border:1px solid var(--hairline);border-radius:6px;padding:9px;word-break:break-all' }, text),
    copyBtn(text));
}

function qrBox(name, session) {
  const src = '/accounts/' + encodeURIComponent(name) + '/auth/qr?session=' + encodeURIComponent(session);
  return el('div', { style: 'display:flex;justify-content:center;padding:12px' },
    el('img', { src, alt: t('add.auth.qr.alt'), width: '256', height: '256', style: 'background:#fff;border-radius:10px;padding:8px' }));
}

export function linkRow(url) {
  const safe = httpsOnly(url);
  if (!safe) return codeBox(url);
  const a = el('a', { href: safe, target: '_blank', rel: 'noopener', style: 'color:var(--accent-text);word-break:break-all' }, safe);
  return el('div', { style: 'display:flex;gap:9px;align-items:center;flex-wrap:wrap' },
    el('a', { href: safe, target: '_blank', rel: 'noopener', class: 'btn' }, t('add.auth.open')), copyBtn(safe), a);
}

// startAuthorization asks the daemon to begin, renders whatever it answers with
// into host, and polls until the flow settles.
//
// created is the POST /accounts reply, used only for the terminal fallback: a
// backend whose credential is a password or an externally issued token has no
// authorization server to drive, and for it the honest answer is the exact
// command to run.
//
// alive() reports whether the surface is still on screen; polling stops when it
// is not. onPending is told the session so the caller can cancel it if the
// person walks away — an authorization left running holds the one callback port
// and the next account cannot start. It can be called after the caller has
// already gone, because the start is a round trip: a caller that can be
// dismissed must cancel the session it is handed rather than store it.
//
// The returned `refused` is a start the daemon turned down for a reason worth
// reading — see auth_policy.js — and is already on screen and in a toast when
// it is set.
export async function startAuthorization({ name, created, host, alive, onPending, onSettled }) {
  const status = el('div', { class: 'dim', style: 'font-size:12.5px' });
  const parts = [];
  let started = null;
  // refused holds a start the daemon turned down for a reason the person can
  // act on. Every failure used to become the terminal fallback, which told
  // someone whose Google Drive was mid-authorization elsewhere to go and run
  // a command — the one thing that does not help. authFailureMode names the
  // two answers that really do mean "no browser flow here"; everything else
  // is said out loud.
  let refused = null;
  try {
    started = await api.post('/accounts/' + encodeURIComponent(name) + '/auth/start', {});
  } catch (e) {
    started = null;
    if (authFailureMode(e instanceof ApiError ? e.status : undefined) === 'surface') refused = e;
  }
  if (started && started.session && onPending) onPending({ name, session: started.session });

  if (started && (started.kind === 'url' || started.kind === 'qr')) {
    parts.push(el('p', { class: 'detail' }, t(started.kind === 'url' ? 'add.auth.url' : 'add.auth.qr')));
    parts.push(started.kind === 'url' ? linkRow(started.value) : qrBox(name, started.session));
    parts.push(status);
  } else if (refused) {
    // The daemon's own sentence, in the language this page asked for. It
    // stays on the row as well as in the toast: a toast fades, and the row is
    // where the person is looking for the reason nothing happened.
    status.textContent = '✕ ' + refused.message;
    status.style.color = 'var(--danger-text)';
    parts.push(status);
    toast(refused.message, 'bad');
  } else {
    parts.push(el('p', { class: 'detail' }, t('add.auth.term')));
    parts.push(codeBox((created && created.next_command) || ('cloudfs config auth ' + name)));
    if (created && created.credentials) {
      parts.push(el('div', { class: 'dim', style: 'font-size:12px' }, created.credentials));
    }
  }
  if (host) fill(host, ...parts);

  if (started && started.session) {
    pollAuthorization({ name, session: started.session, status, alive, onSettled });
  }
  return { parts, started, status, refused };
}

// pollAuthorization watches one flow to its end. It deliberately reports a
// refusal as a settled outcome rather than an error: the person declined, and
// the next thing they need is the chance to try again.
export async function pollAuthorization({ name, session, status, alive, onSettled }) {
  if (status) status.textContent = t('add.waiting');
  for (let i = 0; i < 150; i++) {
    await new Promise((r) => setTimeout(r, 2000));
    if (alive && !alive()) return;
    let st;
    try {
      st = await api.get('/accounts/' + encodeURIComponent(name) + '/auth/status?session=' + encodeURIComponent(session));
    } catch (_) { continue; }
    if (st.state === 'done') {
      if (status) { status.textContent = '✓ ' + t('add.done'); status.style.color = 'var(--ok)'; }
      if (onSettled) onSettled({ name, state: 'done' });
      return;
    }
    if (st.state === 'denied' || st.state === 'error') {
      if (status) {
        status.textContent = '✕ ' + (st.error ? t('add.denied.detail', st.error) : t('add.denied'));
        status.style.color = 'var(--danger-text)';
      }
      if (onSettled) onSettled({ name, state: st.state, error: st.error });
      return;
    }
  }
}
