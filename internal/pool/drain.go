package pool

import (
	"context"
	"errors"
	"fmt"

	"cloudfs/internal/provider"
)

// Draining a member moves everything it holds to the others before it is
// removed: placement stops using it, its copies stop counting towards the
// target so the scan queues every file it holds, repair copies them away
// (the draining member is still a fine source), and each copy is deleted
// from it once the file is safe elsewhere. When nothing is left the member
// can be dropped from the configuration.

// SetMemberState sets a member's operator state: "enabled", "disabled" or
// "draining".
func (p *Pool) SetMemberState(name, state string) error {
	m := p.byName[name]
	if m == nil {
		return fmt.Errorf("%w: pool %s has no member %q", provider.ErrNotFound, p.name, name)
	}
	switch state {
	case "enabled":
		m.health.SetDisabled(false)
		m.health.SetDraining(false)
	case "disabled":
		m.health.SetDraining(false)
		m.health.SetDisabled(true)
	case "draining":
		m.health.SetDisabled(false)
		m.health.SetDraining(true)
	default:
		return fmt.Errorf("pool: unknown member state %q", state)
	}
	return nil
}

// DrainOnce advances a drain: queues and makes the copies the draining
// members' files still need elsewhere, then removes copies that are safe.
// It reports how many copies were removed and whether every draining
// member is now empty.
func (p *Pool) DrainOnce(ctx context.Context) (removed int, empty bool, err error) {
	var draining []*member
	for _, m := range p.members {
		if m.state() == provider.HealthDraining {
			draining = append(draining, m)
		}
	}
	if len(draining) == 0 {
		return 0, true, nil
	}
	if _, err := p.ScanOnce(ctx); err != nil {
		return 0, false, err
	}
	if _, err := p.RepairOnce(ctx); err != nil && ctx.Err() != nil {
		return 0, false, err
	}
	target, _ := p.replicaTarget()
	empty = true
	for _, m := range draining {
		rows, err := p.db.QueryContext(ctx, `SELECT r.path, r.remote_id, e.ctoken, e.kind FROM replicas r JOIN entries e ON e.path = r.path WHERE r.member = ?`, m.name)
		if err != nil {
			return removed, false, fmt.Errorf("pool: %w", err)
		}
		type copyRow struct {
			path, remoteID, ctoken string
			kind                   int
		}
		var copies []copyRow
		for rows.Next() {
			var c copyRow
			if err := rows.Scan(&c.path, &c.remoteID, &c.ctoken, &c.kind); err != nil {
				rows.Close()
				return removed, false, err
			}
			copies = append(copies, c)
		}
		rows.Close()
		for _, c := range copies {
			live, err := p.liveReplicas(ctx, c.path, c.ctoken)
			if err != nil {
				return removed, false, err
			}
			if len(live) < target {
				empty = false
				continue
			}
			if !m.usable(p.probeInterval()) {
				empty = false
				continue
			}
			err = m.p.Delete(ctx, c.remoteID)
			if errors.Is(err, provider.ErrNotFound) {
				err = nil
			}
			m.note(err)
			if err != nil {
				empty = false
				continue
			}
			_, _ = p.execIndex(ctx, `DELETE FROM replicas WHERE path = ? AND member = ?`, c.path, m.name)
			removed++
		}
		var left int
		_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM replicas WHERE member = ?`, m.name).Scan(&left)
		if left > 0 {
			empty = false
		}
	}
	return removed, empty, nil
}
