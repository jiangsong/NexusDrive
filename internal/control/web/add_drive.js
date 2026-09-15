// The add-drive modal: create a connection entirely in the UI. It picks a
// backend, collects the public (non-credential) fields, POSTs /accounts, then
// runs the credential step in place — a daemon-driven browser/QR authorization
// where the backend supports it, or the exact terminal command where the secret
// cannot travel through a browser. Which backends support which is the API's
// answer at runtime, not a list kept here. It never renders a field for a
// credential secret: Quark's QR flow obtains its cookie in the daemon without
// asking the page to collect it.
import { api, ApiError } from '/ui/api.js';
import { el, fill, toast, openPanel } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { startAuthorization } from '/ui/auth_step.js';

export async function openAddDrive(opts = {}) {
  let meta, mounts, pools;
  try {
    meta = await api.get('/accounts');
    mounts = await api.get('/mounts').catch(() => ({ mounts: [] }));
    pools = await api.get('/pool/status').catch(() => ({ pools: [] }));
  } catch (e) {
    toast(e.message, 'bad');
    return;
  }
  if (!meta.configurable) {
    toast(t('add.noconfig'), 'bad');
    return;
  }
  const types = (meta.types || []).slice().sort((a, b) => a.type.localeCompare(b.type));
  const mountPath = (mounts.mounts && mounts.mounts[0] && mounts.mounts[0].path) || '';
  const poolNames = (pools.pools || []).map((p) => p.name);
  const defaultPool = opts.pool || poolNames[0] || '';

  // One shell for every modal in the app: scrim, dialog semantics, focus
  // trap, focus restore, Escape. This sheet replaces its own body as the
  // flow advances, so it renders into a container rather than a fixed tree.
  const body = el('div');
  const dismiss = openPanel({
    title: t('add.title'), content: body, width: 600,
    onEscape: () => close(),
  });
  let pending = null; // { name, session }
  let created = false;
  // Closed before the session arrived. POST /auth/start is a round trip, and
  // the sheet can be dismissed while it is in the air: the cancel below then
  // found no session to cancel, and the session turned up a moment later and
  // was stored on a sheet nobody could see. The flow kept the one loopback
  // callback port until it timed out, and the next drive could not start.
  let closed = false;

  // cancelSession gives the daemon back the callback listener or device-code
  // poll it is holding for this flow. There is one per account at a time, so
  // an abandoned one is what the next attempt meets as a 409.
  function cancelSession({ name, session }) {
    api.post('/accounts/' + encodeURIComponent(name) + '/auth/cancel?session=' + encodeURIComponent(session), {}).catch(() => {});
  }

  function close() {
    closed = true;
    if (pending) {
      const p = pending;
      pending = null;
      cancelSession(p);
    }
    dismiss();
    // The list behind the sheet is stale the moment an account is written.
    if (created && opts.onDone) opts.onDone();
  }

  renderForm();

  function renderForm() {
    const typeSel = el('select', {},
      ...types.map((tp) => el('option', { value: tp.type }, tp.type)));
    const nameInput = el('input', { type: 'text', placeholder: t('add.name.ph'), autocomplete: 'off', spellcheck: 'false' });
    const fieldsHost = el('div', { style: 'display:grid;gap:10px;margin-top:6px' });
    const credNote = el('div', { class: 'dim', style: 'font-size:12px;margin-top:4px' });
    const mountWrap = el('label', { style: 'display:flex;align-items:center;gap:9px;font-size:13px' });
    const mountChk = el('input', { type: 'checkbox' });
    // A pool exists: the new drive joins it by default, and a prefix of
    // its own is the opt-in. Without a pool, the prefix is the default.
    const poolWrap = el('label', { style: 'display:flex;align-items:center;gap:9px;font-size:13px' });
    const poolChk = el('input', { type: 'checkbox' });
    const poolSel = el('select', { style: 'flex-grow:1' }, ...poolNames.map((n) => el('option', { value: n }, n)));
    // Joining a pool does not mean the first write lands in `replicas` places:
    // one member takes it, repair fills the rest in afterwards. Say it here
    // rather than let the checkbox imply otherwise.
    const poolNote = el('div', { class: 'detail', style: 'font-size:12px;margin-left:27px' });
    function refreshPoolNote() {
      const sel = (pools.pools || []).find((x) => x.name === poolSel.value);
      poolNote.textContent = poolChk.checked && sel ? t('pool.protect.async', Math.max((sel.replicas || 1) - 1, 0)) : '';
    }
    poolChk.addEventListener('change', refreshPoolNote);
    poolSel.addEventListener('change', refreshPoolNote);
    if (defaultPool) { poolChk.checked = true; poolSel.value = defaultPool; }
    if (mountPath && !defaultPool) mountChk.checked = true;
    const prefixInput = el('input', { type: 'text', style: 'flex-grow:1' });

    function refreshType() {
      const tp = types.find((x) => x.type === typeSel.value) || types[0];
      fill(fieldsHost, ...(tp.fields || []).map((f) => {
        const inp = el('input', { type: 'text', 'data-field': f.name, autocomplete: 'off', spellcheck: 'false',
          placeholder: f.example || f.default || '' });
        if (f.default) inp.value = f.default;
        return el('div', {},
          el('div', { style: 'font-size:12px;margin-bottom:3px' }, f.name,
            f.required ? el('span', { style: 'color:var(--danger-text);margin-left:6px' }, t('add.required')) : null),
          inp,
          f.prompt ? el('div', { class: 'dim', style: 'font-size:11.5px;margin-top:3px' }, f.prompt) : null);
      }));
      credNote.textContent = tp.credentials ? t('add.credstep.value', tp.credentials) : '';
      if (!prefixInput.dataset.touched) prefixInput.value = '/' + (nameInput.value || tp.type);
    }
    typeSel.addEventListener('change', refreshType);
    nameInput.addEventListener('input', () => { if (!prefixInput.dataset.touched) prefixInput.value = '/' + (nameInput.value || typeSel.value); });
    prefixInput.addEventListener('input', () => { prefixInput.dataset.touched = '1'; });

    const create = el('button', { class: 'primary' }, t('add.create'));
    const cancel = el('button', {}, t('confirm.cancel'));
    cancel.addEventListener('click', close);
    create.addEventListener('click', () => submit(typeSel.value, nameInput.value.trim(), fieldsHost, mountChk.checked, prefixInput.value.trim(), poolChk.checked ? poolSel.value : '', create));

    mountWrap.append(mountChk, el('span', {}, t('add.mount')), prefixInput);
    poolWrap.append(poolChk, el('span', {}, t('add.pool')), poolSel);

    fill(body,
      el('div', { style: 'display:grid;gap:12px' },
        labeled(t('add.type'), typeSel),
        labeled(t('add.name'), nameInput),
        fieldsHost,
        credNote,
        poolNames.length ? poolWrap : null,
        poolNames.length ? poolNote : null,
        mountPath ? mountWrap : null),
      el('div', { class: 'row', style: 'margin-top:18px;justify-content:flex-end' }, cancel, create));
    refreshType();
    refreshPoolNote();
    nameInput.focus();
  }

  async function submit(type, name, fieldsHost, doMount, prefix, pool, btn) {
    if (!name) { toast(t('add.name') + ' ' + t('add.required'), 'bad'); return; }
    const fields = {};
    for (const inp of fieldsHost.querySelectorAll('input[data-field]')) {
      if (inp.value.trim()) fields[inp.getAttribute('data-field')] = inp.value.trim();
    }
    const payload = { name, type, fields };
    if (doMount && mountPath) { payload.mount = mountPath; payload.prefix = prefix || ('/' + name); payload.mode = 'writeback'; }
    if (pool) payload.pool = pool;
    btn.disabled = true; const label = btn.textContent; btn.textContent = t('add.creating');
    try {
      const res = await api.post('/accounts', payload);
      created = true;
      await authStep(name, type, res);
    } catch (e) {
      btn.disabled = false; btn.textContent = label;
      toast(e instanceof ApiError ? e.message : String(e), 'bad');
    }
  }

  async function authStep(name, type, created) {
    const header = el('h3', {}, t('add.next'));
    const authHost = el('div', {});
    const { parts } = await startAuthorization({
      name, created,
      alive: () => body.isConnected,
      onPending: (p) => {
        // Arriving after the sheet went away: cancel it now rather than store
        // it where nothing will ever look again.
        if (closed) { cancelSession(p); return; }
        pending = p;
      },
      onSettled: () => { pending = null; },
    });
    fill(authHost, ...parts);
    const restart = el('button', { class: 'danger', onclick: doRestart }, t('add.restart'));
    const finish = el('button', { class: 'primary', onclick: () => { close(); } }, t('add.finish'));
    fill(body, header, authHost,
      el('div', { class: 'detail', style: 'margin-top:14px;padding-top:12px;border-top:1px solid var(--hairline)' }, t('add.saved')),
      el('div', { class: 'row', style: 'margin-top:14px;justify-content:flex-end' }, restart, finish));
  }

  async function doRestart() {
    try { await api.post('/daemon/restart?confirm=true', {}); toast(t('toast.restarting')); close(); }
    catch (e) { if (e.status) toast(e.message, 'bad'); else { toast(t('toast.restarting')); close(); } }
  }

  function labeled(label, control) {
    return el('div', {}, el('div', { style: 'font-size:12px;margin-bottom:4px' }, label), control);
  }
}
