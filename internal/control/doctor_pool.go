package control

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"cloudfs/internal/pool"
	"cloudfs/internal/provider"
)

// checkPools reports the state a person should act on in each running
// pool: members that are out or refusing, files below their target, ops
// waiting for a member, holds past their budget, divergences, and members
// missing the pool's marker.
func (d *Doctor) checkPools(ctx context.Context) []Check {
	if len(d.Pools) == 0 {
		return nil
	}
	names := make([]string, 0, len(d.Pools))
	for name := range d.Pools {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []Check
	for _, name := range names {
		p := d.Pools[name]
		r, err := p.StatusReport(ctx)
		if err != nil {
			out = append(out, Check{Name: "pool/" + name, Level: LevelFail, Detail: err.Error()})
			continue
		}
		for _, m := range r.Members {
			c := Check{Name: fmt.Sprintf("pool/%s/member/%s", name, m.Name), Level: LevelOK}
			c.passDetail(string(m.State))
			switch m.State {
			case provider.HealthOut:
				c.Level = LevelFail
				c.setDetail("doctor.pool.member.out", m.DownSince.Format("2006-01-02 15:04"))
				c.setFix("doctor.pool.member.fix.out", name, m.Name)
			case provider.HealthDown:
				c.Level = LevelWarn
				c.setDetail("doctor.pool.member.down", m.LastError)
			case provider.HealthDegraded:
				c.Level = LevelWarn
				c.setDetail("doctor.pool.member.recent", m.LastError)
			case provider.HealthDisabled, provider.HealthDraining:
				c.Level = LevelWarn
				c.setDetail("doctor.pool.member.operator", string(m.State))
			}
			if m.PendingOps > 0 {
				c.Level = maxLevel(c.Level, LevelWarn)
				c.addDetail("doctor.pool.member.pending", m.PendingOps)
			}
			out = append(out, c)
		}
		rep := Check{Name: "pool/" + name + "/replicas", Level: LevelOK}
		rep.setDetail("doctor.pool.replicas.ok", r.Files-r.UnderReplicated, r.Target)
		switch {
		case r.Unavailable > 0:
			rep.Level = LevelFail
			rep.setDetail("doctor.pool.replicas.lost", r.Unavailable, r.UnderReplicated)
		case r.BelowMin > 0:
			// min_replicas never fails a write, so this line is the only
			// place it means anything: the operator asked to hear when a
			// file is one member away from being lost.
			rep.Level = LevelWarn
			rep.setDetail("doctor.pool.replicas.belowmin", r.BelowMin, r.MinReplicas)
			rep.setFix("doctor.pool.replicas.fix", name)
		case r.UnderReplicated > 0:
			rep.Level = LevelWarn
			rep.setDetail("doctor.pool.replicas.under", r.UnderReplicated, r.Target, r.Repair.Queued, r.Repair.Blocked)
			rep.setFix("doctor.pool.replicas.fix", name)
		}
		if r.TargetCapped {
			rep.Level = maxLevel(rep.Level, LevelWarn)
			rep.addDetail("doctor.pool.replicas.capped", r.Target, r.Replicas)
		}
		out = append(out, rep)
		if d.HoldMaxBytes > 0 && r.HoldsBytes > d.HoldMaxBytes {
			hc := Check{Name: "pool/" + name + "/holds", Level: LevelWarn}
			hc.setDetail("doctor.pool.holds", r.HoldsBytes, d.HoldMaxBytes)
			out = append(out, hc)
		}
		if r.Divergences > 0 {
			dc := Check{Name: "pool/" + name + "/divergences", Level: LevelWarn}
			dc.setDetail("doctor.pool.divergences", r.Divergences)
			dc.setFix("doctor.pool.divergences.fix")
			out = append(out, dc)
		}
		for _, n := range r.Notices {
			out = append(out, Check{Name: "pool/" + name + "/notice", Level: LevelWarn, Detail: n})
		}
		out = append(out, d.checkMarkers(ctx, name, p, r)...)
	}
	return out
}

// checkMarkers verifies each reachable member carries this pool's marker.
func (d *Doctor) checkMarkers(ctx context.Context, name string, p *pool.Pool, r pool.Report) []Check {
	if d.MemberProviders == nil {
		return nil
	}
	var out []Check
	for _, m := range r.Members {
		if m.State == provider.HealthDown || m.State == provider.HealthOut || m.State == provider.HealthDisabled {
			continue
		}
		mp, ok := d.MemberProviders[m.Name]
		if !ok {
			continue
		}
		c := Check{Name: fmt.Sprintf("pool/%s/marker/%s", name, m.Name), Level: LevelOK}
		c.setDetail("doctor.pool.marker.ok")
		marker, err := pool.ReadMarker(ctx, mp, m.Root)
		switch {
		case err != nil && strings.Contains(err.Error(), "no pool marker"):
			c.Level = LevelWarn
			c.setDetail("doctor.pool.marker.absent")
		case err != nil:
			c.Level = LevelWarn
			c.setDetail("doctor.pool.marker.unreadable", SanitizeError(err))
		case marker.PoolID != r.PoolID:
			c.Level = LevelFail
			c.setDetail("doctor.pool.marker.foreign", marker.PoolID, marker.PoolName)
		}
		out = append(out, c)
	}
	return out
}

func maxLevel(a, b Level) Level {
	rank := map[Level]int{LevelOK: 0, LevelWarn: 1, LevelFail: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
