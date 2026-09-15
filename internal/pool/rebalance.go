package pool

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"cloudfs/internal/provider"
)

// Rebalance moves replicas from the fullest members to the emptiest ones.
// Placement already sends new writes to the member with the most room, so
// a pool drifts back to level on its own — but only for data written
// after the drive was added. A drive added to a pool that already holds a
// terabyte stays empty for as long as nobody writes, and the fullest
// member stays the one that runs out. Rebalance is what makes adding a
// drive change anything about the files that are already there.
//
// Every move is copy, verify, then drop: the file exists on both members
// in between, never on neither. See docs/pool-v2.md §6.6.

// RebalancePlan is what PlanRebalance decided, and what a dry run shows.
type RebalancePlan struct {
	PlanID string
	// Skew is the spread in fill ratio across the members in service,
	// before the plan runs: 0.25 means the fullest is a quarter of its
	// capacity fuller than the emptiest.
	Skew    float64
	Target  float64
	Moves   []RebalanceMove
	Bytes   int64
	Reason  string
	Planned bool
}

// RebalanceMove is one file changing members.
type RebalanceMove struct {
	Path string
	From string
	To   string
	Size int64
}

// RebalanceStats is the queue as status shows it.
type RebalanceStats struct {
	Queued     int
	Done       int
	Failed     int
	BytesMoved int64
	Skew       float64
}

// fill is one member's occupancy, from its own quota when it reports one
// and from the configured capacity minus what the pool placed otherwise.
type fill struct {
	m     *member
	ratio float64
	free  int64
	known bool
}

// fills measures every member that is in service. A member that can say
// nothing about its size cannot be balanced — there is no such thing as
// "half full" without a total — so it is left out rather than guessed at.
func (p *Pool) fills(ctx context.Context) []fill {
	var out []fill
	for _, m := range p.members {
		switch m.state() {
		case provider.HealthOut, provider.HealthDisabled, provider.HealthDraining:
			continue
		}
		q := p.memberQuota(ctx, m)
		if q.Total <= 0 {
			out = append(out, fill{m: m})
			continue
		}
		out = append(out, fill{m: m, ratio: float64(q.Used) / float64(q.Total), free: q.Free(), known: true})
	}
	return out
}

// Skew is the spread between the fullest and the emptiest member in
// service, in fill ratio. It is the number rebalance works against and
// the one status shows.
func (p *Pool) Skew(ctx context.Context) float64 {
	known := 0
	min, max := 1.0, 0.0
	for _, f := range p.fills(ctx) {
		if !f.known {
			continue
		}
		known++
		if f.ratio < min {
			min = f.ratio
		}
		if f.ratio > max {
			max = f.ratio
		}
	}
	if known < 2 {
		return 0
	}
	return max - min
}

// targetSkew is the configured tolerance.
func (p *Pool) targetSkew() float64 {
	if s := p.settings.Rebalance.TargetSkew; s > 0 {
		return s
	}
	return 0.10
}

