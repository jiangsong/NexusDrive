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
			c := Check{Name: fmt.Sprintf("pool/%s/member/%s", name, m.Name), Level: LevelOK, Detail: string(m.State)}
			switch m.State {
			case provider.HealthOut:
				c.Level, c.Detail = LevelFail, "out: down since "+m.DownSince.Format("2006-01-02 15:04")+"; its copies are being rebuilt elsewhere"
				c.Fix = "bring the drive back, or drain and remove it: cloudfs pool drain " + name + " " + m.Name
			case provider.HealthDown:
				c.Level, c.Detail = LevelWarn, "down: "+m.LastError
			case provider.HealthDegraded:
				c.Level, c.Detail = LevelWarn, "recent failures: "+m.LastError
			case provider.HealthDisabled, provider.HealthDraining:
				c.Level, c.Detail = LevelWarn, string(m.State)+" by the operator"
			}
			if m.PendingOps > 0 {
				c.Level = maxLevel(c.Level, LevelWarn)
				c.Detail += fmt.Sprintf("; %d tree operations wait to be replayed on it", m.PendingOps)
			}
			out = append(out, c)
		}
		rep := Check{Name: "pool/" + name + "/replicas", Level: LevelOK, Detail: fmt.Sprintf("%d files at %d replicas", r.Files-r.UnderReplicated, r.Target)}
		switch {
		case r.Unavailable > 0:
			rep.Level, rep.Detail = LevelFail, fmt.Sprintf("%d files have no reachable replica; %d below target", r.Unavailable, r.UnderReplicated)
		case r.UnderReplicated > 0:
			rep.Level, rep.Detail = LevelWarn, fmt.Sprintf("%d files below the target of %d (repair queue %d, %d waiting)", r.UnderReplicated, r.Target, r.Repair.Queued, r.Repair.Blocked)
			rep.Fix = "cloudfs pool repair " + name
		}
		if r.TargetCapped {
			rep.Level = maxLevel(rep.Level, LevelWarn)
			rep.Detail += fmt.Sprintf("; only %d members can hold copies, replicas: %d asks for more", r.Target, r.Replicas)
		}
		out = append(out, rep)
		if d.HoldMaxBytes > 0 && r.HoldsBytes > d.HoldMaxBytes {
			out = append(out, Check{Name: "pool/" + name + "/holds", Level: LevelWarn, Detail: fmt.Sprintf("%d bytes held locally for replication, over the %d budget", r.HoldsBytes, d.HoldMaxBytes)})
		}
		if r.Divergences > 0 {
			out = append(out, Check{Name: "pool/" + name + "/divergences", Level: LevelWarn, Detail: fmt.Sprintf("%d paths need a decision", r.Divergences), Fix: "cloudfs pool divergences"})
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
		c := Check{Name: fmt.Sprintf("pool/%s/marker/%s", name, m.Name), Level: LevelOK, Detail: "marker present"}
		marker, err := pool.ReadMarker(ctx, mp, m.Root)
		switch {
		case err != nil && strings.Contains(err.Error(), "no pool marker"):
			c.Level, c.Detail = LevelWarn, "no .cloudfs-pool.json on the member yet; it is written when the pool starts"
		case err != nil:
			c.Level, c.Detail = LevelWarn, "marker could not be read: "+SanitizeError(err)
		case marker.PoolID != r.PoolID:
			c.Level, c.Detail = LevelFail, fmt.Sprintf("the member carries the marker of another pool (%s, %q)", marker.PoolID, marker.PoolName)
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
