package journal

import (
	"context"
	"fmt"
)

// ListActive pages through unfinished uploads using stable IDs, never offsets.
// Concurrent transitions may remove rows, but cannot shift remaining rows past
// an offset. A new upload whose ID precedes the cursor appears on the next scan.
func (j *Journal) ListActive(ctx context.Context, after string, limit int) ([]Upload, string, error) {
	if limit < 1 || limit > 1000 {
		return nil, "", fmt.Errorf("journal: list limit must be between 1 and 1000")
	}
	rows, err := j.list(ctx, `WHERE state IN (?, ?, ?, ?, ?, ?) AND id > ? ORDER BY id LIMIT ?`,
		string(StatePending), string(StateUploading), string(StateDead), string(StateCancelling), string(StateCancelled), string(StatePurging), after, limit+1)
	if err != nil {
		return nil, "", err
	}
	if len(rows) > limit {
		return rows[:limit], rows[limit-1].ID, nil
	}
	return rows, "", nil
}

// ListCopyJobs includes preparing, failed and submitted jobs. Stable ID cursors
// avoid offset shifts when other jobs are removed; this is not a snapshot of
// jobs created after a scan began. Old read-only journals need no migration.
func (j *Journal) ListCopyJobs(ctx context.Context, after string, limit int) ([]CopyJob, string, error) {
	if limit < 1 || limit > 1000 {
		return nil, "", fmt.Errorf("journal: copy list limit must be between 1 and 1000")
	}
	if after != "" {
		if _, err := j.copyPath(after); err != nil {
			return nil, "", err
		}
	}
	if exists, err := j.hasCopyTable(ctx); err != nil {
		return nil, "", err
	} else if !exists {
		return nil, "", nil
	}
	query, err := j.copyRowsSQL(ctx)
	if err != nil {
		return nil, "", err
	}
	rows, err := j.db.QueryContext(ctx, `SELECT * FROM (`+query+`) WHERE id > ? ORDER BY id LIMIT ?`, after, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []CopyJob
	for rows.Next() {
		job, err := scanCopy(rows)
		if err != nil {
			return nil, "", err
		}
		if _, err := j.copyPath(job.ID); err != nil {
			return nil, "", err
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(out) > limit {
		return out[:limit], out[limit-1].ID, nil
	}
	return out, "", nil
}
