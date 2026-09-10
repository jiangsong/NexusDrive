// The decisions the proxy page makes about one outbound, kept apart from the
// DOM so they can be run and read on their own. Nothing here touches the
// document or the network, and it imports nothing: a refusal is returned as a
// catalog key for the caller to render, not as a sentence.
//
// An address that carries a credential is only ever shown redacted. That one
// fact drives all of it: the page cannot send back what it was shown, cannot
// tell whether a redaction still matches what is stored, and so cannot carry a
// credential across a change of name or type — the daemon refuses that on
// purpose, because it is how a password gets moved onto an outbound it was
// never entered for.

// planOutboundEdit decides what one edit means. It returns either
// { outbound } to put in the config, or { error } naming a catalog key, with
// { args } for the values that key formats.
//
// referrers are the remotes that select this outbound with `proxy:`. They live
// outside the section this page writes, so a rename cannot follow them and the
// daemon refuses the whole save (config: remote %q references unknown proxy
// %q). Saying so here costs the person one dialog instead of one failed save
// with a refusal that names nothing on screen.
export function planOutboundEdit(existing, input, referrers = []) {
  const name = (input.name || '').trim();
  const type = input.type;
  const addr = (input.addr || '').trim();
  if (!name) return { error: 'proxy.name.required' };

  if (existing && name !== existing.name && referrers.length) {
    return { error: 'proxy.rename.blocked', args: [referrers.join(', ')] };
  }

  const hadCredential = Boolean(existing && existing.has_credentials);
  if (!hadCredential) return { outbound: { name, type, addr } };

  if (!addr) {
    // The person was shown a redaction, so an empty box cannot be a decision
    // to replace an address they never saw.
    return { error: 'proxy.creds.cleared' };
  }
  if (addr !== (existing.addr || '')) {
    // A freshly typed address replaces the stored one, credential and all.
    // has_credentials is the daemon's to answer on the next read.
    return { outbound: { name, type, addr } };
  }
  if (name !== existing.name || type !== existing.type) {
    // Keeping the stored address under a different name or type is what the
    // daemon refuses (err.proxy_keep_unknown, err.proxy_keep_type). Say so
    // here, where the address box is still open and can be filled in.
    return { error: 'proxy.creds.reenter' };
  }
  // Untouched: ask the daemon to keep what it holds, and keep saying that this
  // outbound carries a credential — dropping the flag is how a second edit
  // before saving ends up sending the redaction back as an address.
  return { outbound: { name, type, addr, keep_credentials: true, has_credentials: true } };
}

// renameOutboundReferences returns a config in which every group member and
// rule target that named oldName names newName instead. A rename that leaves
// them behind is rejected whole by the daemon (config: group %q references
// unknown outbound %q), so the page rewrites them with the rename rather than
// letting the save fail with nothing to act on.
//
// Only a rule's target — its last comma-separated field — is a reference. A
// name appearing inside a matcher is a domain or an address, not this
// outbound.
export function renameOutboundReferences(cfg, oldName, newName) {
  if (!oldName || !newName || oldName === newName) return cfg;
  return {
    ...cfg,
    groups: (cfg.groups || []).map((g) => ({
      ...g,
      members: (g.members || []).map((m) => (m === oldName ? newName : m)),
    })),
    rules: (cfg.rules || []).map((rule) => {
      const parts = rule.split(',');
      if (parts.length < 2 || parts[parts.length - 1].trim() !== oldName) return rule;
      return [...parts.slice(0, -1), newName].join(',');
    }),
  };
}

// outboundReferences lists what would have to follow a rename, so the page can
// tell someone what a rename is about to touch.
export function outboundReferences(cfg, name) {
  const groups = (cfg.groups || []).filter((g) => (g.members || []).includes(name)).map((g) => g.name);
  const rules = (cfg.rules || []).filter((rule) => {
    const parts = rule.split(',');
    return parts.length >= 2 && parts[parts.length - 1].trim() === name;
  });
  return { groups, rules };
}
