// The choices a connection's "proxy outbound" field offers, computed apart
// from the DOM so they can be checked on their own. The field used to be a
// free-text box: the person had to know the outbound's name from the proxy
// page and type it back without a typo, and an empty box was labeled "direct"
// when it actually means "follow the rules" — for an overseas drive, the
// configured proxy. Listing what the configuration really offers removes both
// the typing and the wrong promise.
//
// The list is, in the order someone reads it:
//
//   1. '' — follow the rules; the label names where the built-in rules send
//      an unruled overseas host (`default` from GET /proxy/config), because
//      that is the question the person is asking;
//   2. 'direct' — always exists, even with no proxy section;
//   3. every group, then every outbound, as the proxy page lists them;
//   4. the value currently saved, when it names nothing above. A stale name
//      is still a name the daemon will refuse at startup, so the select keeps
//      it visible and marked rather than silently snapping to something else.
//
// Each entry is { value, key, args } — the caller renders key through the
// catalog. Nothing here touches the document or the network.
export function proxyChoices(cfg, current = '') {
  const groups = ((cfg && cfg.groups) || []).map((g) => g.name).filter(Boolean);
  const outbounds = ((cfg && cfg.outbounds) || []).map((o) => o.name).filter((n) => n && n !== 'direct');
  const fallback = (cfg && cfg.default) || 'direct';
  const choices = [
    { value: '', key: 'conn.proxy.rules', args: [fallback] },
    { value: 'direct', key: 'conn.proxy.direct', args: [] },
    ...groups.map((n) => ({ value: n, key: 'conn.proxy.group', args: [n] })),
    ...outbounds.map((n) => ({ value: n, key: 'conn.proxy.outbound', args: [n] })),
  ];
  if (current && !choices.some((c) => c.value === current)) {
    choices.push({ value: current, key: 'conn.proxy.missing', args: [current] });
  }
  return choices;
}
