// The connection settings overlay: one drive's own settings, its reachability
// probe, and the mount layouts that point at it. Everything here edits the
// configuration file through the guarded control routes — the daemon keeps
// serving the layout it started with until it restarts, which every reply
// says and this panel repeats once, at the bottom, rather than per field.
//
// It never renders a credential field. Passwords, cookies and tokens are
// collected by the daemon itself or typed in a terminal; the browser is not
// on that path, and `rejectSecretFields` refuses them server-side anyway.
import { api, ApiError } from '/ui/api.js';
import { el, fill, toast, confirmDelete, openPanel } from '/ui/ui.js';
import { t } from '/ui/i18n.js';

const MODES = ['writeback', 'strict', 'readonly'];

export async function openConnection(name, opts = {}) {
  let detail, mountCfg;
  try {
    // Neither read depends on the other, and the panel needs both before it
    // can draw anything.
    [detail, mountCfg] = await Promise.all([
      api.get('/accounts/' + encodeURIComponent(name)),
      api.get('/mounts').catch(() => ({ mounts: [] })),
    ]);
  } catch (e) {
    toast(e instanceof ApiError ? e.message : String(e), 'bad');
    return;
  }

  // The panel's body is replaced on every reload, so the shell is opened
  // once around a container the renders fill. openPanel owns the scrim, the
  // dialog semantics, the focus trap and giving the page back.
  const body = el('div');
  const dismiss = openPanel({
    title: t('conn.title', name), content: body, width: 640,
    onEscape: () => close(),
  });
  function close() {
    dismiss();
    if (opts.onClose) opts.onClose();
  }
  render();

  async function reload() {
    // Neither read depends on the other, and the panel needs both before it
    // can draw anything.
    [detail, mountCfg] = await Promise.all([
      api.get('/accounts/' + encodeURIComponent(name)),
      api.get('/mounts').catch(() => ({ mounts: [] })),
    ]);
    render();
  }

  function render() {
    const proxyInput = el('input', { type: 'text', value: detail.proxy || '', placeholder: t('conn.proxy.ph'), autocomplete: 'off', spellcheck: 'false' });
    const qps = detail.qps || {};
    const meta = numberInput(qps.meta);
    const down = numberInput(qps.download);
    const up = numberInput(qps.upload);
    const xfer = numberInput(qps.transfer);
    const workers = numberInput(detail.upload_workers, '1');
    const fieldInputs = new Map();
    const fieldNames = Object.keys(detail.fields || {}).sort();
    const fieldsHost = el('div', { style: 'display:grid;gap:8px' },
      ...fieldNames.map((k) => {
        const inp = el('input', { type: 'text', value: detail.fields[k], autocomplete: 'off', spellcheck: 'false' });
        fieldInputs.set(k, inp);
        return labeled(k, inp);
      }),
      fieldNames.length ? null : el('div', { class: 'dim', style: 'font-size:12px' }, t('conn.fields.empty')));

    const checkOut = el('span', { class: 'dim', style: 'font-size:12px' });
    const checkBtn = el('button', {}, t('action.check'));
    checkBtn.addEventListener('click', () => runCheck(checkBtn, checkOut));

    const save = el('button', { class: 'primary' }, t('conn.save'));
    save.addEventListener('click', () => {
      const patch = {};
      const proxy = proxyInput.value.trim();
      if (proxy !== (detail.proxy || '')) patch.proxy = proxy;
      const nextQPS = { meta: numberOf(meta), download: numberOf(down), upload: numberOf(up), transfer: numberOf(xfer) };
      if (nextQPS.meta !== (qps.meta || 0) || nextQPS.download !== (qps.download || 0) || nextQPS.upload !== (qps.upload || 0) || nextQPS.transfer !== (qps.transfer || 0)) patch.qps = nextQPS;
      const w = numberOf(workers);
      if (w !== (detail.upload_workers || 0)) patch.upload_workers = w;
      const fields = {};
      for (const [k, inp] of fieldInputs) {
        const now = inp.value.trim();
        // A cleared box removes the key. JSON null is what the daemon reads
        // as "delete this field"; "" would be an empty scalar, which means
        // the same thing to a driver and nothing to a reader.
        if (now !== detail.fields[k]) fields[k] = now === '' ? null : now;
      }
      if (Object.keys(fields).length) patch.fields = fields;
      applyPatch(patch, save);
    });

    fill(body,
      el('div', { class: 'row', style: 'align-items:baseline;gap:10px' },
        el('span', { class: 'dim', style: 'font-size:12px' }, detail.type),
        el('span', { class: 'dim', style: 'font-size:12px' }, detail.live ? t('conn.live') : t('conn.offline')),
        el('span', { class: 'dim', style: 'font-size:12px' }, detail.has_credentials ? t('conn.hascreds') : t('conn.nocreds'))),

      section(t('conn.settings'),
        labeled(t('conn.proxy'), proxyInput),
        el('div', { style: 'display:grid;grid-template-columns:repeat(4,1fr);gap:10px' },
          labeled(t('conn.qps.meta'), meta), labeled(t('conn.qps.download'), down), labeled(t('conn.qps.upload'), up),
          labeled(t('conn.qps.transfer'), xfer)),
        labeled(t('conn.workers'), workers),
        el('div', { class: 'dim', style: 'font-size:11.5px' }, t('conn.zero')),
        el('div', { class: 'row', style: 'gap:9px;align-items:center' }, save, checkBtn, checkOut)),

      section(t('conn.fields'), fieldsHost),
      section(t('conn.mounts'), mountsTable()),
      detail.caps ? section(t('conn.caps'), capsList(detail.caps)) : null,

      el('div', { class: 'detail', style: 'margin-top:16px;padding-top:12px;border-top:1px solid var(--hairline)' }, t('conn.restart')),
      el('div', { class: 'row', style: 'margin-top:12px;justify-content:flex-end' },
        el('button', { onclick: close }, t('conn.close'))));
  }

  function mountsTable() {
    const rows = (detail.mounts || []).map((m) => {
      const unbind = el('button', { class: 'danger' }, t('conn.mount.unbind'));
      unbind.addEventListener('click', () => unbindMount(m, unbind));
      return el('div', { class: 'row', style: 'gap:9px;align-items:center' },
        el('code', { style: 'flex-grow:1;font-size:12px;word-break:break-all' }, m.path + m.prefix),
        el('span', { class: 'dim', style: 'font-size:12px' }, t('mode.' + (m.mode || 'writeback'))),
        unbind);
    });
    if (!rows.length) rows.push(el('div', { class: 'dim', style: 'font-size:12px' }, t('conn.mounts.empty')));

    // The mount path is whatever this configuration already declares; the page
    // does not invent one, because a path with no mount block is a layout the
    // daemon would never read.
    const paths = [...new Set((mountCfg.mounts || []).map((m) => m.path))];
    const pathSel = el('select', {}, ...paths.map((p) => el('option', { value: p }, p)));
    const prefixInput = el('input', { type: 'text', value: '/' + name, autocomplete: 'off', spellcheck: 'false' });
    const modeSel = el('select', {}, ...MODES.map((m) => el('option', { value: m }, t('mode.' + m))));
    const add = el('button', {}, t('conn.mount.add'));
    add.addEventListener('click', () => bindMount(pathSel.value, prefixInput.value.trim(), modeSel.value, add));

    return el('div', { style: 'display:grid;gap:9px' },
      ...rows,
      paths.length
        ? el('div', { class: 'row', style: 'gap:9px;align-items:center;border-top:1px solid var(--hairline);padding-top:10px' },
          pathSel, prefixInput, modeSel, add)
        : el('div', { class: 'dim', style: 'font-size:12px' }, t('conn.mounts.nopath')));
  }

  async function applyPatch(patch, btn) {
    if (!Object.keys(patch).length) { toast(t('conn.nochange')); return; }
    btn.disabled = true;
    try {
      await api.patch('/accounts/' + encodeURIComponent(name), patch);
      toast(t('conn.saved', name));
      await reload();
    } catch (e) {
      toast(e instanceof ApiError ? e.message : String(e), 'bad');
    } finally { btn.disabled = false; }
  }

  async function runCheck(btn, out) {
    btn.disabled = true;
    out.textContent = t('conn.checking');
    out.style.color = '';
    try {
      const r = await api.post('/accounts/' + encodeURIComponent(name) + '/check', {});
      if (r.ok) { out.textContent = '✓ ' + t('conn.check.ok'); out.style.color = 'var(--ok)'; }
      else { out.textContent = '✕ ' + (r.error || t('conn.check.fail')); out.style.color = 'var(--danger-text)'; }
    } catch (e) {
      out.textContent = '✕ ' + (e instanceof ApiError ? e.message : String(e));
      out.style.color = 'var(--danger-text)';
    } finally { btn.disabled = false; }
  }

  async function bindMount(path, prefix, mode, btn) {
    if (!path || !prefix) { toast(t('conn.mount.needprefix'), 'bad'); return; }
    btn.disabled = true;
    try {
      await api.post('/mounts', { path, prefix, remote: name, mode });
      toast(t('conn.mount.bound', path + prefix));
      await reload();
    } catch (e) {
      toast(e instanceof ApiError ? e.message : String(e), 'bad');
    } finally { btn.disabled = false; }
  }

  async function unbindMount(m, btn) {
    const target = m.path + m.prefix;
    const ok = await confirmDelete({
      title: t('conn.mount.unbind.title', target), body: t('conn.mount.unbind.body'),
      confirmToken: m.prefix, confirmLabel: t('conn.mount.unbind'),
    });
    if (!ok) return;
    btn.disabled = true;
    try {
      await api.del('/mounts?path=' + encodeURIComponent(m.path) + '&prefix=' + encodeURIComponent(m.prefix) + '&confirm=true');
      toast(t('conn.mount.unbound', target));
      await reload();
    } catch (e) {
      toast(e instanceof ApiError ? e.message : String(e), 'bad');
    } finally { btn.disabled = false; }
  }
}

