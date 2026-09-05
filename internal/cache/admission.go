package cache

// blockRoom charges only newly resident bytes and one slot only for a new
// block. admitMu keeps another admission from consuming the checked capacity.
// Excluding the replaced block keeps its credit valid while evicting victims.
func (c *Cache) blockRoom(k FileKey, idx, off, n, size int64, partial bool) error {
	id := blockID{k.hash(), idx}
	need, entries := n, 1
	c.mu.Lock()
	if m := c.blocks[id]; m != nil {
		entries = 0
		if !partial {
			if m.flushing && m.mem != nil {
				// Replacing an active flush retires its temporary copy; it
				// cannot supply credit until its writer removes that copy.
				need, entries = n, 1
			} else {
				need = max(0, n-m.size)
			}
		} else if m.partial == nil {
			need = 0
		} else {
			need = 0
			sub := c.subSize()
			_, blockLen := c.BlockRange(idx, size)
			for pos := off; pos < off+n; pos += sub {
				if !m.partial.has(int(pos / sub)) {
					need += min(sub, blockLen-pos)
				}
			}
		}
	}
	c.mu.Unlock()
	if partial && need == 0 && entries == 0 {
		return nil
	}
	return c.makeRoomFor(need, entries, n, &id)
}
