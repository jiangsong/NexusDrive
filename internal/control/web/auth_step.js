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
// same-origin PNG route. The account's credential never crosses this boundary
// — the daemon completes the exchange and saves it itself.
//
// One value does: the secret of an OAuth application the person registered
// themselves, without which no authorization can begin at all. See
// appSecretForm below, and internal/control/app_secret.go for the rule that
// keeps that opening as narrow as the problem.

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
export async function startAuthorization(opts) {
  // The component owns one element and re-renders inside it, because one of
  // its outcomes leads back to the beginning: an authorization that stopped
  // for want of the OAuth application's secret is started again as soon as
  // the secret is stored, and the caller must not have to know that.
  const box = el('div', { style: 'display:grid;gap:10px' });
  const outcome = await attempt(opts, box);
  if (opts.host) fill(opts.host, box);
  return { ...outcome, parts: [box] };
}

async function attempt(opts, box) {
  const { name, created, alive, onPending, onSettled } = opts;
  const status = el('div', { class: 'dim', style: 'font-size:12.5px' });
  let started = null;
  // refused holds a start the daemon turned down for a reason the person can
  // act on. Every failure used to become the terminal fallback, which told
  // someone whose Google Drive was mid-authorization elsewhere to go and run
  // a command — the one thing that does not help. authFailureMode names the
  // two answers that really do mean "no browser flow here", the one that
  // means "a value you have is missing", and says the rest out loud.
  let refused = null;
  let mode = null;
  try {
    started = await api.post('/accounts/' + encodeURIComponent(name) + '/auth/start', {});
  } catch (e) {
    started = null;
    mode = authFailureMode(e instanceof ApiError ? e.status : undefined);
    if (mode !== 'terminal') refused = e;
  }
  if (started && started.session && onPending) onPending({ name, session: started.session });

  if (started && (started.kind === 'url' || started.kind === 'qr')) {
    fill(box,
      el('p', { class: 'detail' }, t(started.kind === 'url' ? 'add.auth.url' : 'add.auth.qr')),
      started.kind === 'url' ? linkRow(started.value) : qrBox(name, started.session),
      status);
  } else if (mode === 'app_secret') {
    // Nothing failed: the account authorizes as somebody's own registered
    // OAuth application, and that registration's secret has never been
    // stored. Asking for it here is the difference between finishing in the
    // browser and being sent to a terminal to hand-edit credentials.
    fill(box,
      el('p', { class: 'detail' }, refused.message),
      appSecretForm(name, () => attempt(opts, box)));
  } else if (refused) {
    // The daemon's own sentence, in the language this page asked for. It
    // stays on the row as well as in the toast: a toast fades, and the row is
    // where the person is looking for the reason nothing happened.
    status.textContent = '✕ ' + refused.message;
    status.style.color = 'var(--danger-text)';
    fill(box, status);
    toast(refused.message, 'bad');
  } else {
    fill(box,
      el('p', { class: 'detail' }, t('add.auth.term')),
      codeBox((created && created.next_command) || ('cloudfs config auth ' + name)),
      created && created.credentials
        ? el('div', { class: 'dim', style: 'font-size:12px' }, created.credentials)
        : null);
  }

  if (started && started.session) {
    pollAuthorization({ name, session: started.session, status, alive, onSettled });
  }
  return { started, status, refused };
}

// appSecretForm collects the OAuth application secret — the only secret-shaped
// value this page ever asks for, and an exception made on purpose.
//
// It is not the account's credential: it identifies the application the
// authorization runs as, the person registered that application themselves
// (Google Drive gives no choice — its scope is restricted, so no application
// ships with the binary), and they are its only source. The token the
// application then obtains is still exchanged and stored by the daemon and
// never comes near this page. The value is posted once, never read back, and
// the field is cleared the moment the daemon has it.
function appSecretForm(name, retry) {
  const input = el('input', {
    type: 'password', autocomplete: 'off', spellcheck: 'false',
    placeholder: t('auth.appsecret.ph'), style: 'flex-grow:1',
  });
  const save = el('button', { class: 'primary' }, t('auth.appsecret.save'));
  async function submit() {
    const secret = input.value.trim();
    if (!secret) { toast(t('auth.appsecret.empty'), 'bad'); return; }
    save.disabled = true;
    input.disabled = true;
    try {
      await api.post('/accounts/' + encodeURIComponent(name) + '/auth/app-secret', { client_secret: secret });
      input.value = '';
      toast(t('auth.appsecret.saved'));
      await retry();
    } catch (e) {
      save.disabled = false;
      input.disabled = false;
      toast(e instanceof ApiError ? e.message : String(e), 'bad');
    }
  }
  save.addEventListener('click', submit);
  input.addEventListener('keydown', (ev) => { if (ev.key === 'Enter') submit(); });
  return el('div', { style: 'display:grid;gap:8px' },
    el('div', { class: 'row', style: 'gap:9px;align-items:center' }, input, save),
    el('div', { class: 'dim', style: 'font-size:12px' }, t('auth.appsecret.note')));
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
