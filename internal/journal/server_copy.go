package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// A server-side copy is the one operation in this system whose result cannot be
// inferred from its error. The provider is asked to duplicate an object and
// answers over a network: a timeout, a reset connection or a 5xx says nothing
// about whether the object now exists. Retrying blind can leave a second copy;
// giving up can leave a file on the account that nothing local knows about.
//
// So the intent is written down before the request goes out and removed only
// once the result is published locally. What is left behind after a crash or an
// ambiguous failure is a question — "did this destination get created?" — that
// reconciliation answers by looking, instead of a guess.
const serverCopySchema = `CREATE TABLE IF NOT EXISTS server_copies (
 id TEXT PRIMARY KEY,
 spec TEXT NOT NULL,
 target_remote TEXT NOT NULL,
 target_parent TEXT NOT NULL,
 target_name TEXT NOT NULL,
 last_error TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS server_copy_destination
 ON server_copies(target_remote,target_parent,target_name);`

// ErrServerCopyBusy reports that another server-side copy to the same
// destination has not been resolved yet. Issuing a second one would make the
// outstanding question unanswerable: two intents, one possible object, no way
// to tell which produced it.
var ErrServerCopyBusy = errors.New("journal: a server-side copy to this destination is unresolved")

// ServerCopySpec is what reconciliation needs to decide whether an object on
// the account is the one this intent asked for, and whether it still belongs
// where the intent said. It holds identities only — never a signed URL or a
// credential.
type ServerCopySpec struct {
	// MetaIdentity, TargetMount, TargetRootID and TargetAccountBinding fence
	// the intent to the configuration that issued it. A journal carried to a
	// different metadata store or re-pointed at another account must not adopt
	// an object into a tree it was not made for.
	MetaIdentity         string
	TargetMount          string
	TargetRootID         string
	TargetAccountBinding string

	SourcePath    string
	SourceRemote  string
	SourceID      string
	SourceVersion string
	Size          int64

	TargetPath      string
	TargetRemote    string
	TargetParentID  string
	TargetParentIno uint64
	TargetName      string
}

// ServerCopy is an outstanding intent.
type ServerCopy struct {
	ID        string
	Spec      ServerCopySpec
	LastError string
}

func validServerCopySpec(spec ServerCopySpec) error {
	switch {
	case spec.MetaIdentity == "":
		return errors.New("journal: server copy needs the metadata store identity")
	case spec.TargetRemote == "" || spec.TargetParentID == "" || spec.TargetName == "":
		return errors.New("journal: server copy needs a complete destination")
	case spec.SourceRemote == "" || spec.SourceID == "":
		return errors.New("journal: server copy needs a source")
	case spec.TargetParentIno == 0:
		return errors.New("journal: server copy needs the destination parent inode")
	case spec.Size < 0:
		return errors.New("journal: server copy has a negative size")
	case strings.ContainsAny(spec.TargetName, "/\x00"):
		return errors.New("journal: server copy destination name is unsafe")
	}
	return nil
}

// BeginServerCopy records the intent and reserves the destination. The caller
// must issue the provider request only after this returns, and must call
// FinishServerCopy once the result is published locally — or leave the row for
// reconciliation if it cannot tell what happened.
func (j *Journal) BeginServerCopy(ctx context.Context, spec ServerCopySpec) (string, error) {
	if err := validServerCopySpec(spec); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	err = j.tx(ctx, func(tx *sql.Tx) error {
		var held int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM server_copies WHERE target_remote=? AND target_parent=? AND target_name=?`,
			spec.TargetRemote, spec.TargetParentID, spec.TargetName).Scan(&held); err != nil {
			return err
		}
		if held != 0 {
			return ErrServerCopyBusy
		}
		_, err := tx.Exec(`INSERT INTO server_copies(id,spec,target_remote,target_parent,target_name) VALUES(?,?,?,?,?)`,
			id, string(encoded), spec.TargetRemote, spec.TargetParentID, spec.TargetName)
		return err
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// FinishServerCopy drops a resolved intent. It is idempotent: a crash between
// the provider's answer and this call leaves the row for reconciliation, and
// reconciliation calls it again.
func (j *Journal) FinishServerCopy(ctx context.Context, id string) error {
	return j.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM server_copies WHERE id=?`, id)
		return err
	})
}

// NoteServerCopyError records why an intent is still open, so an operator
// reading the queue sees the reason rather than an unexplained row.
func (j *Journal) NoteServerCopyError(ctx context.Context, id, reason string) error {
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	return j.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE server_copies SET last_error=? WHERE id=?`, reason, id)
		return err
	})
}

// ServerCopies lists outstanding intents, oldest first by id so a caller
// processes them in a stable order.
func (j *Journal) ServerCopies(ctx context.Context) ([]ServerCopy, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT id,spec,last_error FROM server_copies ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServerCopy
	for rows.Next() {
		var record ServerCopy
		var encoded string
		if err := rows.Scan(&record.ID, &encoded, &record.LastError); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encoded), &record.Spec); err != nil {
			return nil, fmt.Errorf("journal: unreadable server copy intent %s: %w", record.ID, err)
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// ServerCopyAt returns the outstanding intent for a destination, if any. The
// copy path uses it to resolve a previous attempt before issuing a new one.
func (j *Journal) ServerCopyAt(ctx context.Context, remote, parentID, name string) (ServerCopy, bool, error) {
	var record ServerCopy
	var encoded string
	err := j.db.QueryRowContext(ctx,
		`SELECT id,spec,last_error FROM server_copies WHERE target_remote=? AND target_parent=? AND target_name=?`,
		remote, parentID, name).Scan(&record.ID, &encoded, &record.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return ServerCopy{}, false, nil
	}
	if err != nil {
		return ServerCopy{}, false, err
	}
	if err := json.Unmarshal([]byte(encoded), &record.Spec); err != nil {
		return ServerCopy{}, false, fmt.Errorf("journal: unreadable server copy intent %s: %w", record.ID, err)
	}
	return record, true, nil
}