function section(title, ...children) {
  return el('div', { style: 'margin-top:16px' },
    el('div', { class: 'eyebrow', style: 'margin-bottom:8px' }, title),
    el('div', { style: 'display:grid;gap:10px' }, ...children));
}
function labeled(label, control) {
  return el('div', {}, el('div', { style: 'font-size:12px;margin-bottom:4px' }, label), control);
}
function numberInput(value, step = '0.1') {
  return el('input', { type: 'text', inputmode: 'decimal', value: value ? String(value) : '', placeholder: '0', 'data-step': step, autocomplete: 'off' });
}
function numberOf(input) {
  const n = Number(input.value.trim());
  return Number.isFinite(n) && n > 0 ? n : 0;
}
function capsList(caps) {
  const yes = (b) => (b ? t('caps.yes') : t('caps.no'));
  const items = [
    [t('caps.range'), yes(caps.range_read)],
    [t('caps.delta'), yes(caps.delta)],
    [t('caps.servercopy'), yes(caps.server_copy)],
    [t('caps.servermove'), yes(caps.server_move)],
    [t('caps.rapid'), (caps.rapid_upload || []).join(', ') || t('caps.no')],
    [t('caps.tier'), caps.tier ? t('tier.' + caps.tier) : '-'],
  ];
  return el('div', { style: 'display:grid;grid-template-columns:repeat(2,1fr);gap:6px;font-size:12px' },
    ...items.map(([k, v]) => el('div', { class: 'row', style: 'gap:8px' },
      el('span', { class: 'dim' }, k), el('span', {}, v))));
}