// PlanRebalance decides which files should move to bring the pool within
// targetSkew, and records the plan in rebalance_queue. A dry run returns
// the same plan without recording it.
//
// It moves roughly half the difference: enough to close the gap, little
// enough that the two members do not trade places and plan each other
// back. Big files go first — the same number of bytes in fewer moves is
// fewer uploads, fewer deletes and less time with a file on two members.
func (p *Pool) PlanRebalance(ctx context.Context, targetSkew float64, dryRun bool) (RebalancePlan, error) {
	if targetSkew <= 0 {
		targetSkew = p.targetSkew()
	}
	plan := RebalancePlan{Target: targetSkew}
	fs := p.fills(ctx)
	var known []fill
	for _, f := range fs {
		if f.known {
			known = append(known, f)
		}
	}
	if len(known) < 2 {
		plan.Reason = "fewer than two members report their size"
		return plan, nil
	}
	sort.Slice(known, func(i, j int) bool { return known[i].ratio > known[j].ratio })
	fullest, emptiest := known[0], known[len(known)-1]
	plan.Skew = fullest.ratio - emptiest.ratio
	if plan.Skew <= targetSkew {
		plan.Reason = fmt.Sprintf("skew %.3f is within the target of %.3f", plan.Skew, targetSkew)
		return plan, nil
	}
	// Half the gap, measured against the smaller of the two members: on
	// drives of different sizes the smaller one decides how many bytes a
	// ratio point is worth.
	smaller := p.capacityOf(ctx, fullest.m)
	if c := p.capacityOf(ctx, emptiest.m); c > 0 && c < smaller {
		smaller = c
	}
	budget := int64((plan.Skew - targetSkew) / 2 * float64(smaller))
	if budget <= 0 {
		plan.Reason = "the gap is smaller than one file"
		return plan, nil
	}
	cands, err := p.movableFrom(ctx, fullest.m, emptiest.m)
	if err != nil {
		return plan, err
	}
	plan.PlanID = strconv.FormatInt(p.now().UnixNano(), 36)
	for _, c := range cands {
		if plan.Bytes >= budget {
			break
		}
		// Reserve space for moves already selected, not just for this file
		// in isolation. Zero is known-full here; only a negative value means
		// capacity is unknown.
		if emptiest.free >= 0 && (plan.Bytes >= emptiest.free || c.size > emptiest.free-plan.Bytes) {
			continue // it would not fit
		}
		plan.Moves = append(plan.Moves, RebalanceMove{Path: c.path, From: fullest.m.name, To: emptiest.m.name, Size: c.size})
		plan.Bytes += c.size
	}
	if len(plan.Moves) == 0 {
		plan.Reason = fmt.Sprintf("no file on %s can move to %s under the pool's rules", fullest.m.name, emptiest.m.name)
		return plan, nil
	}
	if dryRun {
		plan.Reason = "dry run: nothing was queued"
		return plan, nil
	}
	now := p.now().UnixNano()
	err = p.tx(ctx, func(tx *sql.Tx) error {
		for _, mv := range plan.Moves {
			if _, err := tx.Exec(`INSERT INTO rebalance_queue(path, from_member, to_member, size, state, attempts, next_at, last_error, plan_id, created_at)
				VALUES(?, ?, ?, ?, 'pending', 0, 0, '', ?, ?)
				ON CONFLICT(path) DO UPDATE SET from_member = excluded.from_member, to_member = excluded.to_member,
				size = excluded.size, state = 'pending', attempts = 0, next_at = 0, last_error = '', plan_id = excluded.plan_id`,
				mv.Path, mv.From, mv.To, mv.Size, plan.PlanID, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return plan, fmt.Errorf("pool: %w", err)
	}
	plan.Planned = true
	return plan, nil
}

// capacityOf is the member's total size as placement sees it.
func (p *Pool) capacityOf(ctx context.Context, m *member) int64 {
	if q := p.memberQuota(ctx, m); q.Total > 0 {
		return q.Total
	}
	return m.capacity
}

type movable struct {
	path string
	size int64
}

// movableFrom lists the files the fuller member holds that the emptier
// one does not, that the emptier one is allowed to hold: the rules
// (require, avoid) and the failure domain decide, so a rebalance never
// undoes a placement decision. Biggest first.
func (p *Pool) movableFrom(ctx context.Context, from, to *member) ([]movable, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT r.path, r.size FROM replicas r
		JOIN entries e ON e.path = r.path
		WHERE r.member = ? AND r.state = 'live' AND e.kind = ? AND e.conflict_of = ''
		AND NOT EXISTS (SELECT 1 FROM replicas x WHERE x.path = r.path AND x.member = ?)
		ORDER BY r.size DESC`, from.name, int(provider.KindFile), to.name)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	defer rows.Close()
	var out []movable
	for rows.Next() {
		var mv movable
		if err := rows.Scan(&mv.path, &mv.size); err != nil {
			return nil, err
		}
		out = append(out, mv)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	allowed := out[:0]
	for _, mv := range out {
		if p.mayHold(ctx, to, mv.path) {
			allowed = append(allowed, mv)
		}
	}
	return allowed, nil
}

// mayHold reports whether the member is an acceptable home for the path:
// it is one of the candidates placement would consider. Using the same
// function is the point — a rebalance that placed files the rules forbid
// would be undone by the next repair.
func (p *Pool) mayHold(ctx context.Context, m *member, pth string) bool {
	for _, c := range p.candidates(ctx, pth) {
		if c.name == m.name {
			return true
		}
	}
	return false
}

// SetBusy tells the pool how to ask whether the machine is doing
// something a user is waiting for. Rebalance moves whole files around and
// must not compete with a read the user is waiting on.
func (p *Pool) SetBusy(busy func() bool) {
	p.bgMu.Lock()
	p.busy = busy
	p.bgMu.Unlock()
}

func (p *Pool) isBusy() bool {
	p.bgMu.Lock()
	busy := p.busy
	p.bgMu.Unlock()
	return busy != nil && busy()
}

// RebalanceOnce advances the queue by one move: copy to the destination,
// confirm the index agrees, then drop the source copy. It does one at a
// time on purpose — a move is a whole file in both directions, and two at
// once would double what a rebalance costs a member that is already the
// busiest one in the pool.
func (p *Pool) RebalanceOnce(ctx context.Context) (int, error) {
	if p.isBusy() {
		return 0, nil
	}
	now := p.now().UnixNano()
	var (
		pth, from, to string
		size          int64
		attempts      int
	)
	err := p.db.QueryRowContext(ctx, `SELECT path, from_member, to_member, size, attempts FROM rebalance_queue
		WHERE state IN ('pending', 'copied') AND next_at <= ? ORDER BY created_at ASC LIMIT 1`, now).
		Scan(&pth, &from, &to, &size, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("pool: %w", err)
	}
	started := time.Now()
	src, dst := p.byName[from], p.byName[to]
	if src == nil || dst == nil {
		_, _ = p.db.ExecContext(ctx, `UPDATE rebalance_queue SET state = 'failed', last_error = ? WHERE path = ?`, "member is no longer in the pool", pth)
		return 0, nil
	}
	if err := p.moveReplica(ctx, pth, src, dst); err != nil {
		delay := 30 * time.Second << uint(min(attempts, 6))
		state := "pending"
		if attempts+1 >= 5 {
			state = "failed"
		}
		_, _ = p.db.ExecContext(ctx, `UPDATE rebalance_queue SET state = ?, attempts = attempts + 1, next_at = ?, last_error = ? WHERE path = ?`,
			state, p.now().Add(delay).UnixNano(), err.Error(), pth)
		return 0, err
	}
	_, _ = p.db.ExecContext(ctx, `UPDATE rebalance_queue SET state = 'done', last_error = '' WHERE path = ?`, pth)
	p.rebalanceWait(ctx, size, time.Since(started))
	return 1, nil
}

// rebalanceWait holds the next move back long enough to keep the average
// byte rate under rebalance.max_rate, and to leave pause_between between
// moves. Both are about not making the pool's own housekeeping the
// reason a drive is busy: a move nobody is waiting for can afford to
// take its time.
func (p *Pool) rebalanceWait(ctx context.Context, moved int64, took time.Duration) {
	wait := p.settings.Rebalance.PauseBetween
	if rate := int64(p.settings.Rebalance.MaxRate); rate > 0 && moved > 0 {
		owed := time.Duration(float64(moved) / float64(rate) * float64(time.Second))
		if owed > took {
			if rest := owed - took; rest > wait {
				wait = rest
			}
		}
	}
	if wait <= 0 {
		return
	}
	select {
	case <-time.After(wait):
	case <-ctx.Done():
	}
}

// moveReplica copies one file to dst and then drops it from src. The
// order matters and is not negotiable: a crash between the two leaves a
// surplus replica, which trim removes, while the other order would leave
// a file with one copy fewer than the operator asked for.
func (p *Pool) moveReplica(ctx context.Context, pth string, src, dst *member) error {
	row, ok, err := p.entryAt(ctx, pth)
	if err != nil {
		return err
	}
	if !ok || row.kind != provider.KindFile {
		return nil // deleted under us: nothing to move
	}
	live, err := p.liveReplicas(ctx, pth, row.ctoken)
	if err != nil {
		return err
	}
	held := false
	for _, r := range live {
		if r.member == dst.name {
			held = true
		}
	}
	if !held {
		if err := p.copyReplica(ctx, pth, row, live, dst); err != nil {
			return err
		}
	}
	// Verify against the index rather than against what the copy
	// returned: publishGroup may have adopted a different copy in the
	// meantime, and dropping the source on the strength of a stale answer
	// is how a move loses a file.
	after, err := p.liveReplicas(ctx, pth, row.ctoken)
	if err != nil {
		return err
	}
	confirmed := false
	for _, r := range after {
		if r.member == dst.name && r.ctoken == row.ctoken {
			confirmed = true
		}
	}
	if !confirmed {
		return fmt.Errorf("pool: the copy of %s on %s is not the entry's content", pth, dst.name)
	}
	if len(after) < 2 {
		return fmt.Errorf("pool: %s has no second replica to fall back on", pth)
	}
	return p.dropReplica(ctx, pth, src)
}

// dropReplica removes one member's copy of a path and the index row for
// it. A delete that fails is left to TrimOnce: the copy is surplus, not
// wrong, and nothing is lost by it living a little longer.
func (p *Pool) dropReplica(ctx context.Context, pth string, m *member) error {
	var remoteID string
	err := p.db.QueryRowContext(ctx, `SELECT remote_id FROM replicas WHERE path = ? AND member = ?`, pth, m.name).Scan(&remoteID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pool: %w", err)
	}
	err = m.p.Delete(ctx, remoteID)
	if errors.Is(err, provider.ErrNotFound) {
		err = nil
	}
	m.note(err)
	if err != nil {
		return err
	}
	_, err = p.execIndex(ctx, `DELETE FROM replicas WHERE path = ? AND member = ?`, pth, m.name)
	return err
}

// RebalanceStatus reports the queue and the current skew.
func (p *Pool) RebalanceStatus(ctx context.Context) (RebalanceStats, error) {
	var st RebalanceStats
	rows, err := p.db.QueryContext(ctx, `SELECT state, COUNT(*), COALESCE(SUM(size), 0) FROM rebalance_queue GROUP BY state`)
	if err != nil {
		return st, fmt.Errorf("pool: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		var bytes int64
		if err := rows.Scan(&state, &n, &bytes); err != nil {
			return st, err
		}
		switch state {
		case "done":
			st.Done += n
			st.BytesMoved += bytes
		case "failed":
			st.Failed += n
		default:
			st.Queued += n
		}
	}
	st.Skew = p.Skew(ctx)
	return st, rows.Err()
}

// ClearRebalance drops finished rows, so a plan's leftovers do not read
// as the next plan's progress.
func (p *Pool) ClearRebalance(ctx context.Context) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM rebalance_queue WHERE state IN ('done', 'failed')`)
	return err
}

// backfillIfNeeded starts a plan when a member holds nothing while others
// hold something: a drive that was just added. Without it, adding a drive
// only affects files written afterwards.
func (p *Pool) backfillIfNeeded(ctx context.Context) {
	if p.settings.Rebalance.AutoBackfill != nil && !*p.settings.Rebalance.AutoBackfill {
		return
	}
	empty, filled := false, false
	for _, m := range p.members {
		switch m.state() {
		case provider.HealthOut, provider.HealthDisabled, provider.HealthDraining:
			continue
		}
		if p.memberFiles(ctx, m.name) == 0 {
			empty = true
		} else {
			filled = true
		}
	}
	if !empty || !filled {
		return
	}
	if _, err := p.PlanRebalance(ctx, p.targetSkew(), false); err != nil {
		p.addNotice(fmt.Sprintf("backfill could not be planned: %v", err))
	}
}

// SetMemberCapacities gives every member the same total size. It exists
// for tests: a real pool gets capacities from the configuration, one per
// member, and most backends report their own.
func (p *Pool) SetMemberCapacities(bytes int64) {
	for _, m := range p.members {
		m.capacity = bytes
	}
}
