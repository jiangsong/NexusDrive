package pool

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"cloudfs/internal/provider"
)

// Report is the pool as the control plane shows it.
type Report struct {
	Name            string
	PoolID          string
	Replicas        int
	MinReplicas     int
	Target          int  // what a file can have right now
	TargetCapped    bool // fewer members than replicas
	Members         []MemberReport
	Files           int
	UnderReplicated int
	Unavailable     int
	Repair          RepairStats
	HoldsBytes      int64
	Divergences     int
	Notices         []string
	Quota           provider.Quota
	QuotaKnown      bool
}

// MemberReport is one member in the report.
type MemberReport struct {
	Name         string
	Root         string
	State        provider.HealthState
	LastOK       time.Time
	LastError    string
	DownSince    time.Time
	LatencyMS    float64
	Weight       float64
	Quota        provider.Quota
	QuotaKnown   bool
	Files        int // replicas held
	PendingOps   int
	NamingDenied int
}

// StatusReport gathers everything the control plane needs in one pass.
func (p *Pool) StatusReport(ctx context.Context) (Report, error) {
	target, capped := p.replicaTarget()
	r := Report{Name: p.name, PoolID: p.ID(ctx), Replicas: p.settings.Replicas, MinReplicas: p.settings.MinReplicas, Target: target, TargetCapped: capped, Notices: p.Notices()}
	pending, err := p.PendingOps(ctx)
	if err != nil {
		return r, err
	}
	for _, m := range p.Status() {
		mr := MemberReport{Name: m.Name, Root: m.Root, State: m.Health.State, LastOK: m.Health.LastOK, LastError: m.Health.LastError, DownSince: m.Health.DownSince, LatencyMS: m.LatencyMS, Weight: m.Weight, PendingOps: pending[m.Name]}
		mem := p.byName[m.Name]
		if q := p.memberQuota(ctx, mem); q.Total > 0 {
			mr.Quota, mr.QuotaKnown = q, true
		}
		_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM replicas WHERE member = ?`, m.Name).Scan(&mr.Files)
		_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM member_naming WHERE member = ?`, m.Name).Scan(&mr.NamingDenied)
		r.Members = append(r.Members, mr)
	}
	_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entries WHERE kind = ? AND conflict_of = ''`, int(provider.KindFile)).Scan(&r.Files)
	if r.UnderReplicated, err = p.underReplicated(ctx); err != nil {
		return r, err
	}
	if r.Unavailable, err = p.unavailableFiles(ctx); err != nil {
		return r, err
	}
	if r.Repair, err = p.RepairStatus(ctx); err != nil {
		return r, err
	}
	if r.HoldsBytes, err = p.HoldsBytes(ctx); err != nil {
		return r, err
	}
	_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM divergences`).Scan(&r.Divergences)
	if q, err := p.Quota(ctx); err == nil {
		r.Quota, r.QuotaKnown = q, true
	}
	return r, nil
}

// memberQuota is the member's space as placement sees it.
func (p *Pool) memberQuota(ctx context.Context, m *member) provider.Quota {
	if m == nil {
		return provider.Quota{}
	}
	p.free(ctx, m)
	m.space.mu.Lock()
	q := m.space.quota
	known := m.space.known
	m.space.mu.Unlock()
	if known && q.Total > 0 {
		return q
	}
	if m.capacity > 0 {
		var placed sql.NullInt64
		_ = p.db.QueryRowContext(ctx, `SELECT SUM(size) FROM replicas WHERE member = ?`, m.name).Scan(&placed)
		return provider.Quota{Total: m.capacity, Used: placed.Int64}
	}
	return provider.Quota{}
}

