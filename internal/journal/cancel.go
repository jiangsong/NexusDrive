package journal

import (
	"context"
	"database/sql"
	"errors"
)

// Cancellation stops future attempts and retains the payload. It never proves
// that a request already sent to the provider had no effect.
const (
	StateCancelling State = "cancelling"
	StateCancelled  State = "cancelled"
)

var ErrCancelled = errors.New("journal: upload cancelled; retained content and remote result require explicit management")
var ErrCannotCancel = errors.New("journal: completed or deletion-compensation upload cannot be cancelled")

// RequestCancel persists the stop before a worker's context is cancelled.
// Live callers use Uploader.Cancel, which also coordinates worker completion.
// An offline owner may call this directly, before starting any workers.
func (j *Journal) RequestCancel(ctx context.Context, id string) (State, error) {
	var state State
	if !j.Owner() {
		return state, errors.New("journal: cancellation requires queue ownership")
	}
	err := j.tx(ctx, func(tx *sql.Tx) error {
		var tombstone bool
		if err := tx.QueryRow(`SELECT state,tombstone FROM uploads WHERE id=?`, id).Scan(&state, &tombstone); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if tombstone || state == StateDone {
			return ErrCannotCancel
		}
		switch state {
		case StateCancelled, StateCancelling:
			// Even a repeated stop invalidates an in-progress resume check.
		case StatePending, StateDead:
			state = StateCancelled
		case StateUploading:
			state = StateCancelling
		default:
			return ErrCannotCancel
		}
		if _, err := tx.Exec(`INSERT INTO upload_cancellation(upload_id,revision) VALUES(?,1) ON CONFLICT(upload_id) DO UPDATE SET revision=revision+1`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE uploads SET state=?,last_error=? WHERE id=?`, state, ErrCancelled.Error(), id)
		return err
	})
	return state, err
}

// FinishCancel acknowledges worker termination (or absence after restart).
// The payload/session/parts remain for inspection; no remote rollback is claimed.
func (j *Journal) FinishCancel(ctx context.Context, id string) error {
	if !j.Owner() {
		return errors.New("journal: cancellation acknowledgement requires queue ownership")
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE uploads SET state=? WHERE id=? AND state=?`, StateCancelled, id, StateCancelling)
		return err
	})
}

func guardNotCancelled(tx *sql.Tx, id string) error {
	state, err := uploadMutationState(tx, id)
	if err != nil {
		return err
	}
	if state == StatePurging {
		return ErrUploadPurging
	}
	if state == StateCancelled || state == StateCancelling {
		return ErrCancelled
	}
	return nil
}
