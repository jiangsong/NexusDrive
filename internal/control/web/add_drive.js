// The add-drive modal: create a connection entirely in the UI. It picks a
// backend, collects the public (non-credential) fields, POSTs /accounts, then
// runs the credential step in place — a daemon-driven browser/QR authorization
// where the backend supports it (aliyun, baidu, 115), or the exact terminal
// command where the secret cannot travel through a browser. It never renders a
// field for a password, cookie or token: that boundary is the whole point.
import { api, ApiError } from '/ui/api.js';
import { el, toast } from '/ui/ui.js';
import { t } from '/ui/i18n.js';

export async function openAddDrive() {
  let meta, mounts;
  try {
    meta = await api.get('/accounts');
    mounts = await api.get('/mounts').catch(() => ({ mounts: [] }));
  } catch (e) {
    toast(e.message, 'bad');
    return;
  }
  if (!meta.configurable) {
    toast('这个守护进程没有配置文件，无法添加网盘', 'bad');
    return;
  }
  const types = (meta.types || []).slice().sort((a, b) => a.type.localeCompare(b.type));
  const mountPath = (mounts.mounts && mounts.mounts[0] && mounts.mounts[0].path) || '';

  const opener = document.activeElement;
  const app = document.getElementById('app');
  const body = el('div');
  const sheet = el('div', { class: 'sheet', role: 'dialog', 'aria-modal': 'true', style: 'width:min(600px,calc(100% - 32px))' }, body);
  const scrim = el('div', { class: 'scrim' }, sheet);
  const close = () => { scrim.remove(); if (app) app.inert = false; if (opener && opener.focus) opener.focus(); };
  scrim.addEventListener('keydown', (e) => { if (e.key === 'Escape') close(); });
  document.getElementById('modal-root').append(scrim);
  if (app) app.inert = true;

  renderForm();

  function renderForm() {
    const typeSel = el('select', {},
      ...types.map((tp) => el('option', { value: tp.type }, tp.type)));
    const nameInput = el('input', { type: 'text', placeholder: t('add.name.ph'), autocomplete: 'off', spellcheck: 'false' });
    const fieldsHost = el('div', { style: 'display:grid;gap:10px;margin-top:6px' });
    const credNote = el('div', { class: 'dim', style: 'font-size:12px;margin-top:4px' });
    const mountWrap = el('label', { style: 'display:flex;align-items:center;gap:9px;font-size:13px' });
    const mountChk = el('input', { type: 'checkbox' });
    if (mountPath) mountChk.checked = true;
    const prefixInput = el('input', { type: 'text', style: 'flex-grow:1' });

    function refreshType() {
      const tp = types.find((x) => x.type === typeSel.value) || types[0];
      fieldsHost.replaceChildren(...(tp.fields || []).map((f) => {
        const inp = el('input', { type: 'text', 'data-field': f.name, autocomplete: 'off', spellcheck: 'false',
          placeholder: f.example || f.default || '' });
        if (f.default) inp.value = f.default;
        return el('div', {},
          el('div', { style: 'font-size:12px;margin-bottom:3px' }, f.name,
            f.required ? el('span', { style: 'color:var(--danger-text);margin-left:6px' }, t('add.required')) : null),
          inp,
          f.prompt ? el('div', { class: 'dim', style: 'font-size:11.5px;margin-top:3px' }, f.prompt) : null);
      }));
      credNote.textContent = tp.credentials ? (t('add.credstep') + '：' + tp.credentials) : '';
      if (!prefixInput.dataset.touched) prefixInput.value = '/' + (nameInput.value || tp.type);
    }
    typeSel.addEventListener('change', refreshType);
    nameInput.addEventListener('input', () => { if (!prefixInput.dataset.touched) prefixInput.value = '/' + (nameInput.value || typeSel.value); });
    prefixInput.addEventListener('input', () => { prefixInput.dataset.touched = '1'; });

    const create = el('button', { class: 'primary' }, t('add.create'));
    const cancel = el('button', {}, t('confirm.cancel'));
    cancel.addEventListener('click', close);
    create.addEventListener('click', () => submit(typeSel.value, nameInput.value.trim(), fieldsHost, mountChk.checked, prefixInput.value.trim(), create));

    mountWrap.append(mountChk, el('span', {}, t('add.mount')), prefixInput);

    body.replaceChildren(
      el('h3', {}, t('add.title')),
      el('div', { style: 'display:grid;gap:12px' },
        labeled(t('add.type'), typeSel),
        labeled(t('add.name'), nameInput),
        fieldsHost,
        credNote,
        mountPath ? mountWrap : null),
      el('div', { class: 'row', style: 'margin-top:18px;justify-content:flex-end' }, cancel, create));
    refreshType();
    nameInput.focus();
  }

  async function submit(type, name, fieldsHost, doMount, prefix, btn) {
    if (!name) { toast(t('add.name') + ' ' + t('add.required'), 'bad'); return; }
    const fields = {};
    for (const inp of fieldsHost.querySelectorAll('input[data-field]')) {
      if (inp.value.trim()) fields[inp.getAttribute('data-field')] = inp.value.trim();
    }
    const payload = { name, type, fields };
    if (doMount && mountPath) { payload.mount = mountPath; payload.prefix = prefix || ('/' + name); payload.mode = 'writeback'; }
    btn.disabled = true; const label = btn.textContent; btn.textContent = t('add.creating');
    try {
      const res = await api.post('/accounts', payload);
      await authStep(name, type, res);
    } catch (e) {
      btn.disabled = false; btn.textContent = label;
      toast(e instanceof ApiError ? e.message : String(e), 'bad');
    }
  }

  async function authStep(name, type, created) {
    const parts = [el('h3', {}, t('add.next'))];
    const status = el('div', { class: 'dim', style: 'font-size:12.5px' });
    let started = null;
    try {
      started = await api.post('/accounts/' + encodeURIComponent(name) + '/auth/start', {});
    } catch (e) {
      started = null; // not a browser-drivable type; fall through to the terminal command
    }
    if (started && started.kind === 'url') {
      parts.push(el('p', { class: 'detail' }, t('add.auth.url')));
      parts.push(linkRow(started.value));
      parts.push(status);
      pollAuth(name, started.session, status);
    } else if (started && started.kind === 'qr') {
      parts.push(el('p', { class: 'detail' }, t('add.auth.qr')));
      parts.push(codeBox(started.value));
      parts.push(status);
      pollAuth(name, started.session, status);
    } else {
      parts.push(el('p', { class: 'detail' }, t('add.auth.term')));
      parts.push(codeBox(created.next_command || ('cloudfs config auth ' + name)));
      if (created.credentials) parts.push(el('div', { class: 'dim', style: 'font-size:12px' }, created.credentials));
    }
    parts.push(el('div', { class: 'detail', style: 'margin-top:14px;padding-top:12px;border-top:1px solid var(--hairline)' }, t('add.saved')));
    const restart = el('button', { class: 'danger', onclick: doRestart }, t('add.restart'));
    const finish = el('button', { class: 'primary', onclick: () => { close(); } }, t('add.finish'));
    parts.push(el('div', { class: 'row', style: 'margin-top:14px;justify-content:flex-end' }, restart, finish));
    body.replaceChildren(...parts);
  }

  async function pollAuth(name, session, statusEl) {
    statusEl.textContent = t('add.waiting');
    for (let i = 0; i < 150; i++) {
      await new Promise((r) => setTimeout(r, 2000));
      if (!document.body.contains(scrim)) return; // sheet closed
      let st;
      try { st = await api.get('/accounts/' + encodeURIComponent(name) + '/auth/status?session=' + encodeURIComponent(session)); }
      catch (_) { continue; }
      if (st.state === 'done') { statusEl.textContent = '✓ ' + t('add.done'); statusEl.style.color = 'var(--ok)'; return; }
      if (st.state === 'denied' || st.state === 'error') { statusEl.textContent = '✕ ' + t('add.denied') + (st.error ? '：' + st.error : ''); statusEl.style.color = 'var(--danger-text)'; return; }
    }
  }

  async function doRestart() {
    try { await api.post('/daemon/restart?confirm=true', {}); toast('守护进程正在重启…'); close(); }
    catch (e) { if (e.status) toast(e.message, 'bad'); else { toast('守护进程正在重启…'); close(); } }
  }

  function labeled(label, control) {
    return el('div', {}, el('div', { style: 'font-size:12px;margin-bottom:4px' }, label), control);
  }
  function linkRow(url) {
    const a = el('a', { href: url, target: '_blank', rel: 'noopener', style: 'color:var(--accent-text);word-break:break-all' }, url);
    return el('div', { style: 'display:flex;gap:9px;align-items:center;flex-wrap:wrap' },
      el('a', { href: url, target: '_blank', rel: 'noopener', class: 'btn' }, t('add.auth.open')), copyBtn(url), a);
  }
  function codeBox(text) {
    return el('div', { style: 'display:flex;gap:9px;align-items:flex-start' },
      el('div', { style: 'flex-grow:1;font-family:ui-monospace,monospace;font-size:12px;background:#0a0f16;border:1px solid var(--hairline);border-radius:6px;padding:9px;word-break:break-all' }, text),
      copyBtn(text));
  }
  function copyBtn(text) {
    const b = el('button', {}, t('add.copy'));
    b.addEventListener('click', async () => {
      try { await navigator.clipboard.writeText(text); b.textContent = t('add.copied'); setTimeout(() => { b.textContent = t('add.copy'); }, 1500); }
      catch (_) { toast(text); }
    });
    return b;
  }
}