// unavailableFiles counts files none of whose replicas is on a member that
// can be reached right now.
func (p *Pool) unavailableFiles(ctx context.Context) (int, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT path, ctoken FROM entries WHERE kind = ? AND conflict_of = ''`, int(provider.KindFile))
	if err != nil {
		return 0, fmt.Errorf("pool: %w", err)
	}
	type file struct{ path, ctoken string }
	var files []file
	for rows.Next() {
		var f file
		if err := rows.Scan(&f.path, &f.ctoken); err != nil {
			rows.Close()
			return 0, err
		}
		files = append(files, f)
	}
	rows.Close()
	n := 0
	for _, f := range files {
		if a, err := p.Availability(ctx, f.path); err == nil && a.State == AvailUnavailable {
			n++
		}
	}
	return n, nil
}

// Availability is what a browser shows beside a file.
type Availability struct {
	State  string // full, degraded, unavailable
	Live   int    // replicas on members in service
	Target int
	Reason string
}

const (
	AvailFull        = "full"
	AvailDegraded    = "degraded"
	AvailUnavailable = "unavailable"
)

// Availability reports a path's replica state. A directory is always full:
// it is metadata the index holds.
func (p *Pool) Availability(ctx context.Context, pth string) (Availability, error) {
	pth, err := cleanPath(pth)
	if err != nil {
		return Availability{}, err
	}
	target, _ := p.replicaTarget()
	if pth == "/" {
		return Availability{State: AvailFull, Live: target, Target: target}, nil
	}
	row, ok, err := p.entryAt(ctx, pth)
	if err != nil {
		return Availability{}, err
	}
	if !ok {
		return Availability{}, fmt.Errorf("%w: %s", provider.ErrNotFound, pth)
	}
	if row.kind == provider.KindDir {
		return Availability{State: AvailFull, Live: target, Target: target}, nil
	}
	reps, err := p.replicasOf(ctx, pth, row.ctoken)
	if err != nil {
		return Availability{}, err
	}
	probe := p.probeInterval()
	reachable, inService := 0, 0
	var reason string
	for _, r := range reps {
		m := p.byName[r.member]
		if m == nil {
			continue
		}
		switch st := m.state(); st {
		case provider.HealthOut, provider.HealthDisabled:
			if reason == "" {
				reason = fmt.Sprintf("member %s is %s", m.name, st)
			}
			continue
		}
		inService++
		if m.usable(probe) && m.state() != provider.HealthDown {
			reachable++
		} else if reason == "" {
			reason = fmt.Sprintf("member %s is %s", m.name, m.state())
		}
	}
	a := Availability{Live: inService, Target: target, Reason: reason}
	switch {
	case reachable == 0:
		a.State = AvailUnavailable
		if a.Reason == "" {
			a.Reason = "no replica on a reachable member"
		}
	case inService < target:
		a.State = AvailDegraded
		if a.Reason == "" {
			var queued string
			_ = p.db.QueryRowContext(ctx, `SELECT reason FROM repair_queue WHERE path = ?`, pth).Scan(&queued)
			if queued != "" {
				a.Reason = "repair: " + queued
			} else {
				a.Reason = fmt.Sprintf("%d of %d replicas", inService, target)
			}
		}
	case reachable < inService:
		a.State = AvailDegraded
	default:
		a.State = AvailFull
		a.Reason = ""
	}
	return a, nil
}

// Rebuild drops the index and re-lists the members from the root: the
// members are the truth, and this is how a lost or doubtful index comes
// back. Ids survive, so the VFS keeps its inodes; holds and the op log
// survive, since they are this machine's own work.
func (p *Pool) Rebuild(ctx context.Context) error {
	p.mu.Lock()
	err := p.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM entries`, `DELETE FROM replicas`, `DELETE FROM member_dirs`, `DELETE FROM divergences`, `DELETE FROM repair_queue`} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		return nil
	})
	for _, m := range p.members {
		m.mu.Lock()
		m.dirIDs = map[string]string{}
		m.needsScrub = true
		m.mu.Unlock()
	}
	p.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = p.listDir(ctx, "/")
	if err != nil {
		return err
	}
	_, err = p.ScrubOnce(ctx)
	return err
}
