// The guided setup: from nothing to a mounted, redundant pool.
//
// It exists because the product's own onboarding used to be unreachable. Adding
// a drive means a browser authorization, the screen that drives one lives in
// this control plane, and the control plane only runs inside a daemon that
// refuses to start without a configuration naming a mount and a remote. So the
// first thing a new person met was an instruction to hand-write YAML from a
// design document. `cloudfs setup` serves this screen with nothing behind it.
//
// Two constraints shape the whole flow:
//
//   - Only one drive can authorize at a time. The OAuth callback binds one
//     fixed loopback port and providers that require an exactly-registered
//     redirect URI leave no room for an ephemeral one, so a second attempt
//     fails with "address already in use". The screen never offers two.
//   - The daemon must restart for any of this to take effect, and it re-execs
//     in place — same PID, same address — so the page simply waits and polls
//     rather than telling someone to come back later.
//
// Progress lives on the server, not here. localStorage holds only the intent
// (which drives, how many copies, which folder); every question about what has
// actually happened is answered by /accounts, /pool/status and /mounts. That is
// what makes "I closed the browser after two drives" and "someone ran cloudfs
// config add in a terminal" the same case.

import { api, ApiError } from '/ui/api.js';
import { el, fill, toast, bytes } from '/ui/ui.js';
import { t } from '/ui/i18n.js';
import { planSetup, nextAuthTarget } from '/ui/setup_plan.js';
import { startAuthorization } from '/ui/auth_step.js';

const INTENT_KEY = 'cloudfs.setup.v1';
const TOTAL_STEPS = 6;

function loadIntent() {
  try {
    const raw = localStorage.getItem(INTENT_KEY);
    return raw ? JSON.parse(raw) : null;
  } catch (_) {
    return null; // a browser with storage disabled starts from the server's truth
  }
}

function saveIntent(intent) {
  try { localStorage.setItem(INTENT_KEY, JSON.stringify(intent)); } catch (_) { /* not worth failing over */ }
}

