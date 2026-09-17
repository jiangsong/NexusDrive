// The memory tab's layout decisions, with no DOM in them (ui-plan G9):
// how the agent list groups by owner in layout v2, and how a merge
// proposal from GET /memory/merge is broken into rows the overlay draws —
// plain lines, and the conflict blocks between the markers memory_merge
// writes, each as its two sides.

// groupByOwner turns the agents of GET /memory/agents into groups: in
// v2 one per owner (shared last, under ''), in v1 a single unnamed group.
// Groups and agents keep the daemon's order within a group; owners sort
// with the current owner first.
export function groupByOwner(agents, layout, me) {
  const list = Array.isArray(agents) ? agents : [];
  if (layout !== 'v2') return [{ owner: '', agents: list }];
  const by = new Map();
  for (const a of list) {
    const owner = a && typeof a.owner === 'string' ? a.owner : '';
    if (!by.has(owner)) by.set(owner, []);
    by.get(owner).push(a);
  }
  const owners = [...by.keys()].filter((o) => o !== '').sort((a, b) => (a === me ? -1 : b === me ? 1 : a < b ? -1 : a > b ? 1 : 0));
  const out = owners.map((o) => ({ owner: o, agents: by.get(o) }));
  if (by.has('')) out.push({ owner: '', agents: by.get('') });
  return out;
}

// mergeRows splits a merged text into rows: {kind: 'line', text} for a
// line both sides agree on, {kind: 'conflict', ours: [...], theirs: [...]}
// for a block between the markers. An unterminated block is closed at the
// end so nothing is lost.
export function mergeRows(merged) {
  const rows = [];
  let block = null;
  let side = null;
  for (const line of String(merged || '').split('\n')) {
    if (line.startsWith('<<<<<<<')) { block = { kind: 'conflict', ours: [], theirs: [] }; side = 'ours'; continue; }
    if (block && line === '=======') { side = 'theirs'; continue; }
    if (block && line.startsWith('>>>>>>>')) { rows.push(block); block = null; side = null; continue; }
    if (block) block[side].push(line); else rows.push({ kind: 'line', text: line });
  }
  if (block) rows.push(block);
  return rows;
}

// adoptable says a proposal can be stored as it is: no conflict block
// left, and the daemon called it clean.
export function adoptable(res) {
  return !!res && res.clean === true && mergeRows(res.merged).every((r) => r.kind === 'line');
}
