// The settings screen's shape, with no DOM in it, so it runs under node:
// the sections GET /settings answers, each as rows of {key, value} the
// screen labels through t(), and the YAML that would set that section in
// the configuration file. The screen is read-only (ui-plan G3, 2026-09-17):
// configuration is defined in the file and only there, so what the page
// offers is the exact lines to paste, not a form.

// yamlValue renders one scalar or list the way the file accepts it.
export function yamlValue(v) {
  if (Array.isArray(v)) return v.length ? '[' + v.map(yamlValue).join(', ') + ']' : '[]';
  if (v === null || v === undefined || v === '') return '""';
  if (typeof v === 'boolean' || typeof v === 'number') return String(v);
  const s = String(v);
  return /^[A-Za-z0-9_./:-]+$/.test(s) ? s : JSON.stringify(s);
}

// yamlBlock renders nested plain objects as indented YAML under a root key.
export function yamlBlock(root, obj) {
  const lines = [root + ':'];
  const walk = (o, depth) => {
    for (const [k, v] of Object.entries(o)) {
      const pad = '  '.repeat(depth);
      if (v && typeof v === 'object' && !Array.isArray(v)) { lines.push(pad + k + ':'); walk(v, depth + 1); } else lines.push(pad + k + ': ' + yamlValue(v));
    }
  };
  walk(obj, 1);
  return lines.join('\n');
}

// readTextTokens is the budget hint: with a MaxTokens of n, one read_text
// answer carries at most about n tokens (0 means the default of 20,000; a
// negative value switches the budget off).
export function readTextTokens(maxTokens) {
  const n = Number(maxTokens);
  if (!Number.isFinite(n) || n === 0) return 20000;
  return n < 0 ? 0 : n;
}

// sections turns the /settings answer into the screen's sections: id, the
// rows to show (key is a settings.<section>.<field> phrase, value is
// shown as text), and the YAML for that section.
export function sections(v) {
  const s = v || {};
  const mcp = s.mcp || {};
  const session = mcp.session || {};
  const hooks = s.hooks || {};
  const memory = s.memory || {};
  const index = s.index || {};
  const heat = s.heat || {};
  const share = s.share || {};
  const budget = readTextTokens(mcp.max_tokens);
  return [
    {
      id: 'mcp',
      rows: [
        { key: 'settings.mcp.http', value: mcp.http || '' },
        { key: 'settings.mcp.allow', value: (mcp.allow || []).join(', ') },
        { key: 'settings.mcp.read_only', value: !!mcp.read_only },
        { key: 'settings.mcp.workspace', value: mcp.workspace || '' },
        { key: 'settings.mcp.max_tokens', value: mcp.max_tokens ?? 0, hint: budget ? { key: 'settings.mcp.max_tokens.hint', arg: budget } : { key: 'settings.mcp.max_tokens.off' } },
        { key: 'settings.mcp.install_transport', value: mcp.install_transport || 'auto' },
      ],
      yaml: yamlBlock('mcp', { http: mcp.http || '', allow: mcp.allow || [], read_only: !!mcp.read_only, limits: { max_tokens: mcp.max_tokens ?? 0 }, install: { transport: mcp.install_transport || 'auto' } }),
    },
    {
      id: 'session',
      rows: [
        { key: 'settings.session.idle', value: session.idle || '' },
        { key: 'settings.session.retain', value: session.retain || '' },
        { key: 'settings.session.retain_blobs', value: session.retain_blobs || '' },
        { key: 'settings.session.max_preimage_bytes', value: session.max_preimage_bytes || 0, bytes: true },
        { key: 'settings.session.preimage_files', value: session.preimage_files || 0 },
        { key: 'settings.audit.retain', value: mcp.audit_retain || '' },
      ],
      yaml: yamlBlock('mcp', { audit: { retain: mcp.audit_retain || '' }, session: { idle: session.idle || '', retain: session.retain || '', retain_blobs: session.retain_blobs || '', max_preimage_bytes: session.max_preimage_bytes || 0, preimage_files: session.preimage_files || 0 } }),
    },
    {
      id: 'hooks',
      rows: [
        { key: 'settings.hooks.context', value: hooks.context || 'minimal' },
        { key: 'settings.hooks.changed_max', value: hooks.changed_max || 0 },
        { key: 'settings.hooks.memory_head_lines', value: hooks.memory_head_lines || 0 },
      ],
      yaml: yamlBlock('hooks', { context: hooks.context || 'minimal', changed_max: hooks.changed_max || 20, memory_head_lines: hooks.memory_head_lines || 30 }),
    },
    {
      id: 'heat',
      rows: [
        { key: 'settings.heat.enabled', value: !!heat.enabled },
        { key: 'settings.heat.retention_days', value: heat.retention_days || 0 },
      ],
      yaml: yamlBlock('mcp', { heat: { enabled: !!heat.enabled, retention_days: heat.retention_days || 400 } }),
    },
    {
      id: 'memory',
      rows: [
        { key: 'settings.memory.root', value: memory.root || '' },
        { key: 'settings.memory.layout', value: memory.layout || 'v1' },
        { key: 'settings.memory.max_fact_bytes', value: memory.max_fact_bytes || 0, bytes: true },
      ],
      yaml: yamlBlock('memory', { root: memory.root || '', layout: memory.layout || 'v1' }),
    },
    {
      id: 'share',
      rows: [
        { key: 'settings.share.console_links', value: share.console_links !== false },
        { key: 'settings.share.default_expiry', value: share.default_expiry || '' },
        { key: 'settings.share.render.enabled', value: !!(share.render && share.render.enabled) },
        { key: 'settings.share.render.listen', value: (share.render && share.render.listen) || '' },
        { key: 'settings.share.render.token_ttl', value: (share.render && share.render.token_ttl) || '' },
      ],
      yaml: yamlBlock('share', { console_links: share.console_links !== false, default_expiry: share.default_expiry || '168h', render: { enabled: !!(share.render && share.render.enabled), listen: (share.render && share.render.listen) || '0.0.0.0:9102', token_ttl: (share.render && share.render.token_ttl) || '1h' } }),
    },
    {
      id: 'index',
      rows: [
        { key: 'settings.index.enabled', value: !!index.enabled },
        { key: 'settings.index.pinned', value: !!index.pinned },
        { key: 'settings.index.rules', value: index.rules || 0 },
      ],
      yaml: yamlBlock('index', { enabled: !!index.enabled, pinned: !!index.pinned }),
    },
  ];
}