export function renderSetup(host) {
  let intent = loadIntent() || { drives: [], replicas: 2, mountPath: '' };
  let plan = { step: 0, drives: [], resumed: false };
  let types = [];
  // Where the daemon keeps the file it is editing. `cloudfs setup` is exactly
  // the case where that is not the default path, so a terminal command
  // printed without it would edit a different configuration.
  let configPath = '';
  let pending = null;
  // One authorization in the air at a time — see renderConnect. `pending` is
  // the single slot the unmount handler cancels, so there must never be a
  // second flow for it to miss.
  let busy = false;
  let syncConnectButtons = () => {};
  let alive = true;
  // What each drive answered when it was checked: a total of 0 means the
  // backend reports nothing, which is what decides whether to ask for a
  // capacity. planSetup's drives carry identity only — this is the one fact
  // that comes from checking an account, not from listing the configuration.
  //
  // It is seeded from the intent and written back to it because the wizard
  // outlives the page: a reload, or the daemon's own re-exec, used to leave
  // the map empty and step 3 then asked for a manual capacity for every
  // drive, including the ones whose backend had already answered.
  const space = new Map(Object.entries((intent && typeof intent.space === 'object' && intent.space) || {}));
  // Drives already asked, so a backend that cannot answer is asked once
  // rather than on every render.
  const probed = new Set(space.keys());

  const body = el('div', { class: 'stack', style: 'padding:20px;max-width:720px' });
  const stepLine = el('div', { class: 'eyebrow' });

  host.append(el('div', { class: 'pad' },
    el('div', {}, stepLine, el('h2', { class: 'section', style: 'margin:6px 0 0' }, t('setup.title'))),
  ), body);

  load();

  async function load(forceStep) {
    try {
      const [accounts, pools, mounts] = await Promise.all([
        api.get('/accounts'),
        api.get('/pool/status').catch(() => ({ pools: [] })),
        api.get('/mounts').catch(() => ({ mounts: [] })),
      ]);
      types = (accounts && accounts.types) || [];
      configPath = (accounts && accounts.config_path) || configPath;
      plan = planSetup({ accounts, pools, mounts, intent });
      if (forceStep != null) plan = { ...plan, step: forceStep };
      if (plan.drives.length && !intent.drives.length) {
        intent = { ...intent, drives: plan.drives.map((d) => ({ name: d.name, type: d.type })) };
        saveIntent(intent);
      }
      render();
    } catch (e) {
      fill(body, el('div', { class: 'panel pad' }, e instanceof ApiError ? e.message : String(e)));
    }
  }

  function render() {
    stepLine.textContent = t('setup.step', String(plan.step + 1), String(TOTAL_STEPS));
    switch (plan.step) {
      case 0: return renderWelcome();
      case 1: return renderChoose();
      case 2: return renderConnect();
      case 3: return renderReplicas();
      case 4: return renderPlace();
      default: return renderFinish();
    }
  }

  function panel(...nodes) { return el('div', { class: 'panel pad', style: 'display:grid;gap:12px' }, ...nodes); }

  function nav(back, next) {
    const row = el('div', { class: 'row', style: 'justify-content:flex-end;gap:9px;margin-top:6px' });
    if (back) row.append(el('button', { onclick: back }, t('setup.back')));
    if (next) row.append(el('button', { class: 'primary', onclick: next }, t('setup.next')));
    return row;
  }

  function goto(step) { plan = { ...plan, step }; render(); }

  function renderWelcome() {
    fill(body, panel(
      el('p', { class: 'detail' }, t('setup.welcome.body')),
      nav(null, () => goto(1)),
    ));
  }

  function renderChoose() {
    const rows = el('div', { class: 'stack' });
    const draw = () => {
      fill(rows, ...intent.drives.map((d, i) => driveRow(d, i, draw)));
      if (!intent.drives.length) rows.append(el('div', { class: 'dim' }, t('setup.drives.min')));
    };
    const add = el('button', { onclick: () => {
      const type = browserTypes()[0] || (types[0] && types[0].type) || 'webdav';
      const name = suggestName(type);
      intent = {
        ...intent,
        drives: [...intent.drives, { name, type }],
        // Adding a drive back under a name that was removed is someone
        // changing their mind, and the exclusion must not outlive it.
        excluded: exclusions().filter((x) => x !== name),
      };
      saveIntent(intent);
      draw();
    } }, t('setup.drives.add'));
    draw();
    fill(body, panel(
      el('h3', { style: 'margin:0' }, t('setup.drives.title')),
      el('p', { class: 'detail' }, t('setup.drives.body')),
      plan.resumed ? el('div', { class: 'dim' }, t('setup.resume.found', String(plan.drives.filter((d) => d.exists).length))) : null,
      rows,
      el('div', { class: 'row' }, add),
      nav(() => goto(0), () => {
        if (intent.drives.length < 2) { toast(t('setup.drives.min'), 'bad'); return; }
        // A required field left blank is refused by the daemon when the
        // account is created, halfway through the connect step. Asking here
        // costs one glance; finding out there costs the flow.
        for (const d of intent.drives) {
          const known = plan.drives.find((x) => x.name === d.name);
          if (known && known.exists) continue;
          for (const f of fieldsFor(d.type)) {
            if (f.required && !((d.fields || {})[f.name] || '').trim()) {
              toast(d.name + ': ' + (f.prompt || f.name) + ' ' + t('add.required'), 'bad');
              return;
            }
          }
        }
        // Reload rather than just changing the step: the drive list is what
        // step 2 renders, and it is derived from the server. Advancing without
        // recomputing it showed an empty connect screen on a first run, since
        // the plan was last computed when nothing had been chosen yet.
        load();
      }),
    ));
  }

  // exclusions is the removed-drive list as it stands. The intent comes back
  // from localStorage, where it can be anything at all — an older shape, or a
  // hand-edited one — so nothing reads a field from it without checking.
  function exclusions() {
    return Array.isArray(intent.excluded) ? intent.excluded : [];
  }

  function browserTypes() {
    return types.filter((ty) => ty.browser_auth).map((ty) => ty.type);
  }

  function suggestName(type) {
    let n = 1;
    const taken = new Set(intent.drives.map((d) => d.name));
    while (taken.has(type + '-' + n)) n++;
    return type + '-' + n;
  }

  // fieldsFor is what the daemon says this backend needs before it can be
  // authorized — a Google client id, a Dropbox app key, a WebDAV URL. The
  // wizard has to collect them: an account created with none of them fails at
  // the authorization step with "client_id is required", which is a wall in
  // the middle of the flow rather than a question at the start of it.
  function fieldsFor(type) {
    const ty = types.find((x) => x.type === type);
    return (ty && ty.fields) || [];
  }

  function driveRow(d, index, redraw) {
    const known = plan.drives.find((x) => x.name === d.name);
    const name = el('input', { type: 'text', value: d.name, autocomplete: 'off', spellcheck: 'false' });
    name.addEventListener('change', () => {
      intent = { ...intent, drives: intent.drives.map((x, i) => (i === index ? { ...x, name: name.value.trim() } : x)) };
      saveIntent(intent);
    });
    const type = el('select', {}, ...types.map((ty) => el('option', { value: ty.type }, ty.type)));
    type.value = d.type;
    type.addEventListener('change', () => {
      intent = { ...intent, drives: intent.drives.map((x, i) => (i === index ? { ...x, type: type.value } : x)) };
      saveIntent(intent);
    });
    // An account the daemon already knows about is not ours to rename or retype.
    if (known && known.exists) { name.disabled = true; type.disabled = true; }
    const remove = el('button', { class: 'danger', onclick: () => {
      // Dropping the row is not enough for a drive the daemon already has:
      // the next load reads /accounts and planSetup adopts it straight back,
      // so it returned at step 2 and joined the pool. The removal has to be
      // recorded as a decision. It removes the drive from *this pool*, not
      // from the configuration — `cloudfs config remove` is what deletes an
      // account, and doing that from here would throw away a credential
      // somebody just obtained.
      const dropped = intent.drives[index];
      const excluded = exclusions().filter((x) => x !== dropped.name);
      intent = {
        ...intent,
        drives: intent.drives.filter((_, i) => i !== index),
        excluded: [...excluded, dropped.name],
      };
      saveIntent(intent);
      redraw();
    } }, t('setup.drives.remove'));
    const fieldsHost = el('div', { style: 'display:grid;gap:6px' });
    const drawFields = () => {
      const declared = fieldsFor(type.value);
      fill(fieldsHost, ...declared.map((f) => {
        const box = el('input', {
          type: 'text', autocomplete: 'off', spellcheck: 'false',
          value: ((d.fields || {})[f.name]) || '',
          placeholder: f.example || '',
          style: 'min-width:240px',
        });
        if (known && known.exists) box.disabled = true;
        box.addEventListener('change', () => {
          intent = {
            ...intent,
            drives: intent.drives.map((x, i) => (i === index
              ? { ...x, fields: { ...(x.fields || {}), [f.name]: box.value.trim() } }
              : x)),
          };
          saveIntent(intent);
        });
        return el('div', { class: 'row', style: 'gap:9px;align-items:center' },
          el('span', { class: 'dim', style: 'min-width:150px;font-size:12px' },
            f.prompt || f.name, f.required ? ' *' : ''),
          box);
      }));
    };
    type.addEventListener('change', drawFields);
    drawFields();
    return el('div', { style: 'display:grid;gap:8px' },
      el('div', { class: 'row', style: 'gap:9px;align-items:center' }, name, type, el('span', { style: 'flex-grow:1' }), remove),
      fieldsHost);
  }

  function renderConnect() {
    const target = nextAuthTarget(plan.drives);
    const buttons = [];
    const rows = plan.drives.map((d) => connectRow(d, target, buttons));
    // Exactly one authorize button is live at any moment, and this is the
    // rule that makes it so rather than the daemon's 409. Leaving every
    // already-connected drive's "Authorize again" enabled put two live
    // buttons on screen, and the second flow could not bind the one fixed
    // loopback callback port; worse, `pending` is a single slot, so the two
    // sessions overlapped and the unmount handler could only ever cancel one
    // of them — the other kept the port until it timed out.
    //
    // While a flow is running nothing is offered, including its own button:
    // starting the same drive twice orphans the first session just as surely.
    // With every drive connected there is no target left, and then "Authorize
    // again" is the only thing on the step, so each row may offer it.
    syncConnectButtons = () => {
      for (const b of buttons) b.el.disabled = busy || (target ? b.name !== target : false);
    };
    syncConnectButtons();
    fill(body, panel(
      el('h3', { style: 'margin:0' }, t('setup.connect.title')),
      el('p', { class: 'detail' }, t('setup.connect.body')),
      el('div', { class: 'stack' }, ...rows),
      nav(() => goto(1), target ? null : () => goto(3)),
    ));
  }

  function connectRow(d, target, buttons) {
    const isNext = d.name === target;
    const state = d.authorized ? t('setup.connect.ok') : (isNext ? t('setup.connect.active') : t('setup.connect.pending'));
    const authHost = el('div', {});
    const button = el('button', { class: 'primary', onclick: () => connect(d, authHost) },
      t(d.authorized ? 'setup.connect.retry' : 'setup.connect.begin'));
    buttons.push({ name: d.name, el: button });
    return el('div', { class: 'panel pad', style: 'display:grid;gap:8px' },
      el('div', { class: 'row', style: 'align-items:center;gap:9px' },
        el('span', { style: 'font-weight:620' }, d.name),
        el('span', { class: 'muted' }, d.type),
        el('span', { style: 'flex-grow:1' }),
        el('span', { class: 'dim' }, spaceLabel(d) || state),
        button),
      authHost);
  }

  // spaceLabel says what the drive answered, once it has been asked. A drive
  // that reports nothing is not broken — dropbox-style backends publish no
  // figure — so it gets its own sentence rather than a blank.
  function spaceLabel(d) {
    if (!d.authorized) return '';
    const s = space.get(d.name);
    if (!s) return '';
    return s.total ? t('setup.connect.space', bytes(s.free)) : t('setup.connect.nospace');
  }

  async function connect(drive, authHost) {
    busy = true;
    syncConnectButtons();
    // Nothing is polling once this runs: the step is free to be started
    // again. It is called when a flow settles, when none was started at all,
    // and when the attempt threw.
    const release = () => { busy = false; syncConnectButtons(); };
    try {
      // What POST /accounts answers with matters. The terminal fallback is
      // built from `next_command`, which carries `--config <path>` — and
      // `cloudfs setup` is the one case where the configuration is not at the
      // default path, so the command printed without it edited a different
      // file. `credentials` is the sentence saying which secret that backend
      // needs. Both were thrown away.
      let created = { next_command: terminalCommand(drive.name) };
      if (!drive.exists) {
        const planned = (intent.drives.find((x) => x.name === drive.name) || {}).fields || {};
        const reply = await api.post('/accounts', { name: drive.name, type: drive.type, fields: planned });
        if (reply && typeof reply === 'object') created = reply;
      }
      const { started } = await startAuthorization({
        name: drive.name,
        created,
        host: authHost,
        alive: () => alive && host.isConnected,
        onPending: (p) => {
          // The session can arrive after the screen has gone: /auth/start is
          // a round trip and the person can navigate away inside it. Storing
          // it then would leave the flow holding the loopback port until it
          // timed out, with nothing left to cancel it.
          if (!alive || !host.isConnected) { cancelSession(p); return; }
          pending = p;
        },
        onSettled: async ({ state }) => {
          pending = null;
          release();
          if (state === 'done') await afterAuthorized(drive.name);
        },
      });
      // No session means nothing will settle — a terminal-only backend, or a
      // start the daemon refused — so the step must not stay locked.
      if (!started || !started.session) release();
    } catch (e) {
      release();
      toast(e instanceof ApiError ? e.message : String(e), 'bad');
    }
  }

  // terminalCommand is what someone would type to authorize this account by
  // hand. The path matters: the wizard runs against a configuration file that
  // is usually not the default one.
  function terminalCommand(name) {
    return 'cloudfs config auth ' + name + (configPath ? ' --config ' + configPath : '');
  }

  // cancelSession hands the daemon back the callback listener it is holding.
  // There is one per account at a time and the port is shared, so an
  // abandoned flow is what the next drive meets as a 409.
  function cancelSession({ name, session }) {
    api.post('/accounts/' + encodeURIComponent(name) + '/auth/cancel?session=' + encodeURIComponent(session), {}).catch(() => {});
  }

  async function afterAuthorized(name) {
    try {
      const check = await api.post('/accounts/' + encodeURIComponent(name) + '/check', {});
      if (check && check.ok) {
        rememberSpace(name, check);
        const known = check.total ? t('setup.connect.space', bytes(check.free)) : t('setup.connect.nospace');
        toast(name + ' — ' + known);
      }
    } catch (_) { /* the authorization already succeeded; the check is a courtesy */ }
    await load();
  }

  // rememberSpace keeps what a drive answered where the next page load can
  // still see it. The intent is the only thing that survives a reload or the
  // daemon's re-exec, and without this every drive looked like a drive that
  // reports no space.
  function rememberSpace(name, check) {
    const s = { total: check.total || 0, free: check.free || 0 };
    space.set(name, s);
    probed.add(name);
    intent = { ...intent, space: { ...(intent.space || {}), [name]: s } };
    saveIntent(intent);
  }

  // probeSpace asks the drives nobody has asked yet. Step 3 only asks a
  // person for a capacity when the backend reports none, and the evidence for
  // that is a check call — which had only ever been made in the same page
  // load that authorized the drive.
  async function probeSpace() {
    const missing = plan.drives.filter((d) => d.authorized && !probed.has(d.name));
    if (!missing.length) return;
    let learned = false;
    for (const d of missing) {
      probed.add(d.name);
      try {
        const check = await api.post('/accounts/' + encodeURIComponent(d.name) + '/check', {});
        if (check && check.ok) { rememberSpace(d.name, check); learned = true; }
      } catch (_) { /* a drive that will not answer is one this step asks about by hand */ }
    }
    // Redraw only for an answer, and only if this is still the step on
    // screen: probed now holds every name, so this cannot loop.
    if (learned && alive && host.isConnected && plan.step === 3) renderReplicas();
  }

  function renderReplicas() {
    // Anything still unasked is asked now, in the background: the answer
    // decides whether the capacity block below has a row for that drive.
    probeSpace();
    const count = plan.drives.length;
    const input = el('input', { type: 'number', min: '1', max: String(count), value: String(Math.min(intent.replicas || 2, count)), style: 'max-width:90px' });
    const explain = el('p', { class: 'detail' });
    // What repair does after the first write lands. It moves with the replica
    // count, so it is drawn with everything else that does: written once
    // during fill, it went on saying "the other 1 copies" after the number
    // was changed to 4.
    const protect = el('p', { class: 'detail' });
    const draw = () => {
      const r = Math.max(1, Math.min(Number(input.value) || 1, count));
      explain.textContent = t('setup.replicas.body', String(count), String(r), String(r), String(r - 1));
      protect.textContent = t('pool.protect.async', String(Math.max(0, r - 1)));
    };
    input.addEventListener('input', draw);
    draw();

    // Only the drives that could not report their own space need a number from
    // a person; asking about the others would invite a guess that overrides
    // the truth the backend already tells us.
    const unknown = plan.drives.filter((d) => d.authorized && !(space.get(d.name) || {}).total);
    const capacityInputs = new Map();
    const capacityBlock = unknown.length ? el('div', { style: 'display:grid;gap:8px' },
      el('div', { style: 'font-weight:620' }, t('setup.capacity.title')),
      el('p', { class: 'detail' }, t('setup.capacity.body')),
      ...unknown.map((d) => {
        // Seeded from the intent: stepping back to change the replica count
        // and forward again used to arrive at an empty box and then send an
        // empty member_capacity, silently wiping what had been typed.
        const box = el('input', {
          type: 'text', placeholder: t('setup.capacity.ph'), autocomplete: 'off',
          value: capacityText(d.name), style: 'max-width:160px',
        });
        capacityInputs.set(d.name, box);
        return el('div', { class: 'row', style: 'gap:9px;align-items:center' }, el('span', { style: 'min-width:140px' }, d.name), box);
      })) : null;

    fill(body, panel(
      el('h3', { style: 'margin:0' }, t('setup.replicas.title')),
      el('div', { class: 'row', style: 'gap:9px;align-items:center' }, input),
      explain,
      protect,
      capacityBlock,
      nav(() => goto(2), () => {
        intent = {
          ...intent,
          replicas: Math.max(1, Math.min(Number(input.value) || 1, count)),
          capacity: readCapacities(capacityInputs, intent.capacity),
        };
        saveIntent(intent);
        goto(4);
      }),
    ));
  }

  // capacityText is what was typed for this drive last time, ready to be
  // typed over. Older saved plans hold the parsed byte count instead, which
  // parseSize reads back as itself.
  function capacityText(name) {
    const saved = (intent.capacity || {})[name];
    return saved == null ? '' : String(saved);
  }

  // readCapacities updates the drives this step asked about and leaves every
  // other answer alone. Replacing the whole key meant that stepping back from
  // step 4 and forward again — or a drive answering for itself in between, so
  // that its box was no longer drawn — sent an empty member_capacity and
  // wiped what had been typed. Emptying a box on purpose still means "no
  // answer", so a blank deletes rather than preserves.
  function readCapacities(inputs, previous) {
    const out = { ...(previous || {}) };
    for (const [name, box] of inputs) {
      const text = (box.value || '').trim();
      if (parseSize(text) > 0) out[name] = text;
      else delete out[name];
    }
    return out;
  }

  // capacityBytes turns what was typed into the byte counts /pool/create
  // takes, for the drives actually joining the pool. The intent keeps the
  // text so that stepping back shows a person "2 TB" rather than
  // 2000000000000, and it outlives the drives it was typed for — one removed
  // at step 1 must not arrive as a capacity for a member that is not there.
  function capacityBytes(capacity, members) {
    const joining = new Set(members);
    const out = {};
    for (const [name, value] of Object.entries(capacity || {})) {
      if (!joining.has(name)) continue;
      const n = typeof value === 'number' ? value : parseSize(value);
      if (n > 0) out[name] = n;
    }
    return out;
  }

  // parseSize accepts what the configuration file accepts, so a number typed
  // here means the same thing it would mean written in YAML.
  function parseSize(text) {
    const m = /^\s*(\d+(?:\.\d+)?)\s*(B|KiB|MiB|GiB|TiB|PiB|KB|MB|GB|TB)?\s*$/i.exec(text || '');
    if (!m) return 0;
    const units = { b: 1, kib: 1024, mib: 1024 ** 2, gib: 1024 ** 3, tib: 1024 ** 4, pib: 1024 ** 5, kb: 1000, mb: 1000 ** 2, gb: 1000 ** 3, tb: 1000 ** 4 };
    return Math.round(Number(m[1]) * (units[(m[2] || 'b').toLowerCase()] || 1));
  }

  function renderPlace() {
    const folder = el('input', { type: 'text', value: intent.mountPath || '~/CloudFS', autocomplete: 'off', style: 'min-width:280px' });
    fill(body, panel(
      el('h3', { style: 'margin:0' }, t('setup.place.title')),
      el('p', { class: 'detail' }, t('setup.place.body')),
      el('div', { class: 'row', style: 'gap:9px;align-items:center' },
        el('span', {}, t('setup.place.folder')), folder),
      nav(() => goto(3), async () => {
        intent = { ...intent, mountPath: folder.value.trim() || '~/CloudFS' };
        saveIntent(intent);
        await createPool();
      }),
    ));
  }

  async function createPool() {
    try {
      // plan.drives is the merged list, and planSetup has already dropped
      // whatever was removed at step 1 — see `excluded` there. Building the
      // membership from anything else is how a drive someone took out of the
      // list ended up in the pool anyway.
      const members = plan.drives.filter((d) => d.authorized).map((d) => d.name);
      await api.post('/pool/create', {
        name: 'home',
        members,
        replicas: intent.replicas,
        mount: intent.mountPath,
        prefix: '/',
        member_capacity: capacityBytes(intent.capacity, members),
      });
      // The pool now exists, so the plan lands on the last step by itself;
      // forcing it keeps the screen from flickering through an intermediate
      // render on the way there.
      await load(5);
    } catch (e) {
      toast(e instanceof ApiError ? e.message : String(e), 'bad');
    }
  }

  function renderFinish() {
    const status = el('div', { class: 'dim' });
    if (plan.finished) {
      // The daemon is already serving the pool: there is nothing left to do
      // but say so and get out of the way.
      fill(body, panel(
        el('h3', { style: 'margin:0' }, t('setup.finish.title')),
        el('p', { class: 'detail' }, t('setup.finish.done', intent.mountPath || '~/CloudFS')),
        el('div', { class: 'row' }, el('a', { class: 'btn', href: '#/pool' }, t('setup.finish.open'))),
      ));
      return;
    }
    const restart = el('button', { class: 'primary', onclick: async () => {
      status.textContent = t('setup.finish.waiting');
      try { await api.post('/daemon/restart?confirm=true', {}); } catch (_) { /* the connection drops as it goes down */ }
      await waitForDaemon(status);
    } }, t('setup.finish.restart'));
    fill(body, panel(
      el('h3', { style: 'margin:0' }, t('setup.finish.title')),
      el('p', { class: 'detail' }, t('setup.finish.body')),
      el('div', { class: 'row', style: 'gap:9px' }, restart,
        el('a', { class: 'btn', href: '#/pool' }, t('setup.finish.open'))),
      status,
    ));
  }

  async function waitForDaemon(status) {
    for (let i = 0; i < 60; i++) {
      await new Promise((r) => setTimeout(r, 1000));
      if (!host.isConnected) return;
      try {
        await api.get('/status');
        status.textContent = t('setup.finish.done', intent.mountPath || '~/CloudFS');
        return;
      } catch (_) { /* still down */ }
    }
    status.textContent = t('setup.restart.slow', 'cloudfs mount');
  }

  // Leaving the screen gives back whatever the daemon is holding. A session
  // still in flight is handled at onPending, which sees `alive` go false.
  return () => {
    alive = false;
    if (pending) {
      const p = pending;
      pending = null;
      cancelSession(p);
    }
  };
}
